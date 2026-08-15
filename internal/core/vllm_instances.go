package core

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/engines/vllm"
)

// This file is the P4.4 core dispatch for vLLM (CONTRACT level). It resolves the model + verdict,
// applies the same Phase-4 safety pre-check, and drives the vLLM Runner (docker/attach). The real
// docker E2E is P4.5; here the runner is exercised against a stub in tests.

// startVLLMInstance starts a vLLM instance. For docker/subprocess it needs a host port (from the
// vLLM port pool) and the model reference (HF repo for docker); for attach it uses the configured
// attach URL. The compat pre-check (runs/runs_degraded + fits) applies to docker/subprocess; attach
// adopts an already-running endpoint and skips the fit check (LocalHub does not allocate its VRAM).
func (s *Service) startVLLMInstance(ctx context.Context, req StartInstanceRequest, reqCtx int) (InstanceView, error) {
	if s.deps.VLLMRunner == nil {
		return InstanceView{}, fmt.Errorf("start instance: vllm runner not configured (is vLLM enabled?)")
	}

	desc, _, derr := s.resolveDescriptor(ctx, CompatRequest{ModelID: req.ModelID, Ctx: reqCtx, Batch: req.Batch})
	if derr != nil {
		return InstanceView{}, fmt.Errorf("resolve model %q: %w", req.ModelID, derr)
	}

	// Four-state residency policy (migration 0015) — see instances.go's identical call site for the
	// full explanation; the same pin/TTL derivation applies uniformly across engines.
	launchState := s.launchStateFor(ctx, req.ModelID)
	ttlSeconds, forcePin := residencyPolicyForLaunchState(launchState, s.deps.Cfg.Residency.DefaultTTLSeconds)

	mode := s.deps.Cfg.Engines.VLLM.Mode
	spec := vllm.StartSpec{
		InstanceID:      uuid.NewString(),
		ModelID:         req.ModelID,
		MaxModelLen:     reqCtx,
		GPUMemUtil:      s.deps.Cfg.Engines.VLLM.DefaultGPUMemoryUtil,
		ExtraArgs:       s.deps.Cfg.Engines.VLLM.ExtraArgs,
		ExtraDockerArgs: s.deps.Cfg.Engines.VLLM.ExtraDockerArgs,
		Priority:        req.Priority,
		Pinned:          req.Pin || forcePin,
		TTLSeconds:      ttlSeconds,
	}

	if mode == "attach" {
		spec.AttachURL = s.deps.Cfg.Engines.VLLM.AttachURL
		if spec.AttachURL == "" {
			return InstanceView{}, fmt.Errorf("start vllm instance: mode=attach but no attach_url configured")
		}
		inst, err := s.deps.VLLMRunner.Start(ctx, spec)
		if err != nil {
			s.publishInstanceUpdated()
			return InstanceView{}, err
		}
		s.publishInstanceUpdated()
		return instanceToView(inst), nil
	}

	// docker/subprocess: needs a model reference + a compat fit pre-check.
	model := desc.Repo
	if model == "" {
		model = desc.LocalPath
	}
	if model == "" {
		return InstanceView{}, fmt.Errorf("start vllm instance: model %q has no repo or local path to pass to --model", req.ModelID)
	}
	spec.Model = model

	snap, herr := s.deps.Hardware.Snapshot(ctx)
	if herr != nil {
		return InstanceView{}, fmt.Errorf("read hardware snapshot: %w", herr)
	}
	avail := s.engineAvailability(ctx)
	verdict := s.deps.Compat.Evaluate(ctx, compat.Request{
		Descriptor: desc, RequestedCtx: reqCtx, Batch: req.Batch,
		Profile: legacyKVProfile(reqCtx, req.Batch, req.KVQuant),
	}, snap, avail)

	// Unified admission (fix 2): the SAME residency gate llama.cpp/Ollama use — route_loaded /
	// load_new / evict_then_load / queue / reject (falling back to the Phase-4 pre-check when
	// residency is off). For vLLM, admission's estimator computes the footprint as
	// gpu_memory_utilization × total VRAM (admitForStart drops the compat estimate for vLLM), which
	// is what vLLM actually pre-allocates. Engine-specific docker/subprocess mechanics below are
	// unchanged. (attach mode returned earlier and is exempt — LocalHub does not allocate its VRAM.)
	warm, _, aerr := s.admitGate(ctx, req, reqCtx, verdict, snap)
	if aerr != nil {
		return InstanceView{}, aerr
	}
	if warm != nil {
		return *warm, nil // already HEALTHY-resident on vLLM.
	}
	if s.deps.Residency != nil {
		defer s.deps.Residency.ReleaseReservation(req.ModelID, req.Engine)
	}
	spec.VRAMEstMiB = verdict.EstimatedVRAMMiB

	// Allocate a vLLM host port. The vLLM port pool shares the same PortPool type over the config
	// vllm.port_range; it is created in main and injected via the store's DB. For the contract
	// path we allocate from a dedicated pool passed through deps.
	if s.deps.VLLMPortPool == nil {
		return InstanceView{}, fmt.Errorf("start vllm instance: vllm port pool not configured")
	}
	port, perr := s.deps.VLLMPortPool.Allocate(ctx, spec.InstanceID)
	if perr != nil {
		return InstanceView{}, fmt.Errorf("allocate vllm port: %w", perr)
	}
	spec.HostPort = port

	inst, err := s.deps.VLLMRunner.Start(ctx, spec)
	if err != nil {
		_ = s.deps.VLLMPortPool.Free(ctx, port)
		s.publishInstanceUpdated()
		return InstanceView{}, err
	}
	s.publishInstanceUpdated()
	return instanceToView(inst), nil
}

// ReconcileVLLM runs the docker-mode vLLM container reconcile at startup (P4.4). Returns the result
// for logging. No-op when the reconciler is not wired.
func (s *Service) ReconcileVLLM(ctx context.Context) (vllm.ReconcileResult, error) {
	if s.deps.VLLMReconciler == nil {
		return vllm.ReconcileResult{}, nil
	}
	res, err := s.deps.VLLMReconciler.Reconcile(ctx)
	if err == nil {
		s.publishInstanceUpdated()
	}
	return res, err
}
