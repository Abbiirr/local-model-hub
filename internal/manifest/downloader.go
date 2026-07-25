package manifest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the high-level download orchestrator: it composes HF resolve → size guard →
// per-file download (resume + hash + verify) → warning scan → immutable manifest. The core
// calls these; the pieces (HFClient, FileFetcher, GuardConfig, Store, Scan) are individually
// testable. Everything here honors path containment: files land inside DestRoot which the
// core validates against configured model dirs.

// Downloader ties the download pieces together with shared config (guards, containment,
// strict mode). Construct with NewDownloader.
type Downloader struct {
	HF      *HFClient
	Fetcher *FileFetcher
	Guard   GuardConfig
	// DestRoot is the directory downloads land in (must be inside a configured model dir —
	// the core validates this). Per-repo/per-file paths are SafeJoin-ed under it.
	DestRoot string
	// StrictMode, when true, upgrades a pickle warning to a REFUSAL (opt-in, off by default,
	// ADR-004). It is the ONLY thing that blocks a download.
	StrictMode bool
}

// HFDownloadRequest selects an HF download.
type HFDownloadRequest struct {
	Repo string
	Ref  string // branch/tag/sha; "" => main. Resolved to a commit before download.
	// Files selects which repo files to download. Empty => a sensible default set (all GGUF
	// for a GGUF repo, or all safetensors + config/tokenizer for a safetensors repo). For a
	// single-GGUF pick, pass the one filename.
	Files []string
}

// ResolveHF resolves the ref to a commit + file listing WITHOUT downloading. This backs the
// first step of the two-step flow (spec-digest §7 "show resolved commit SHA before
// downloading"): the API returns this + the compat verdict, and only a second call with
// confirm:true proceeds to DownloadHF.
func (d *Downloader) ResolveHF(ctx context.Context, repo, ref string) (RepoInfo, error) {
	return d.HF.ResolveRevision(ctx, repo, ref)
}

// DownloadHF downloads the selected files at the resolved commit, verifying each file's
// sha256 against HF's LFS hash when available, and returns the immutable manifest (NOT yet
// persisted — the core persists it and links the model row in one transaction). info is the
// already-resolved RepoInfo (so the two-step flow doesn't re-resolve between show and commit).
func (d *Downloader) DownloadHF(ctx context.Context, info RepoInfo, files []string, prog ProgressFunc) (*Manifest, error) {
	selected := selectFiles(info.Files, files)
	if len(selected) == 0 {
		return nil, fmt.Errorf("no files selected for download from %s@%s", info.Repo, info.CommitSHA)
	}

	// Size guard on the total (before any bytes fetched).
	var total int64
	for _, f := range selected {
		total += f.SizeBytes
	}
	if err := d.Guard.CheckDownloadSize(total); err != nil {
		return nil, err
	}

	// Pre-flight strict-mode / pickle check on the RESOLVED file paths (we can warn before
	// downloading — the fileset is known from metadata). This lets strict mode refuse without
	// fetching a byte.
	preWarnings := Scan(ScanInput{
		Files:   toManifestFiles(selected),
		License: info.License,
		Gated:   info.Gated,
	})
	if d.StrictMode && HasPickleWarning(preWarnings) {
		return nil, fmt.Errorf(
			"strict mode: refusing to download %s — it contains pickle-format weights with no safetensors alternative (%s). Disable security.strict_model_artifact_mode to allow it",
			info.Repo, WarnPickle)
	}

	m := &Manifest{
		ID:           newUUID(),
		Source:       SourceHF,
		Repo:         info.Repo,
		Revision:     info.CommitSHA,
		License:      info.License,
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
	}

	for i, f := range selected {
		dest, err := SafeJoin(d.DestRoot, hfLocalRel(info.Repo, f.Path))
		if err != nil {
			return nil, err
		}
		url := d.HF.ResolveURL(info.Repo, info.CommitSHA, f.Path)
		res, err := d.Fetcher.Fetch(ctx, url, dest, f.LFSSHA256, prog, i+1, len(selected))
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", f.Path, err)
		}
		m.Files = append(m.Files, File{Path: f.Path, SHA256: res.SHA256, SizeBytes: res.SizeBytes})
		// Record the primary local path + format/quant from the biggest weight file.
		if isWeightFile(f.Path) && (m.LocalPath == "" || res.SizeBytes > 0) {
			m.LocalPath = res.Path
			m.Format = formatFromExt(f.Path)
			if q := quantFromName(f.Path); q != "" {
				m.Quantization = q
			}
		}
	}

	// Final warning scan over the downloaded fileset (identical result, but now definitive).
	m.SecurityWarnings = Scan(ScanInput{
		Files:   m.Files,
		License: info.License,
		Gated:   info.Gated,
	})
	return m, nil
}

