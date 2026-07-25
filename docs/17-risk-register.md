# 17 — Risk Register

| Risk | Severity | Likelihood | Mitigation | Close signal |
|---|---:|---:|---|---|
| Product duplicates GPUStack/Xinference too much | High | Medium | Phase 0 go/no-go; narrow to single-box bundle | User friction log shows clear gap |
| Estimator says `runs` but model OOMs | Critical | Medium | Conservative margins; 20-model validation; false-positive block gate | Zero false-positive runs in gate set |
| vLLM version/CUDA churn breaks adapter | High | High | Version pinning policy; Docker option; source revalidation | Adapter matrix green for pinned versions |
| Ollama unload timing is asynchronous | Medium | High | Poll `/api/ps`; verify VRAM delta | Stop gate verifies actual VRAM reclaimed |
| Cross-engine scheduler scope creep | Critical | Medium | ADR-001 + unwaivable gate | No code implementing memory fair-share/preemption |
| Model artifact RCE via pickle/trust_remote_code | High | Medium | Safetensors preference, warnings, strict mode, non-root | Warning displayed and stored in manifest |
| Public exposure of admin controls | Critical | Low | localhost default, Tailscale Serve, token option | Exposure test passes |
| Tailscale requirement hurts adoption | Medium | Medium | Tailscale recommended, not required | LAN mode supported with warning |
| Disk bloat from multiple formats | Medium | Medium | No auto conversion; disk accounting | No implicit duplicate artifacts |
| Phase 6 becomes LiteLLM clone | High | Medium | Thin proxy only; integration docs for LiteLLM | No provider budgets/fallback/key management |
