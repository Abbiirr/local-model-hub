package llamacpp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveBinary turns a candidate into an absolute path. A bare name (no path separator) is
// resolved via PATH using exec.LookPath. An explicit path is checked for existence. Returns
// an error if the binary cannot be found — the caller turns that into a Detected=false
// status with a Reason.
func resolveBinary(candidate string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", os.ErrNotExist
	}
	if isExplicitPath(candidate) {
		info, err := os.Stat(candidate)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", &os.PathError{Op: "stat", Path: candidate, Err: os.ErrInvalid}
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			return candidate, nil
		}
		return abs, nil
	}
	return exec.LookPath(candidate)
}

// isExplicitPath reports whether a candidate is an explicit filesystem path (contains a
// path separator or drive prefix) rather than a bare command name to resolve via PATH.
func isExplicitPath(candidate string) bool {
	if strings.ContainsRune(candidate, '/') || strings.ContainsRune(candidate, filepath.Separator) {
		return true
	}
	// Windows drive prefix like "K:" or "C:\..." even if only backslashes.
	if len(candidate) >= 2 && candidate[1] == ':' {
		return true
	}
	return false
}

// envWithExtraPath returns the current process environment with extraPath directories
// prepended to PATH (using the OS-specific list separator). Prepending (not appending)
// ensures the extra directories win for DLL/shared-library resolution — the CUDA fork
// needs its CUDA v12.5 bin dir found first. If extraPath is empty the environment is
// returned unchanged.
func envWithExtraPath(extraPath []string) []string {
	if len(extraPath) == 0 {
		return os.Environ()
	}
	sep := string(os.PathListSeparator)
	prepend := strings.Join(extraPath, sep)

	env := os.Environ()
	out := make([]string, 0, len(env))
	replaced := false
	for _, kv := range env {
		// PATH key is case-insensitive on Windows ("Path"); compare the key portion
		// case-insensitively.
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			if strings.EqualFold(kv[:eq], "PATH") {
				out = append(out, kv[:eq]+"="+prepend+sep+kv[eq+1:])
				replaced = true
				continue
			}
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+prepend)
	}
	return out
}

// quote wraps s in double quotes for readable error/Reason messages.
func quote(s string) string { return "\"" + s + "\"" }
