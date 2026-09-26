// Package testtmp adjusts the process temporary directory for test runs.
package testtmp

import (
	"os"
	"path/filepath"
)

// Canonicalize replaces TMPDIR with its resolved path when the platform
// exposes the temporary directory through a symbolic link. macOS points TMPDIR
// at /var/folders/... and /var is a symbolic link to /private/var, while
// managed directory paths intentionally reject symbolic link components. Tests
// that create directories below TMPDIR must therefore use the resolved path.
func Canonicalize() {
	directory := os.TempDir()
	if directory == "" {
		return
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved == "" || resolved == directory {
		return
	}
	_ = os.Setenv("TMPDIR", resolved)
}
