//go:build windows

package main

import (
	"encoding/binary"
	"testing"
)

// The embedded icon is shared with the Start Menu shortcut, so a malformed or missing file would
// silently regress both the tray icon and the shortcut back to a generic glyph.
func TestLocalHubIconIsMultiResolutionICO(t *testing.T) {
	icon := localHubIcon()
	if len(icon) < 22 {
		t.Fatalf("icon is too short: %d bytes", len(icon))
	}
	if got := binary.LittleEndian.Uint16(icon[0:]); got != 0 {
		t.Fatalf("ICO reserved field = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint16(icon[2:]); got != 1 {
		t.Fatalf("icon type = %d, want 1 (ICO)", got)
	}
	count := int(binary.LittleEndian.Uint16(icon[4:]))
	if count < 2 {
		t.Fatalf("image count = %d, want several sizes for tray, shortcut, and Explorer", count)
	}

	sizes := map[int]bool{}
	for i := 0; i < count; i++ {
		entry := icon[6+16*i:]
		width := int(entry[0])
		if width == 0 {
			width = 256 // 0 encodes 256 in an ICO directory entry.
		}
		sizes[width] = true
		length := binary.LittleEndian.Uint32(entry[8:])
		offset := binary.LittleEndian.Uint32(entry[12:])
		if offset < 6 || uint64(offset)+uint64(length) > uint64(len(icon)) {
			t.Fatalf("image %d points outside the file: offset=%d length=%d total=%d",
				i, offset, length, len(icon))
		}
	}
	// 16 is what the notification area actually draws; without it the tray icon is a rescale.
	if !sizes[16] {
		t.Errorf("no 16x16 image; sizes present: %v", sizes)
	}
	if !sizes[32] {
		t.Errorf("no 32x32 image; systray loads the tray icon at LR_DEFAULTSIZE (32x32): %v", sizes)
	}
}

func TestShortError(t *testing.T) {
	if got := shortError(assertionError("short")); got != "short" {
		t.Fatalf("shortError = %q", got)
	}
	long := make([]byte, 100)
	for i := range long {
		long[i] = 'x'
	}
	if got := shortError(assertionError(string(long))); len([]rune(got)) > 80 {
		t.Fatalf("shortError was not shortened: %q", got)
	}
}

func TestFriendlyVRAMError(t *testing.T) {
	err := assertionError("start model: Refusing fresh load: 14845 MiB VRAM is free; this profile requires at least 15000 MiB before startup.")
	want := "Not enough VRAM: 14,845 MiB free; 15,000 MiB required"
	if got := friendlyError(err); got != want {
		t.Fatalf("friendlyError = %q, want %q", got, want)
	}
}

func TestTrimEllipsis(t *testing.T) {
	for label, want := range map[string]string{
		"Starting LocalHub…":   "Starting LocalHub",
		"Starting LocalHub...": "Starting LocalHub",
		"Checking LocalHub":    "Checking LocalHub",
	} {
		if got := trimEllipsis(label); got != want {
			t.Errorf("trimEllipsis(%q) = %q, want %q", label, got, want)
		}
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }
