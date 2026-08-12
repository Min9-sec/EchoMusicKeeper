//go:build windows

package security

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsAtomicDirectoryCreateOptions(t *testing.T) {
	want := uintptr(ntFileDirectoryFile | ntFileOpenReparsePoint | ntFileOpenForBackupIntent | ntFileSynchronousIONonalert)
	if ntFileCreateOptions != want {
		t.Fatalf("create options = 0x%x, want 0x%x", ntFileCreateOptions, want)
	}
	if ntFileCreate != 2 || ntObjectDontReparse != 0x1000 {
		t.Fatalf("create disposition/no-reparse = 0x%x/0x%x", ntFileCreate, ntObjectDontReparse)
	}
}

func TestWindowsAtomicDirectoryCreateCollision(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "existing"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, _, err := createChildDirectoryPlatform(root, "existing", nil); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("create existing directory error = %v, want fs.ErrExist", err)
	}
}
