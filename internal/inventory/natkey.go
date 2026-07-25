package inventory

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Natural-key derivation.
//
// models.id is a "stable slug" (spec-digest §2). The scanner needs a DETERMINISTIC id per
// artifact so re-scanning the same file updates its row instead of inserting a duplicate.
// The id is derived purely from stable identity (source + a source-specific key), never
// from volatile facts like size or mtime — those change without the model changing
// identity, and the whole point of the natural key is stability across re-scans.
//
// Keys per source:
//   - local_path (GGUF): slug of the file's path relative to its scan root, minus the
//     .gguf extension and any multi-part shard suffix. Two shards of one model collapse to
//     one id.
//   - huggingface (safetensors dir): slug of the directory's path relative to its scan
//     root (the repo folder name, e.g. "Qwen--Qwen2.5-Coder-7B").
//   - ollama: slug of the Ollama model name (e.g. "llama3.1:8b" -> "llama3.1-8b").
//
// Slugs are lowercased, with path separators and non-slug characters collapsed to single
// hyphens, so they are filesystem/URL-safe and human-recognizable.

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug lowercases s and collapses any run of non-[a-z0-9] characters to a single hyphen,
// trimming leading/trailing hyphens.
func slug(s string) string {
	s = strings.ToLower(s)
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// relOrBase returns path relative to root when possible (so the id encodes some directory
// context and stays unique across subdirs), else the base name. It never returns an
// absolute path so ids don't leak drive letters.
func relOrBase(root, path string) string {
	if root != "" {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return filepath.Base(path)
}

// ggufNatKey builds the natural key for a loose GGUF file: source prefix + slug of the
// shard-stripped, extension-stripped relative path.
func ggufNatKey(root, path string) string {
	rel := relOrBase(root, path)
	rel = strings.TrimSuffix(rel, filepath.Ext(rel)) // drop .gguf
	rel = stripShardSuffix(rel)
	return "gguf-" + slug(rel)
}

// hfNatKey builds the natural key for a safetensors/HF model directory.
func hfNatKey(root, dir string) string {
	rel := relOrBase(root, dir)
	return "hf-" + slug(rel)
}

// ollamaNatKey builds the natural key for an Ollama model name.
func ollamaNatKey(name string) string {
	return "ollama-" + slug(name)
}

// shardSuffix matches a multi-part GGUF shard suffix like "-00001-of-00008" (the llama.cpp
// split convention, github.com/ggml-org/llama.cpp gguf-split). partIndex/partTotal are the
// captured numbers; the whole match is what gets stripped for the natural key.
var shardSuffix = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})$`)

// stripShardSuffix removes a trailing "-NNNNN-of-NNNNN" shard marker if present.
func stripShardSuffix(name string) string {
	return shardSuffix.ReplaceAllString(name, "")
}

// parseShard reports whether base (a .gguf filename without extension) is a shard, and if
// so returns its 1-based index and total count. Non-shard names return ok=false.
func parseShard(base string) (index, total int, ok bool) {
	m := shardSuffix.FindStringSubmatch(base)
	if m == nil {
		return 0, 0, false
	}
	// The regex guarantees 5-digit numeric groups; atoiNoErr is safe here.
	return atoiNoErr(m[1]), atoiNoErr(m[2]), true
}

func atoiNoErr(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
