// This file implements the P4.3 INSTANCE MIRRORING for Ollama. Ollama models are not spawned by
// LocalHub — their residency lives in the shared daemon. So instead of owning a process, the
// Ollama adapter MIRRORS the daemon's resident set (/api/ps) into `instances` rows so the
// dashboard, residency layer, and reconciler see Ollama models the same way they see spawned
// llama.cpp/vLLM instances (engine=ollama, pid=0, port=0, base_url=the daemon).
//
// Mirror rules (engine-adapters.md §5, spec-digest §1 "Ollama models: state lives in Ollama
// daemon; mirror via /api/ps"):
//   - A model present in /api/ps but with no active instance row  → create a REQUESTED→STARTING→
//     HEALTHY row (it appeared; we adopt it as HEALTHY, recording size_vram as the observed VRAM
//     and expires_at as the idle TTL anchor).
//   - A model whose LocalHub-warmed row is STARTING and now appears in /api/ps → advance to
//     HEALTHY (the warm succeeded).
//   - A HEALTHY Ollama row whose model is no longer in /api/ps → mark STOPPED ("unloaded by the
//     Ollama daemon"): the daemon evicted it (idle TTL) or someone else unloaded it.
//
// Never kill the daemon; never spawn one. The mirror only READS /api/ps and WRITES instance rows.
package ollama

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/localhub/localhub/internal/instances"
)

// bytesPerMiB converts /api/ps size_vram (bytes) to the MiB the instances table stores.
const bytesPerMiB = 1024 * 1024

// InstanceSyncStore is the subset of *instances.Store the mirror needs. An interface so the sync
// is unit-testable with a fake and so this package does not hard-depend on the concrete store's
// full surface.
type InstanceSyncStore interface {
	List(ctx context.Context, activeOnly bool) ([]instances.Instance, error)
	Create(ctx context.Context, p instances.CreateParams) (instances.Instance, error)
	Transition(ctx context.Context, id string, p instances.TransitionParams) (instances.Instance, error)
}

// MirrorResult summarizes one sync pass (for logging + the P4.3 gate evidence).
type MirrorResult struct {
	Added    []string `json:"added"`    // instance IDs newly created for a resident model
	Promoted []string `json:"promoted"` // instance IDs advanced STARTING→HEALTHY (warm confirmed)
	Stopped  []string `json:"stopped"`  // instance IDs marked STOPPED (model left /api/ps)
	Resident []string `json:"resident"` // model names currently resident (for the log)
	Errors   []string `json:"errors"`   // non-fatal per-model errors
}

// ModelIDResolver maps an Ollama model tag from /api/ps (e.g. "mistral:latest") to the LocalHub
// catalog model id (e.g. "ollama-mistral-latest") that the instances.model_id FK references. The
// core supplies this by looking up the inventory (which keys Ollama rows as "ollama-<slug>"): a
// resident model with no inventory row (ok=false) is SKIPPED — we cannot create an instance whose
// model_id would violate the models(id) foreign key. Keeping this mapping out of the ollama
// package avoids duplicating the inventory slug logic and guarantees FK correctness.
type ModelIDResolver func(ctx context.Context, ollamaTag string) (modelID string, ok bool)

// Mirror maps the Ollama daemon's /api/ps resident set into instance rows. Construct with
// NewMirror. It holds no mutable state beyond its collaborators; safe for periodic invocation.
type Mirror struct {
	client  *Client
	store   InstanceSyncStore
	resolve ModelIDResolver
}

// NewMirror builds a Mirror over an Ollama client, the instance store, and a resolver that maps
// an Ollama tag to a catalog model id (FK-safe). A nil resolver falls back to using the raw tag
// as the model id (only safe when the caller guarantees a matching models row exists — e.g. in
// tests that seed the tag directly).
func NewMirror(client *Client, store InstanceSyncStore, resolve ModelIDResolver) *Mirror {
	if resolve == nil {
		resolve = func(_ context.Context, tag string) (string, bool) { return tag, true }
	}
	return &Mirror{client: client, store: store, resolve: resolve}
}