// GGUFURLRequest selects a single-GGUF direct-URL download.
type GGUFURLRequest struct {
	URL      string
	Filename string // destination basename; derived from the URL when empty
}

// DownloadGGUFURL downloads a single .gguf from a direct URL with the same machinery (resume,
// streamed sha256, guard). No HF metadata / LFS hash is available, so the hash is recorded but
// not cross-verified. Returns the immutable manifest (unpersisted).
func (d *Downloader) DownloadGGUFURL(ctx context.Context, req GGUFURLRequest, prog ProgressFunc) (*Manifest, error) {
	name := strings.TrimSpace(req.Filename)
	if name == "" {
		name = urlBasename(req.URL)
	}
	if name == "" {
		return nil, fmt.Errorf("could not derive a filename from URL %q; pass filename explicitly", req.URL)
	}
	dest, err := SafeJoin(d.DestRoot, name)
	if err != nil {
		return nil, err
	}
	// Size unknown up front (no metadata) → the hard cap is enforced during/after by the disk,
	// but we still can't precheck. Proceed; the fetcher streams and ctx can cancel.
	res, err := d.Fetcher.Fetch(ctx, req.URL, dest, "", prog, 1, 1)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	// Post-hoc hard-cap check (defensive): if the downloaded file blew the cap, flag it.
	if res.SizeBytes > d.Guard.hardCap() {
		return nil, fmt.Errorf("downloaded %s exceeds the %s hard cap (%s); removing", name, humanBytes(d.Guard.hardCap()), humanBytes(res.SizeBytes))
	}
	m := &Manifest{
		ID:           newUUID(),
		Source:       SourceGGUFURL,
		URL:          req.URL,
		Format:       "gguf",
		Quantization: quantFromName(name),
		LocalPath:    res.Path,
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		Files:        []File{{Path: name, SHA256: res.SHA256, SizeBytes: res.SizeBytes}},
	}
	m.SecurityWarnings = Scan(ScanInput{Files: m.Files})
	return m, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// selectFiles returns the RepoInfo files matching `want` (by exact path or basename). An empty
// `want` returns the default download set: all weight files (gguf/safetensors/pickle) plus the
// small config/tokenizer files needed to run (config.json, *.json, tokenizer*).
func selectFiles(all []HFFile, want []string) []HFFile {
	if len(want) > 0 {
		wantSet := map[string]bool{}
		for _, w := range want {
			wantSet[w] = true
			wantSet[filepath.Base(w)] = true
		}
		var out []HFFile
		for _, f := range all {
			if wantSet[f.Path] || wantSet[filepath.Base(f.Path)] {
				out = append(out, f)
			}
		}
		return out
	}
	var out []HFFile
	for _, f := range all {
		if isWeightFile(f.Path) || isRunConfigFile(f.Path) {
			out = append(out, f)
		}
	}
	return out
}

// isRunConfigFile matches the small metadata files needed to run a model (kept in the default
// download set alongside weights).
func isRunConfigFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "config.json", "generation_config.json", "tokenizer.json", "tokenizer_config.json",
		"special_tokens_map.json", "vocab.json", "merges.txt", "model.safetensors.index.json":
		return true
	}
	return strings.HasPrefix(base, "tokenizer")
}

func toManifestFiles(fs []HFFile) []File {
	out := make([]File, 0, len(fs))
	for _, f := range fs {
		out = append(out, File{Path: f.Path, SHA256: f.LFSSHA256, SizeBytes: f.SizeBytes})
	}
	return out
}

