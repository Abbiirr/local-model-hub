package inventory

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Scanner discovers local models under configured model directories and (optionally) from
// the Ollama daemon. It is stateless between scans and safe to call repeatedly; idempotency
// lives in the repository, which upserts on the deterministic natural key.
//
// Construct with New. The zero value is not usable (nil dirs → empty scan).
type Scanner struct {
	// dirs are the roots to walk for GGUF files and HF/safetensors model directories
	// (config Data.ModelDirs). Each is walked independently and used as the relative-path
	// root for natural-key derivation.
	dirs []string
	// ollama is the optional Ollama source. nil disables the Ollama pass (e.g. when the
	// engine is not configured/reachable) — the disk scan still runs.
	ollama OllamaLister
}

// Option configures a Scanner.
type Option func(*Scanner)

// WithOllama sets the Ollama source. Pass nil (or omit) to skip the Ollama pass.
func WithOllama(l OllamaLister) Option {
	return func(s *Scanner) { s.ollama = l }
}

// New builds a Scanner over the given model directories with optional sources.
func New(modelDirs []string, opts ...Option) *Scanner {
	s := &Scanner{dirs: append([]string(nil), modelDirs...)}
	for _, o := range opts {
		o(s)
	}
	return s
}

// partialSuffixes are download-litter extensions that must be excluded from inventory and
// logged (TESTING.md#p14; the reference box has Qwen3.6-27B-TQ3_4S.gguf.part.aria2 and
// Qwen3.5-27B-TQ3_1S.gguf.tmp). Matched case-insensitively as a suffix of the full name so
// "*.gguf.part.aria2" and "*.gguf.tmp" are caught.
var partialSuffixes = []string{
	".part", ".tmp", ".aria2", ".part.aria2", ".download", ".incomplete", ".crdownload",
}

// isPartial reports whether name looks like an in-progress/partial download.
func isPartial(name string) bool {
	low := strings.ToLower(name)
	for _, suf := range partialSuffixes {
		if strings.HasSuffix(low, suf) {
			return true
		}
	}
	return false
}

// Scan performs one discovery pass across all sources and returns normalized records. It
// never returns a hard error for a single unreadable file/dir; those are collected in
// ScanResult.Errors and the scan continues (a background inventory walk should be robust to
// litter). A cancelled context aborts the walk and returns what was gathered so far plus
// ctx.Err() in Errors.
func (s *Scanner) Scan(ctx context.Context) ScanResult {
	var res ScanResult

	for _, root := range s.dirs {
		if err := ctx.Err(); err != nil {
			res.Errors = append(res.Errors, err)
			return res
		}
		s.scanDir(ctx, root, &res)
	}

	if s.ollama != nil {
		list, err := s.ollama.ListModels(ctx)
		if err != nil {
			// Ollama being down must not fail the whole inventory; record and move on.
			res.Errors = append(res.Errors, err)
		} else {
			res.Models = append(res.Models, modelsFromOllama(list)...)
		}
	}

	return res
}

// scanDir walks one root, collecting GGUF files and safetensors/HF directories.
func (s *Scanner) scanDir(ctx context.Context, root string, res *ScanResult) {
	// Directory-level pass first: identify HF/safetensors model roots. A directory is a
	// model root if it holds config.json + (any *.safetensors OR model.safetensors.index
	// .json). Such a directory is inventoried as ONE record and its contents are not also
	// walked as loose GGUFs (they usually contain none anyway). We remember matched roots
	// so the file walk can skip inside them.
	hfRoots := map[string]bool{}

	// gguf collects loose GGUFs by directory so mmproj/multi-part linking is per-directory.
	byDir := map[string][]ggufFile{}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// Unreadable entry: note it, keep walking siblings.
			res.Errors = append(res.Errors, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Skip HF cache/metadata dirs that are never a model root themselves.
			base := d.Name()
			if base == ".cache" || base == ".git" || base == ".huggingface" {
				return fs.SkipDir
			}
			// Is this directory itself an HF/safetensors model root?
			if isHFModelRoot(path) {
				if m, ok := hfModel(root, path, res); ok {
					res.Models = append(res.Models, m)
				}
				hfRoots[path] = true
				return fs.SkipDir // do not descend into a claimed model root.
			}
			return nil
		}

		name := d.Name()

		// Exclude and log partial downloads regardless of extension shape.
		if isPartial(name) {
			res.Skipped = append(res.Skipped, Skipped{
				Path:   path,
				Reason: "partial/in-progress download (excluded from inventory)",
			})
			return nil
		}

		if strings.EqualFold(filepath.Ext(name), ".gguf") {
			info, statErr := d.Info()
			if statErr != nil {
				res.Errors = append(res.Errors, statErr)
				return nil
			}
			dir := filepath.Dir(path)
			byDir[dir] = append(byDir[dir], ggufFile{
				path:    path,
				size:    info.Size(),
				modTime: info.ModTime().UnixNano(),
			})
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipDir) {
		res.Errors = append(res.Errors, walkErr)
	}

	// Resolve GGUFs per directory (needs the whole directory listing to link mmproj/shards).
	for dir, files := range byDir {
		s.resolveGGUFDir(root, dir, files, res)
	}
}