// Sync runs one mirror pass. It is safe to call at reconcile time (daemon startup) and on a
// periodic interval (config). It NEVER touches the daemon beyond a read-only GET /api/ps, and it
// NEVER creates/kills a process. If the daemon is unreachable, it returns the transport error and
// makes NO changes (so a transient outage does not mark resident models STOPPED).
func (m *Mirror) Sync(ctx context.Context) (MirrorResult, error) {
	var res MirrorResult

	ps, err := m.client.listRunning(ctx)
	if err != nil {
		// Daemon unreachable: do nothing (never flap Ollama rows on a transient outage).
		return res, &DaemonUnreachableError{BaseURL: m.client.BaseURL, Op: "mirror", Err: err}
	}
	// Resident /api/ps entries keyed by the RESOLVED catalog model_id. A resident model with no
	// inventory row is skipped (recorded in Resident with an "(unmapped)" note) so we never try to
	// create an instance whose model_id would break the FK.
	residentByModel := map[string]PSModel{}
	for _, p := range ps {
		tag := p.Model
		if tag == "" {
			tag = p.Name
		}
		modelID, ok := m.resolve(ctx, tag)
		if !ok {
			res.Resident = append(res.Resident, tag+" (unmapped — no inventory row; not mirrored)")
			continue
		}
		residentByModel[modelID] = p
		res.Resident = append(res.Resident, tag+" → "+modelID)
	}

	// Current active Ollama instance rows, keyed by model_id.
	active, err := m.store.List(ctx, true)
	if err != nil {
		return res, fmt.Errorf("list active instances for ollama mirror: %w", err)
	}
	rowByModel := map[string]instances.Instance{}
	for _, inst := range active {
		if inst.Engine != instances.EngineOllama {
			continue
		}
		rowByModel[inst.ModelID] = inst
	}

	// 1. Resident models: adopt/promote. `model` here is the catalog model_id.
	for model, p := range residentByModel {
		vramMiB := int(p.SizeVRAM / bytesPerMiB)
		row, ok := rowByModel[model]
		if !ok {
			// No active row → create REQUESTED→STARTING→HEALTHY (adopt as HEALTHY).
			if id, cerr := m.adopt(ctx, model, p, vramMiB); cerr != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: adopt: %v", model, cerr))
			} else {
				res.Added = append(res.Added, id)
			}
			continue
		}
		// Row exists. If it is STARTING (a LocalHub warm in flight), promote to HEALTHY now that
		// the model appears in /api/ps. If already HEALTHY, refresh the observed VRAM only.
		if row.State == instances.StateStarting {
			if _, terr := m.store.Transition(ctx, row.ID, instances.TransitionParams{
				To:         instances.StateHealthy,
				Reason:     fmt.Sprintf("warm confirmed: model %q now resident in /api/ps (size_vram %d MiB, expires_at %s)", model, vramMiB, p.ExpiresAt),
				VRAMObsMiB: &vramMiB,
			}); terr != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: promote: %v", model, terr))
			} else {
				res.Promoted = append(res.Promoted, row.ID)
			}
		}
	}

	// 2. Active Ollama rows whose model is NO LONGER resident → STOPPED (daemon unloaded it).
	for model, row := range rowByModel {
		if _, still := residentByModel[model]; still {
			continue
		}
		// A STARTING row that has not yet appeared is left alone (warm may still be loading); only
		// HEALTHY/DRAINING rows that vanished are marked STOPPED.
		if row.State != instances.StateHealthy && row.State != instances.StateDraining {
			continue
		}
		if _, terr := m.store.Transition(ctx, row.ID, instances.TransitionParams{
			To:     instances.StateStopped,
			Reason: fmt.Sprintf("model %q no longer resident in /api/ps — unloaded by the Ollama daemon (idle TTL or external unload); VRAM released by the daemon", model),
		}); terr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: mark stopped: %v", model, terr))
		} else {
			res.Stopped = append(res.Stopped, row.ID)
		}
	}

	return res, nil
}

// adopt creates a REQUESTED→STARTING→HEALTHY instance row for a model that appeared in /api/ps
// without a LocalHub-initiated row (e.g. warmed by `ollama run` directly, or resident before the
// daemon started). It records size_vram as the observed VRAM. Returns the new instance ID.
func (m *Mirror) adopt(ctx context.Context, model string, p PSModel, vramMiB int) (string, error) {
	id := uuid.NewString()
	if _, err := m.store.Create(ctx, instances.CreateParams{
		ID:      id,
		ModelID: model,
		Engine:  instances.EngineOllama,
		Reason:  fmt.Sprintf("mirrored from Ollama /api/ps: model %q resident (size_vram %d MiB)", model, vramMiB),
	}); err != nil {
		return "", err
	}
	base := m.client.BaseURL
	zero := 0
	if _, err := m.store.Transition(ctx, id, instances.TransitionParams{
		To:      instances.StateStarting,
		Reason:  "adopting resident Ollama model (mirror)",
		PID:     &zero, // Ollama instances have no owned PID
		Port:    &zero, // and no owned port (shared daemon)
		BaseURL: &base,
	}); err != nil {
		return "", err
	}
	if _, err := m.store.Transition(ctx, id, instances.TransitionParams{
		To:         instances.StateHealthy,
		Reason:     fmt.Sprintf("resident in Ollama /api/ps (size_vram %d MiB, expires_at %s)", vramMiB, p.ExpiresAt),
		VRAMObsMiB: &vramMiB,
	}); err != nil {
		return "", err
	}
	return id, nil
}

// StartPeriodic runs Sync every interval until ctx is cancelled. Best-effort: a per-pass error
// (including a daemon outage) is passed to onErr (if non-nil) but does not stop the loop. Returns
// when ctx is done. Callers run this in a goroutine from the daemon wiring.
func (m *Mirror) StartPeriodic(ctx context.Context, interval time.Duration, onResult func(MirrorResult), onErr func(error)) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			res, err := m.Sync(ctx)
			if err != nil && onErr != nil {
				onErr(err)
			} else if err == nil && onResult != nil {
				onResult(res)
			}
		}
	}
}