// hfLocalRel maps a repo file path to its local relative path under DestRoot. We namespace by
// repo (owner__name) so two repos' config.json don't collide.
func hfLocalRel(repo, filePath string) string {
	safeRepo := strings.ReplaceAll(repo, "/", "__")
	return filepath.Join(safeRepo, filepath.FromSlash(filePath))
}

func formatFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".gguf":
		return "gguf"
	case ".safetensors":
		return "safetensors"
	case ".bin", ".pt", ".pth", ".ckpt":
		return "pytorch"
	}
	return ""
}

func quantFromName(file string) string {
	low := strings.ToLower(file)
	for _, q := range []string{"tq3_4s", "tq3_1s", "tq3_0", "q2_k", "q3_k_m", "q4_k_m", "q4_k_s", "q4_0", "q5_k_m", "q5_0", "q6_k", "q8_0", "iq4_xs", "bf16", "fp16", "f16"} {
		if strings.Contains(low, q) {
			return q
		}
	}
	return ""
}

func urlBasename(u string) string {
	u = strings.SplitN(u, "?", 2)[0]
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

// NewUUID returns a random RFC4122 v4 uuid string. Exported so the core can mint download-job
// ids with the same generator (keeping crypto/rand out of core).
func NewUUID() string { return newUUID() }

// SelectFilesForView exposes the default/explicit file selection to the core's
// show-before-commit view, so the resolved payload marks exactly what DownloadHF would fetch.
func SelectFilesForView(all []HFFile, want []string) []HFFile { return selectFiles(all, want) }

// newUUID returns a random RFC4122-ish v4 uuid string for a manifest id.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// NewDownloader builds a Downloader with sensible defaults.
func NewDownloader(destRoot string, guard GuardConfig, strict bool, token string) *Downloader {
	return &Downloader{
		HF:         &HFClient{Token: token},
		Fetcher:    &FileFetcher{Token: token},
		Guard:      guard,
		DestRoot:   destRoot,
		StrictMode: strict,
	}
}

// ── local import ─────────────────────────────────────────────────────────────

// LocalImportRequest registers an existing on-disk file/dir as a model with a local_import
// manifest. The path MUST already be inside a configured model dir (the core validates
// containment). Hashing large files is deferred to a background job (HashPending=true);
// FinalizeHashes completes it later.
type LocalImportRequest struct {
	Path      string // absolute path to an existing .gguf file (or model dir)
	DeferHash bool   // true => write manifest with hash_pending, hash in background
}

// BuildLocalImportManifest stats the path, scans warnings, and returns a local_import
// manifest. When DeferHash is true the file hash is left empty and HashPending=true (the core
// spawns a background hasher and calls FinalizeHashes). When false, the hash is computed
// synchronously (small files only).
func BuildLocalImportManifest(ctx context.Context, req LocalImportRequest) (*Manifest, error) {
	fi, err := os.Stat(req.Path)
	if err != nil {
		return nil, fmt.Errorf("stat import path %q: %w", req.Path, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("local import of a directory is not supported yet; point at the primary .gguf file")
	}
	name := filepath.Base(req.Path)
	m := &Manifest{
		ID:           newUUID(),
		Source:       SourceLocalImport,
		Format:       formatFromExt(name),
		Quantization: quantFromName(name),
		LocalPath:    req.Path,
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		Files:        []File{{Path: name, SizeBytes: fi.Size()}},
	}
	m.SecurityWarnings = Scan(ScanInput{Files: m.Files, LocalImport: true})

	if req.DeferHash {
		m.HashPending = true
		return m, nil
	}
	// Synchronous hash (small files).
	fp, err := os.Open(req.Path)
	if err != nil {
		return nil, fmt.Errorf("open import path %q: %w", req.Path, err)
	}
	defer fp.Close()
	sum, _, err := HashReader(fp)
	if err != nil {
		return nil, fmt.Errorf("hash import path %q: %w", req.Path, err)
	}
	m.Files[0].SHA256 = sum
	return m, nil
}

// HashFile computes the sha256 of a single file (for the background finalizer). Returns the
// lowercase hex digest.
func HashFile(path string) (string, error) {
	fp, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fp.Close()
	sum, _, err := HashReader(fp)
	return sum, err
}
