package safetensors

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// WeightsInfo carries the authoritative weights size for a model, plus its provenance so the
// verdict can label confidence. TotalBytes > 0 means we have an exact on-disk/metadata size;
// TotalBytes == 0 means the estimator must fall back to param-count * bytes-per-param.
//
// Provenance (in the spec's preference order, §4(1)):
//  1. WeightsFromIndex   — model.safetensors.index.json metadata.total_size (exact).
//  2. WeightsFromShards  — sum of safetensors shard sizes from a file listing (exact bytes).
//  3. WeightsFromHeader  — parsed safetensors header(s) of a LOCAL file (exact, optional).
//  4. WeightsUnknown     — no size available; caller must estimate from param count.
type WeightsInfo struct {
	TotalBytes int64
	Source     WeightsSource
}

// WeightsSource labels how TotalBytes was obtained (for verdict confidence + notes).
type WeightsSource string

const (
	WeightsUnknown    WeightsSource = "unknown"
	WeightsFromIndex  WeightsSource = "index_total_size"
	WeightsFromShards WeightsSource = "shard_sum"
	WeightsFromHeader WeightsSource = "safetensors_header"
)

// safetensorsIndex is the defensive subset of model.safetensors.index.json. The standard
// HF sharded-weights index carries metadata.total_size (bytes of all tensors across shards).
type safetensorsIndex struct {
	Metadata struct {
		TotalSize int64 `json:"total_size"`
	} `json:"metadata"`
	WeightMap map[string]string `json:"weight_map"`
}

// ParseIndexTotalSize reads model.safetensors.index.json bytes and returns the exact total
// weights size from metadata.total_size. ok=false when the JSON is unparseable or the field
// is absent/zero (caller then falls back to shard sum or param-count estimate).
func ParseIndexTotalSize(raw []byte) (WeightsInfo, bool) {
	var idx safetensorsIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return WeightsInfo{Source: WeightsUnknown}, false
	}
	if idx.Metadata.TotalSize <= 0 {
		return WeightsInfo{Source: WeightsUnknown}, false
	}
	return WeightsInfo{TotalBytes: idx.Metadata.TotalSize, Source: WeightsFromIndex}, true
}

// WeightsFromShardSizes sums a list of per-shard byte sizes (as would come from an HF API
// file listing or a local directory stat). ok=false if the list is empty or sums to zero.
func WeightsFromShardSizes(shardSizes []int64) (WeightsInfo, bool) {
	var total int64
	for _, s := range shardSizes {
		if s > 0 {
			total += s
		}
	}
	if total <= 0 {
		return WeightsInfo{Source: WeightsUnknown}, false
	}
	return WeightsInfo{TotalBytes: total, Source: WeightsFromShards}, true
}

// safetensors header format (github.com/huggingface/safetensors): the file begins with a
// little-endian uint64 giving the byte length N of a JSON header, followed by N bytes of
// JSON, followed by the raw tensor bytes. Each JSON entry (except "__metadata__") is
// {"dtype","shape","data_offsets":[start,end]}; end is the offset of the last tensor byte,
// so the largest end == total tensor-data byte size for that file.
const stHeaderLenSize = 8 // uint64 header-length prefix

// SumLocalSafetensorsHeaders parses the 8-byte length + JSON header of each LOCAL
// safetensors file path and returns the summed tensor-data size across files, WITHOUT
// reading the (large) tensor bodies. This is optional (spec: "implement but keep optional")
// and used only when neither an index nor a shard listing is available. It reads at most the
// header of each file. ok=false if no path yields a valid header.
func SumLocalSafetensorsHeaders(paths []string) (WeightsInfo, bool) {
	var total int64
	var any bool
	for _, p := range paths {
		n, err := localSafetensorsDataSize(p)
		if err != nil || n <= 0 {
			continue
		}
		total += n
		any = true
	}
	if !any || total <= 0 {
		return WeightsInfo{Source: WeightsUnknown}, false
	}
	return WeightsInfo{TotalBytes: total, Source: WeightsFromHeader}, true
}

// localSafetensorsDataSize reads only the header of one .safetensors file and returns the
// total tensor-data byte size (max data_offsets end across tensors). It never reads tensor
// bodies. A sane cap on header length guards against corrupt files.
func localSafetensorsDataSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var lenBuf [stHeaderLenSize]byte
	if _, err := io.ReadFull(f, lenBuf[:]); err != nil {
		return 0, fmt.Errorf("safetensors: read header length: %w", err)
	}
	hlen := binary.LittleEndian.Uint64(lenBuf[:])
	// Guard: header JSON should be small (KB–MB). 256 MiB is an absurd upper bound that still
	// rejects garbage without a real risk of a false negative.
	const maxHeaderBytes = 256 * 1024 * 1024
	if hlen == 0 || hlen > maxHeaderBytes {
		return 0, fmt.Errorf("safetensors: implausible header length %d", hlen)
	}
	hbuf := make([]byte, hlen)
	if _, err := io.ReadFull(f, hbuf); err != nil {
		return 0, fmt.Errorf("safetensors: read header json: %w", err)
	}

	// The header is a flat map of tensor-name -> object; "__metadata__" is a special entry.
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(hbuf, &hdr); err != nil {
		return 0, fmt.Errorf("safetensors: parse header json: %w", err)
	}
	var maxEnd int64
	for name, raw := range hdr {
		if name == "__metadata__" {
			continue
		}
		var t struct {
			DataOffsets []int64 `json:"data_offsets"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		if len(t.DataOffsets) == 2 && t.DataOffsets[1] > maxEnd {
			maxEnd = t.DataOffsets[1]
		}
	}
	if maxEnd <= 0 {
		return 0, fmt.Errorf("safetensors: no tensor offsets in header")
	}
	return maxEnd, nil
}