// ggufFile is a loose GGUF discovered during the walk, before mmproj/shard resolution.
type ggufFile struct {
	path    string
	size    int64
	modTime int64
}

// shardGroup collects the shards of one logical GGUF model (a multi-part set, or a single
// file) so it becomes one inventory record.
type shardGroup struct {
	primary ggufFile // shard 00001 (or the sole file)
	parts   []string
	total   int64 // summed size across shards
	modTime int64 // newest mtime across shards
}

// resolveGGUFDir turns the GGUF files found in a single directory into inventory records,
// linking mmproj companions and collapsing multi-part shard sets into one record.
func (s *Scanner) resolveGGUFDir(root, dir string, files []ggufFile, res *ScanResult) {
	// Stable order so shard-0 and record identity are deterministic.
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	// Partition into mmproj companions vs base models.
	var mmprojs []struct {
		path string
		size int64
	}
	var bases []ggufFile
	for _, f := range files {
		if isMMProj(filepath.Base(f.path)) {
			mmprojs = append(mmprojs, struct {
				path string
				size int64
			}{f.path, f.size})
			continue
		}
		bases = append(bases, f)
	}

	// Group base GGUFs by their shard-stripped stem so a multi-part set becomes one record.
	groups := map[string]*shardGroup{}
	var order []string
	for _, b := range bases {
		stem := strings.TrimSuffix(filepath.Base(b.path), filepath.Ext(b.path))
		idx, _, isShard := parseShard(stem)
		key := stripShardSuffix(stem)
		g := groups[key]
		if g == nil {
			g = &shardGroup{}
			groups[key] = g
			order = append(order, key)
		}
		g.parts = append(g.parts, b.path)
		g.total += b.size
		if b.modTime > g.modTime {
			g.modTime = b.modTime
		}
		// The primary artifact is shard 1, or the file itself when not sharded.
		if !isShard || idx == 1 || g.primary.path == "" {
			// Prefer an explicit shard-1; otherwise keep the first/only file as primary.
			if g.primary.path == "" || (isShard && idx == 1) {
				g.primary = b
			}
		}
	}

	for _, key := range order {
		g := groups[key]
		if g.primary.path == "" {
			// All members were shards but none was index 1 (unusual). Fall back to the
			// lexically-first part as primary so we still produce a record.
			sort.Strings(g.parts)
			g.primary = ggufFile{path: g.parts[0], size: g.total, modTime: g.modTime}
		}

		// Validate the primary artifact's GGUF header (cheap 8-byte sniff).
		hdr, err := sniffGGUF(g.primary.path)
		if err != nil {
			res.Skipped = append(res.Skipped, Skipped{
				Path:   g.primary.path,
				Reason: "not a valid GGUF (header check failed): " + err.Error(),
			})
			continue
		}
		_ = hdr // version captured for future use (P2.1); not a models column.

		m := Model{
			ID:           ggufNatKey(root, g.primary.path),
			DisplayName:  strings.TrimSuffix(filepath.Base(g.primary.path), filepath.Ext(g.primary.path)),
			Source:       SourceLocalPath,
			Format:       FormatGGUF,
			Quantization: DetectQuant(filepath.Base(g.primary.path)),
			LocalPath:    g.primary.path,
			SizeBytes:    sizePtr(g.total),
			// SHA256 intentionally nil — hash-deferred policy (see model.go package doc).
			ModTime: nanoToTime(g.modTime),
		}
		if len(g.parts) > 1 {
			sort.Strings(g.parts)
			m.Parts = g.parts
		}
		// Link mmproj companions to this base. With a single base in the directory (the
		// common layout on the reference box: one model + its projector), link ALL mmprojs.
		// With multiple bases, avoid indiscriminate cross-linking: prefer name-affinity
		// (mmproj filename shares a stem token with the base) and fall back to linking a
		// mmproj to every base only when no base has an affinity match for it, so a genuinely
		// shared projector is still surfaced rather than lost.
		for _, mp := range mmprojs {
			link := len(bases) == 1 || len(groups) == 1 ||
				mmprojAffinity(filepath.Base(g.primary.path), filepath.Base(mp.path)) ||
				!mmprojHasAnyAffinity(filepath.Base(mp.path), order, groups)
			if link {
				m.Companions = append(m.Companions, Companion{
					Kind:      "mmproj",
					Path:      mp.path,
					SizeBytes: mp.size,
				})
			}
		}
		res.Models = append(res.Models, m)
	}

	// Orphan mmproj handling: an mmproj with no base GGUF in its directory is not a
	// standalone model (it can't run alone). Record it as skipped-with-reason so it is
	// logged, not silently dropped and not mistaken for a model.
	if len(bases) == 0 {
		for _, mp := range mmprojs {
			res.Skipped = append(res.Skipped, Skipped{
				Path:   mp.path,
				Reason: "orphan mmproj (multimodal projector with no base GGUF in its directory); not a standalone model",
			})
		}
	}
}

