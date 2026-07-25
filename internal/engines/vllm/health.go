// This file plugs a vLLM HealthChecker into the existing internal/instances.Reconciler
// (P4.4). At daemon-startup reconcile a docker-mode vLLM instance is checked two ways:
//   - the container is still running (`docker ps --filter name=<name>` returns the name), AND
//   - GET {base_url}/health answers 200.
//
// Both true → re-attach as HEALTHY. Container gone → the reconciler marks it STOPPED (PID 0 path).
//
// The instances.Reconciler process-reconciles only rows with a PID (llama.cpp); vLLM docker rows
// have PID 0, so the base Reconcile() SKIPS them. This checker is used by a vLLM-specific reconcile
// pass (ReconcileDocker) that mirrors the Reconciler's outcomes for containers. It is tested with a
// fake DockerClient + fake health endpoint (no real docker).
package vllm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// ReconcileStore is the subset of *instances.Store the docker reconcile needs.
type ReconcileStore interface {
	List(ctx context.Context, activeOnly bool) ([]instances.Instance, error)
	Transition(ctx context.Context, id string, p instances.TransitionParams) (instances.Instance, error)
}

// DockerReconciler reconciles docker-mode vLLM instances at startup: container alive + healthy →
// re-attach; container gone → STOPPED. Construct with NewDockerReconciler.
type DockerReconciler struct {
	store  ReconcileStore
	docker DockerClient
	http   *http.Client
}

// NewDockerReconciler builds a reconciler over the instance store + docker client.
func NewDockerReconciler(store ReconcileStore, docker DockerClient) *DockerReconciler {
	return &DockerReconciler{store: store, docker: docker, http: &http.Client{Timeout: 4 * time.Second}}
}

// ReconcileResult summarizes a vLLM docker reconcile pass.
type ReconcileResult struct {
	Reattached []string `json:"reattached"`
	Stopped    []string `json:"stopped"`
	Errors     []string `json:"errors"`
}

// Reconcile runs one pass over active vLLM instances. Only docker-mode rows (BinaryPath = an
// image, Port > 0) are handled; attach rows are left alone (LocalHub does not own them).
func (r *DockerReconciler) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult
	active, err := r.store.List(ctx, true)
	if err != nil {
		return res, fmt.Errorf("list active instances for vllm reconcile: %w", err)
	}
	for _, inst := range active {
		if inst.Engine != instances.EngineVLLM || inst.Port == 0 {
			continue // not a docker-mode vLLM instance
		}
		r.reconcileOne(ctx, inst, &res)
	}
	return res, nil
}

func (r *DockerReconciler) reconcileOne(ctx context.Context, inst instances.Instance, res *ReconcileResult) {
	// Container still running?
	out, err := r.docker.Run(ctx, DockerPsNameArgs(inst.ID)...)
	containerUp := err == nil && strings.TrimSpace(out) != ""
	if !containerUp {
		if _, terr := r.transition(ctx, inst, instances.StateStopped, "vLLM container gone at reconcile (not listed by `docker ps`); marking STOPPED"); terr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: mark stopped: %v", inst.ID, terr))
			return
		}
		res.Stopped = append(res.Stopped, inst.ID)
		return
	}
	// Container up → health check.
	if r.health(ctx, inst.BaseURL) {
		if inst.State != instances.StateHealthy {
			if _, terr := r.transition(ctx, inst, instances.StateHealthy, "reattached at reconcile: vLLM container running and /health OK"); terr != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: reattach: %v", inst.ID, terr))
				return
			}
		}
		res.Reattached = append(res.Reattached, inst.ID)
		return
	}
	// Container up but unhealthy → leave as-is (still STARTING) unless it was HEALTHY; a flap to
	// FAILED here would be premature during a slow cold start. Record a note.
	res.Errors = append(res.Errors, fmt.Sprintf("%s: container running but /health not answering yet (cold start?)", inst.ID))
}

func (r *DockerReconciler) health(ctx context.Context, baseURL string) bool {
	if baseURL == "" {
		return false
	}
	hctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (r *DockerReconciler) transition(ctx context.Context, inst instances.Instance, to instances.State, reason string) (instances.Instance, error) {
	if !instances.CanTransition(inst.State, to) {
		if inst.State.Terminal() {
			return inst, nil
		}
		return inst, fmt.Errorf("illegal transition %s→%s", inst.State, to)
	}
	return r.store.Transition(ctx, inst.ID, instances.TransitionParams{To: to, Reason: reason})
}
