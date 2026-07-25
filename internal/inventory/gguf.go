package inventory

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// GGUF header sniffing.
//
// This is NOT a GGUF parser. Full metadata parsing (KV/tensor counts, arch, ctx) is P2.1
// and belongs to gguf-parser-go [ADR-002]. Here we only read the first 8 bytes to confirm
// a file really is a GGUF container and capture its version, so a mislabeled or truncated
// file doesn't enter inventory as a valid model.
//
// GGUF layout (github.com/ggml-org/ggml/blob/master/docs/gguf.md):
//   bytes 0..3  magic "GGUF" (0x47 0x47 0x55 0x46)
//   bytes 4..7  version: uint32, little-endian (v1, v2, v3 exist)

// ggufMagic is the 4-byte container magic.
var ggufMagic = [4]byte{'G', 'G', 'U', 'F'}

// ggufHeader is the result of a cheap header sniff.
type ggufHeader struct {
	Version uint32
}

// sniffGGUF reads and validates the GGUF magic + version from the start of the file at
// path. It returns an error if the file is too short, unreadable, or lacks the magic —
// callers treat that as "not a valid GGUF" and skip the file (with a reason) rather than
// inventorying a corrupt/partial artifact.
//
// Only 8 bytes are read regardless of file size, so this is cheap even for 13 GB weights.
func sniffGGUF(path string) (ggufHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return ggufHeader{}, fmt.Errorf("open gguf %q: %w", path, err)
	}
	defer f.Close()

	var buf [8]byte
	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return ggufHeader{}, fmt.Errorf("read gguf header %q: %w", path, err)
	}
	if buf[0] != ggufMagic[0] || buf[1] != ggufMagic[1] || buf[2] != ggufMagic[2] || buf[3] != ggufMagic[3] {
		return ggufHeader{}, fmt.Errorf("bad gguf magic in %q: got %q, want \"GGUF\"", path, string(buf[0:4]))
	}
	ver := binary.LittleEndian.Uint32(buf[4:8])
	// Versions 1..3 are known; anything wildly out of range signals a byte-order or
	// corruption problem. We accept a generous ceiling rather than hardcoding the current
	// max, since new versions ship; but a version of 0 or an absurd value is rejected.
	if ver == 0 || ver > 1000 {
		return ggufHeader{}, fmt.Errorf("implausible gguf version %d in %q (corrupt or wrong byte order)", ver, path)
	}
	return ggufHeader{Version: ver}, nil
}
