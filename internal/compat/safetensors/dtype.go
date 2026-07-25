package safetensors

import "strings"

// normalizeDtype folds the various torch_dtype spellings into a small canonical set used for
// bytes-per-param selection. Unknown/empty dtypes fold to "" (caller defaults to fp16).
func normalizeDtype(dtype string) string {
	switch strings.ToLower(strings.TrimSpace(dtype)) {
	case "bfloat16", "float16", "half":
		return "float16"
	case "float32", "float":
		return "float32"
	case "float8_e4m3fn", "float8_e5m2", "fp8", "float8":
		return "float8"
	default:
		return ""
	}
}
