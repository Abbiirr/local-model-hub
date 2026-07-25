package gguf

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// testhelper_test.go generates tiny synthetic GGUF files for the parse-path and
// fallback-path unit tests. A synthetic file has a valid GGUF v3 header, a
// minimal set of KV metadata, and either (a) no tensors, or (b) a single tensor
// whose GGML type we control (used to fake a TQ3-like unknown type: GGMLType 46).
// These files are a few hundred bytes — no real weights.

// gguf metadata value type ids (mirror the const block in fallback.go / the spec).
const (
	tMvUint32 uint32 = 4
	tMvString uint32 = 8
	tMvUint64 uint32 = 10
)

// syntheticModel describes the KV metadata to embed.
type syntheticModel struct {
	arch          string
	blockCount    uint32 // n_layer
	embedding     uint32 // n_embd
	headCount     uint32 // n_head
	headCountKV   uint32 // n_head_kv (0 => omit key => MHA)
	contextLength uint32 // trained max ctx (0 => omit)
}

// writeLEString writes a GGUF string (uint64 len prefix + bytes).
func writeLEString(buf *bytes.Buffer, s string) {
	_ = binary.Write(buf, binary.LittleEndian, uint64(len(s)))
	buf.WriteString(s)
}

// writeKVString writes a full string KV entry (key + type + value).
func writeKVString(buf *bytes.Buffer, key, val string) {
	writeLEString(buf, key)
	_ = binary.Write(buf, binary.LittleEndian, tMvString)
	writeLEString(buf, val)
}

// writeKVUint32 writes a full uint32 KV entry.
func writeKVUint32(buf *bytes.Buffer, key string, val uint32) {
	writeLEString(buf, key)
	_ = binary.Write(buf, binary.LittleEndian, tMvUint32)
	_ = binary.Write(buf, binary.LittleEndian, val)
}

// buildGGUF builds a synthetic GGUF v3 byte stream. If tensor is true, it emits
// one tensor-info record with the given ggmlType (used to fake TQ3 GGMLType 46)
// plus a small tensor-data blob so the library reaches tensor parsing.
func buildGGUF(m syntheticModel, tensor bool, ggmlType uint32) []byte {
	var kv bytes.Buffer
	kvCount := uint64(0)

	writeKVString(&kv, "general.architecture", m.arch)
	kvCount++
	writeKVUint32(&kv, m.arch+".block_count", m.blockCount)
	kvCount++
	writeKVUint32(&kv, m.arch+".embedding_length", m.embedding)
	kvCount++
	writeKVUint32(&kv, m.arch+".attention.head_count", m.headCount)
	kvCount++
	if m.headCountKV != 0 {
		writeKVUint32(&kv, m.arch+".attention.head_count_kv", m.headCountKV)
		kvCount++
	}
	if m.contextLength != 0 {
		writeKVUint32(&kv, m.arch+".context_length", m.contextLength)
		kvCount++
	}

	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(ggufMagicLE)) // "GGUF" (must be 4 bytes)
	_ = binary.Write(&out, binary.LittleEndian, uint32(3))           // version 3

	tensorCount := uint64(0)
	if tensor {
		tensorCount = 1
	}
	_ = binary.Write(&out, binary.LittleEndian, tensorCount)
	_ = binary.Write(&out, binary.LittleEndian, kvCount)
	out.Write(kv.Bytes())

	if tensor {
		// tensor-info record: name(string) | n_dims(uint32) | dims(uint64*n) |
		// ggml_type(uint32) | offset(uint64)
		writeLEString(&out, "token_embd.weight")
		_ = binary.Write(&out, binary.LittleEndian, uint32(1)) // n_dims
		_ = binary.Write(&out, binary.LittleEndian, uint64(32))
		_ = binary.Write(&out, binary.LittleEndian, ggmlType) // faked type
		_ = binary.Write(&out, binary.LittleEndian, uint64(0))
		// A little tensor data padding so any downstream read has bytes.
		out.Write(make([]byte, 64))
	}

	return out.Bytes()
}

// writeSyntheticGGUF writes a synthetic GGUF into a temp dir and returns its path.
func writeSyntheticGGUF(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write synthetic gguf: %v", err)
	}
	return p
}
