// Package hardware probes the host's GPU, CPU, RAM, and disk and exposes a single
// Snapshot type consumed by the dashboard (GET /api/hardware) and, later, the
// compatibility engine (which needs CURRENT free VRAM, not just total).
//
// PLAN.md decision 5 / spec-digest §13 conflict 3: the probe is an INTERFACE with two
// implementations. On the Windows reference box it shells out to `nvidia-smi` (control
// plane must NOT link CUDA/NVML via cgo — ADR-005 spirit). A native NVML implementation
// can be added later on Linux behind the same interface. Never link go-nvml on the
// Windows box.
//
// Field naming: Go field names are descriptive (VRAMTotalMiB, ...); JSON tags match the
// keys in spec-digest §3 hardware endpoint shape
// `{cpu, ram, disk, gpus:[{index,name,vram_total,vram_free,driver,cuda,cc,temp,power,util}]}`
// so the wire format the UI consumes is exactly what the spec specifies. Where the spec is
// silent (e.g. vram_used, per-field units), we add fields with descriptive JSON keys.
package hardware

import (
	"context"
	"time"
)

// Probe returns a point-in-time hardware Snapshot. Implementations must be safe for
// concurrent use (the metrics loop and request handlers both call Snapshot). Snapshot must
// NOT fail merely because the GPU probe is unavailable: a box with no nvidia-smi still has
// a valid CPU/RAM/disk snapshot and simply reports zero GPUs (see NvidiaSmiProbe). An error
// is reserved for a genuine failure to read system info (rare) or a cancelled context.
type Probe interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// Snapshot is the full hardware picture at CapturedAt. It is the payload behind
// GET /api/hardware and each SSE frame of GET /api/hardware/live.
type Snapshot struct {
	// GPUs is empty (not nil-erroring) when no NVIDIA GPU / no nvidia-smi is present.
	GPUs []GPU `json:"gpus"`
	CPU  CPU   `json:"cpu"`
	RAM  RAM   `json:"ram"`
	// Disks covers the configured model directories' volumes (and/or defaults). The spec
	// §3 key is "disk"; we expose a list because a box may keep models on more than one
	// volume.
	Disks []Disk `json:"disk"`
	// CapturedAt is when this snapshot was taken. Lets the SSE/dashboard show staleness and
	// lets callers cache. RFC3339.
	CapturedAt time.Time `json:"captured_at"`
}

// GPU is one NVIDIA device. JSON keys align with spec-digest §3
// (index,name,vram_total,vram_free,driver,cuda,cc,temp,power,util). VRAM values are MiB
// (nvidia-smi --nounits reports MiB for memory.*), matching the spec's vram_total_mb usage
// elsewhere. Fields nvidia-smi could not supply are the zero value; UtilizationPct may be
// -1 if the field was absent from the query output.
type GPU struct {
	Index             int     `json:"index"`
	Name              string  `json:"name"`
	VRAMTotalMiB      int     `json:"vram_total"`
	VRAMFreeMiB       int     `json:"vram_free"`
	VRAMUsedMiB       int     `json:"vram_used"`
	DriverVersion     string  `json:"driver"`
	ComputeCapability string  `json:"cc"`   // e.g. "8.9" (compute_cap query field)
	UtilizationPct    float64 `json:"util"` // 0..100; -1 if unavailable
	TemperatureC      float64 `json:"temp"` // Celsius; -1 if unavailable
	PowerDrawW        float64 `json:"power"`
	// CUDAVersion is the CUDA runtime version nvidia-smi reports at the driver level. The
	// per-GPU CSV query does not expose it (it is a header field), so it is populated
	// best-effort and may be empty. Spec key: "cuda".
	CUDAVersion string `json:"cuda,omitempty"`
}

// CPU describes the processor. UtilizationPct is -1 when not measured: computing CPU
// utilization on Windows needs performance counters / sampling over an interval, which the
// v1 probe deliberately omits (spec-digest §9 lists CPU util as a metric, but the mandatory
// hardware fields are model/cores/threads; util is best-effort). See wiring-p12.md.
type CPU struct {
	Model          string  `json:"model"`
	Cores          int     `json:"cores"`   // physical cores
	Threads        int     `json:"threads"` // logical processors
	UtilizationPct float64 `json:"util"`    // 0..100; -1 if not measured
}

// RAM is system memory in MiB.
type RAM struct {
	TotalMiB int `json:"total"`
	FreeMiB  int `json:"free"`
	UsedMiB  int `json:"used"`
}

// Disk is free/total space for one volume, keyed by the path probed.
type Disk struct {
	Path     string  `json:"path"`
	TotalGiB float64 `json:"total_gib"`
	FreeGiB  float64 `json:"free_gib"`
	UsedGiB  float64 `json:"used_gib"`
}
