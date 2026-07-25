package manifest

import "fmt"

// This file holds the download SAFEGUARDS (PLAN.md "Hardware safeguards — Downloads"):
// K: has only ~24 GB free; new downloads must not fill the disk or exceed the phase budget.
// These are conservative refusals with readable reasons (CLAUDE.md), checked BEFORE any bytes
// are fetched, using the total size computed from HF metadata.
//
// Two independent caps:
//   - HardCapBytes: an absolute ceiling on a single download's total size. PLAN.md caps NEW
//     downloads and the task brief HARD-refuses anything > 8 GB total on this box.
//   - Disk headroom: refuse if the download would leave less than DiskHeadroomBytes free
//     (size must be <= free_disk - headroom). Protects the box from a full-disk wedge.

const (
	// HardCapBytes is the absolute per-download size ceiling (8 GiB). A download whose total
	// exceeds this is refused outright regardless of free disk.
	HardCapBytes int64 = 8 << 30
	// DiskHeadroomBytes is the free space that must remain AFTER a download (5 GiB). A
	// download that would eat into this headroom is refused.
	DiskHeadroomBytes int64 = 5 << 30
)

// GuardConfig parameterizes the guards so tests (and future config) can tune them without
// changing the constants. Zero fields fall back to the constants above.
type GuardConfig struct {
	HardCapBytes      int64
	DiskHeadroomBytes int64
	// FreeDiskBytes is the current free space on the target volume (from the hardware probe /
	// os stat). 0 means "unknown" → the disk-headroom check is skipped (the hard cap still
	// applies) with a note; the caller should supply a real value.
	FreeDiskBytes int64
}

func (g GuardConfig) hardCap() int64 {
	if g.HardCapBytes > 0 {
		return g.HardCapBytes
	}
	return HardCapBytes
}

func (g GuardConfig) headroom() int64 {
	if g.DiskHeadroomBytes > 0 {
		return g.DiskHeadroomBytes
	}
	return DiskHeadroomBytes
}

// CheckDownloadSize refuses a download whose total sizeBytes violates either cap. It returns
// nil to proceed, or an error with a human-readable reason. sizeBytes of 0 (unknown size) is
// allowed to proceed (the download engine still streams and could be cancelled), but the
// caller should prefer a known size from metadata.
func (g GuardConfig) CheckDownloadSize(sizeBytes int64) error {
	if sizeBytes > g.hardCap() {
		return fmt.Errorf(
			"refusing download of %s: exceeds the %s hard cap for this box (PLAN.md safeguard — disk is nearly full)",
			humanBytes(sizeBytes), humanBytes(g.hardCap()))
	}
	if g.FreeDiskBytes > 0 {
		remaining := g.FreeDiskBytes - sizeBytes
		if remaining < g.headroom() {
			return fmt.Errorf(
				"refusing download of %s: would leave only %s free (below the %s headroom); free disk is %s",
				humanBytes(sizeBytes), humanBytes(remaining), humanBytes(g.headroom()), humanBytes(g.FreeDiskBytes))
		}
	}
	return nil
}

// humanBytes renders a byte count in GiB/MiB for readable reasons.
func humanBytes(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
