//go:build windows

package hardware

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// winSystemProbe reads CPU/RAM/disk on Windows using x/sys/windows syscalls and the registry.
// No cgo, no WMI process spawns for the common fields (WMI-free per the task): RAM via
// GlobalMemoryStatusEx, disk via GetDiskFreeSpaceEx, CPU model from the registry, physical
// cores via GetLogicalProcessorInformationEx.
type winSystemProbe struct{}

func newSystemProbe() SystemProbe { return winSystemProbe{} }

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX struct. GlobalMemoryStatusEx is not wrapped
// by x/sys/windows, so we declare the struct and call it via a lazy DLL proc.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// RAM reports total/free/used system memory in MiB via GlobalMemoryStatusEx.
func (winSystemProbe) RAM(ctx context.Context) (RAM, error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r1, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r1 == 0 {
		return RAM{}, err
	}
	total := int(m.TotalPhys / (1024 * 1024))
	free := int(m.AvailPhys / (1024 * 1024))
	return RAM{TotalMiB: total, FreeMiB: free, UsedMiB: total - free}, nil
}

// Disks reports free/total space for each requested path's volume via GetDiskFreeSpaceEx.
// With no paths, it probes the current working directory's volume as a sensible default.
// Duplicate volumes (multiple model dirs on the same drive) are de-duplicated by drive root.
func (winSystemProbe) Disks(ctx context.Context, paths []string) ([]Disk, error) {
	if len(paths) == 0 {
		if wd, err := filepath.Abs("."); err == nil {
			paths = []string{wd}
		} else {
			paths = []string{`C:\`}
		}
	}
	seen := map[string]bool{}
	var disks []Disk
	for _, p := range paths {
		root := volumeRoot(p)
		if root == "" || seen[strings.ToUpper(root)] {
			continue
		}
		seen[strings.ToUpper(root)] = true

		u16, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		var freeToCaller, total, totalFree uint64
		if err := windows.GetDiskFreeSpaceEx(u16, &freeToCaller, &total, &totalFree); err != nil {
			// Volume unreachable/offline: skip rather than fail the whole snapshot.
			continue
		}
		const gib = 1024 * 1024 * 1024
		totalGiB := float64(total) / gib
		freeGiB := float64(freeToCaller) / gib
		disks = append(disks, Disk{
			Path:     root,
			TotalGiB: round2(totalGiB),
			FreeGiB:  round2(freeGiB),
			UsedGiB:  round2(totalGiB - freeGiB),
		})
	}
	return disks, nil
}

// volumeRoot returns the drive root (e.g. "K:\") for a path. It tolerates a bare drive
// letter, a directory, or a UNC-less absolute path.
func volumeRoot(p string) string {
	vn := filepath.VolumeName(p)
	if vn == "" {
		// Relative or rootless path: resolve against cwd.
		if abs, err := filepath.Abs(p); err == nil {
			vn = filepath.VolumeName(abs)
		}
	}
	if vn == "" {
		return ""
	}
	return vn + `\`
}

// CPU reads the processor model from the registry and core/thread counts. Threads =
// runtime.NumCPU() (logical processors visible to the process). Physical cores come from
// GetLogicalProcessorInformationEx(RelationProcessorCore); on failure we fall back to
// threads. UtilizationPct is -1: measuring it needs perf counters/sampling, deferred to v2
// (documented in wiring-p12.md).
func (winSystemProbe) CPU(ctx context.Context) (CPU, error) {
	c := CPU{Threads: runtime.NumCPU(), UtilizationPct: -1}
	c.Model = cpuModelFromRegistry()
	if cores := physicalCoreCount(); cores > 0 {
		c.Cores = cores
	} else {
		c.Cores = c.Threads
	}
	return c, nil
}

func cpuModelFromRegistry() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	name, _, err := k.GetStringValue("ProcessorNameString")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// processorRelationship is the header of each SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX record.
type logicalProcessorInfoExHeader struct {
	Relationship uint32
	Size         uint32
}

const relationProcessorCore = 0 // RelationProcessorCore

// physicalCoreCount counts SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX records of type
// RelationProcessorCore. Each such record is exactly one physical core. Returns 0 on any
// error (caller falls back to logical count).
func physicalCoreCount() int {
	proc := modkernel32.NewProc("GetLogicalProcessorInformationEx")
	if err := proc.Find(); err != nil {
		return 0
	}
	// First call with a nil buffer to learn the required length.
	var length uint32
	r1, _, _ := proc.Call(uintptr(relationProcessorCore), 0, uintptr(unsafe.Pointer(&length)))
	if length == 0 {
		return 0
	}
	buf := make([]byte, length)
	r1, _, _ = proc.Call(
		uintptr(relationProcessorCore),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&length)),
	)
	if r1 == 0 {
		return 0
	}
	count := 0
	offset := uint32(0)
	for offset < length {
		hdr := (*logicalProcessorInfoExHeader)(unsafe.Pointer(&buf[offset]))
		if hdr.Size == 0 {
			break
		}
		if hdr.Relationship == relationProcessorCore {
			count++
		}
		offset += hdr.Size
	}
	return count
}

// hideWindow sets CREATE_NO_WINDOW so spawning nvidia-smi does not flash a console window
// (PLAN.md conflict-resolution 5 Windows lessons).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
