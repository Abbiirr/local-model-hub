package manifest

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// isWindows reports whether the host OS uses case-insensitive, backslash-separated paths.
func isWindows() bool { return runtime.GOOS == "windows" }

// This file implements path-containment validation (research/lmwrapper-reuse-notes.md §5:
// "path containment validation"; spec-digest §7). Downloads and imports MUST land inside a
// configured model directory. This prevents a hostile/malformed repo file path (e.g.
// "../../etc/passwd" or an absolute "/somewhere") from writing outside the model store when
// LocalHub joins a repo-relative path to the download root, and it prevents importing a path
// that isn't actually under a managed directory.
//
// The check is done on cleaned, absolute paths using a path-prefix comparison with a
// separator boundary, so "/models-evil" is NOT considered inside "/models".

// ContainConfig configures path containment. Roots are the configured model directories
// (config.data.model_dirs). At least one root is required for a download target check.
type ContainConfig struct {
	Roots []string
}

// ValidateWithin returns nil iff target (once cleaned+absolute) is inside one of roots.
// It rejects traversal, absolute escapes, and paths under no configured root. The returned
// error names the target and the roots so the user can see why (CLAUDE.md reason rule).
//
// On Windows, comparison is case-insensitive on the volume+path (NTFS is case-insensitive);
// elsewhere it is case-sensitive. Separators are normalized so a mix of / and \ compares
// correctly.
func ValidateWithin(target string, roots []string) error {
	if len(roots) == 0 {
		return fmt.Errorf("no model directories configured; refusing to place %q (nothing to contain it within)", target)
	}
	at, err := absClean(target)
	if err != nil {
		return fmt.Errorf("resolve target path %q: %w", target, err)
	}
	for _, root := range roots {
		ar, err := absClean(root)
		if err != nil {
			continue
		}
		if isWithin(ar, at) {
			return nil
		}
	}
	return fmt.Errorf(
		"path %q is not inside any configured model directory (%s); refusing to write there (path-containment guard)",
		at, strings.Join(roots, ", "))
}

// SafeJoin joins root and a repo-relative path (as HF returns them, using "/") and returns
// the cleaned absolute result, guaranteeing it stays inside root. It rejects a rel that
// escapes via ".." or is absolute. This is the primitive the HF downloader uses to turn a
// repo file path into a local destination without a traversal risk.
func SafeJoin(root, rel string) (string, error) {
	ar, err := absClean(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	// Reject an absolute or drive-qualified rel outright — a file path from a repo must be
	// relative. We check both OS-native absoluteness AND a leading slash/backslash (a POSIX
	// absolute path is NOT filepath.IsAbs on Windows, but must still be refused).
	if filepath.IsAbs(rel) || strings.ContainsAny(rel, ":") ||
		strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("refusing absolute/qualified file path %q from a repo listing", rel)
	}
	// Normalize forward slashes to the OS separator, then clean-join.
	relOS := filepath.FromSlash(rel)
	joined := filepath.Join(ar, relOS)
	cleaned, err := absClean(joined)
	if err != nil {
		return "", err
	}
	if !isWithin(ar, cleaned) {
		return "", fmt.Errorf("file path %q escapes its download root %q (path-containment guard)", rel, ar)
	}
	return cleaned, nil
}

// absClean returns the absolute, cleaned form of p.
func absClean(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	ap, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(ap), nil
}

// isWithin reports whether child is root or a descendant of root. It compares on a
// separator boundary so "/a/bc" is NOT within "/a/b". On Windows the compare is
// case-insensitive.
func isWithin(root, child string) bool {
	root = strings.TrimRight(root, string(filepath.Separator))
	if eqPath(root, child) {
		return true
	}
	prefix := root + string(filepath.Separator)
	if isWindows() {
		return strings.HasPrefix(strings.ToLower(child), strings.ToLower(prefix))
	}
	return strings.HasPrefix(child, prefix)
}

func eqPath(a, b string) bool {
	if isWindows() {
		return strings.EqualFold(a, b)
	}
	return a == b
}