// mmprojAffinity reports whether an mmproj filename shares a distinctive token with a base
// GGUF filename, indicating they belong together (e.g. base "PaddleOCR-VL-1.5.gguf" and
// mmproj "PaddleOCR-VL-1.5-mmproj.gguf"). Tokens are the alphanumeric runs of the names,
// lowercased, with the literal "mmproj" and pure-quant tokens removed; a shared token of
// length >= 3 counts as affinity. Conservative by design: false negatives just fall back to
// the shared-projector heuristic.
func mmprojAffinity(baseName, mmprojName string) bool {
	bt := distinctiveTokens(baseName)
	mt := distinctiveTokens(mmprojName)
	for tok := range mt {
		if len(tok) >= 3 && bt[tok] {
			return true
		}
	}
	return false
}

// mmprojHasAnyAffinity reports whether the given mmproj has affinity with ANY base group in
// the directory. Used to decide whether an mmproj is "owned" by a specific base (link only
// there) or is unattached (treat as shared and link to all).
func mmprojHasAnyAffinity(mmprojName string, order []string, groups map[string]*shardGroup) bool {
	for _, key := range order {
		g := groups[key]
		if g.primary.path == "" {
			continue
		}
		if mmprojAffinity(filepath.Base(g.primary.path), mmprojName) {
			return true
		}
	}
	return false
}

// paramCountToken matches a parameter-count size marker like "27b", "8b", "35b", "a3b",
// "1_5b" — these describe model SIZE, not identity, and are shared across unrelated models,
// so they must not count toward mmproj name-affinity.
var paramCountToken = regexp.MustCompile(`^a?\d+(?:_\d+)?b$`)

// distinctiveTokens splits a filename into lowercased alphanumeric tokens that identify the
// MODEL (not its size or quant). It drops: the "mmproj" marker; bare quant labels; pure
// numeric tokens (version fragments like "5" that collide trivially); and parameter-count
// size markers (27b/8b/a3b). What remains — model family/name tokens like "qwopus3",
// "qwen3", "paddleocr", "glm" — is what makes two files provably the same model.
func distinctiveTokens(name string) map[string]bool {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	out := map[string]bool{}
	for _, tok := range nonSlug.Split(strings.ToLower(name), -1) {
		if tok == "" || tok == "mmproj" {
			continue
		}
		if DetectQuant(tok) == tok { // token IS a bare quant label → not distinctive.
			continue
		}
		if isAllDigits(tok) { // bare version/number fragment → not distinctive.
			continue
		}
		if paramCountToken.MatchString(tok) { // size marker (27b, a3b) → not distinctive.
			continue
		}
		out[tok] = true
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// mmprojPattern matches multimodal projector companion filenames. llama.cpp names these
// "mmproj*.gguf" or "*-mmproj*.gguf" / "*mmproj*.gguf" (github.com/ggml-org/llama.cpp
// multimodal). Case-insensitive substring "mmproj" is the reliable signal on this box
// (mmproj-BF16.gguf, mmproj-F16.gguf, Qwopus3.5-27B-v3-mmproj.gguf, mmproj-GLM-OCR-Q8_0.gguf).
func isMMProj(name string) bool {
	return strings.Contains(strings.ToLower(name), "mmproj")
}
