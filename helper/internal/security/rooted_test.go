package security

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnchoredDirectoryRejectsReplacedPath(t *testing.T) {
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "managed")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenDirectory(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(rootPath, "recorded.mp3"), []byte("recorded"), 0o600); err != nil {
		t.Fatal(err)
	}
	displaced := rootPath + ".displaced"
	if err := os.Rename(rootPath, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, rootPath); err != nil {
		t.Fatal(err)
	}
	if err := root.Verify(); err == nil {
		t.Fatal("Verify accepted a replaced directory path")
	}
	if err := root.RemoveRegular("recorded.mp3"); err == nil {
		t.Fatal("RemoveRegular accepted a replaced directory path")
	}
	if _, err := os.Stat(filepath.Join(displaced, "recorded.mp3")); err != nil {
		t.Fatalf("anchored source changed: %v", err)
	}
}

func TestVolumeAnchorRenameRejectsSymlinkedParentReplacement(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, "container")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(filepath.Join(container, "old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	anchor, oldRelative, err := OpenVolume(filepath.Join(container, "old"))
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.Close()
	newRelative, err := anchor.Relative(filepath.Join(container, "new"))
	if err != nil {
		t.Fatal(err)
	}
	displaced := container + ".displaced"
	if err := os.Rename(container, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, container); err != nil {
		t.Fatal(err)
	}
	if err := anchor.Rename(oldRelative, newRelative); err == nil {
		t.Fatal("anchored rename accepted a symlinked parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("anchored rename changed replacement target: %v", entries)
	}
}

func TestDirectoryOperationsRejectRelativeSymlinkParents(t *testing.T) {
	operations := map[string]func(*Directory, string) error{
		"open": func(root *Directory, alias string) error {
			file, err := root.OpenFile(filepath.Join(alias, "file.txt"), os.O_RDONLY, 0)
			if file != nil {
				_ = file.Close()
			}
			return err
		},
		"lstat": func(root *Directory, alias string) error {
			_, err := root.Lstat(filepath.Join(alias, "file.txt"))
			return err
		},
		"read directory": func(root *Directory, alias string) error {
			_, err := root.ReadDir(alias)
			return err
		},
		"remove": func(root *Directory, alias string) error {
			return root.Remove(filepath.Join(alias, "file.txt"))
		},
		"remove regular": func(root *Directory, alias string) error {
			return root.RemoveRegular(filepath.Join(alias, "file.txt"))
		},
		"remove all": func(root *Directory, alias string) error {
			return root.RemoveAll(filepath.Join(alias, "nested"))
		},
		"rename": func(root *Directory, alias string) error {
			return root.Rename(filepath.Join(alias, "file.txt"), "renamed.txt")
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "target")
			if err := os.MkdirAll(filepath.Join(target, "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
			filePath := filepath.Join(target, "file.txt")
			if err := os.WriteFile(filePath, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "nested", "child.txt"), []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			alias := "relative-link"
			if err := os.Symlink("target", filepath.Join(parent, alias)); err != nil {
				t.Fatal(err)
			}
			root, err := OpenDirectory(parent, false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := operation(root, alias); err == nil {
				t.Fatalf("%s followed a relative symlink parent", name)
			}
			if got, err := os.ReadFile(filePath); err != nil || string(got) != "outside" {
				t.Fatalf("target file = %q, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(target, "nested", "child.txt")); err != nil {
				t.Fatalf("target directory changed: %v", err)
			}
		})
	}
}

func TestVolumeOperationsRejectRelativeSymlinkParent(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(target, "source.txt")
	if err := os.WriteFile(filePath, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "relative-link")
	if err := os.Symlink("target", alias); err != nil {
		t.Fatal(err)
	}
	anchor, source, err := OpenVolume(filepath.Join(alias, "source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.Close()
	destination, err := anchor.Relative(filepath.Join(parent, "renamed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Rename(source, destination); err == nil {
		t.Fatal("volume rename followed a relative symlink parent")
	}
	if got, err := os.ReadFile(filePath); err != nil || string(got) != "outside" {
		t.Fatalf("target file = %q, %v", got, err)
	}
}

func TestVolumeRenameAcrossVerifiedParents(t *testing.T) {
	parent := t.TempDir()
	sourceParent := filepath.Join(parent, "source")
	destinationParent := filepath.Join(parent, "destination")
	if err := os.MkdirAll(sourceParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destinationParent, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceParent, "cache")
	if err := os.MkdirAll(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "marker"), []byte("moved"), 0o600); err != nil {
		t.Fatal(err)
	}
	anchor, source, err := OpenVolume(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.Close()
	destinationPath := filepath.Join(destinationParent, "cache")
	destination, err := anchor.Relative(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Rename(source, destination); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(destinationPath, "marker")); err != nil || string(got) != "moved" {
		t.Fatalf("renamed marker = %q, %v", got, err)
	}
}

func TestCreateDirectoryOpenFailureKeepsReplacement(t *testing.T) {
	parent := t.TempDir()
	created := filepath.Join(parent, "created")
	displaced := created + ".displaced"
	want := []byte("replacement")
	directory, err := createDirectory(created, func() {
		if err := os.Rename(created, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(created, want, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if directory != nil {
		_ = directory.Close()
		t.Fatal("createDirectory returned a capability for the replacement")
	}
	if err == nil {
		t.Fatal("createDirectory succeeded after the created directory was replaced")
	}
	if got, readErr := os.ReadFile(created); readErr != nil || string(got) != string(want) {
		t.Fatalf("replacement = %q, %v, want %q", got, readErr, want)
	}
	if !strings.Contains(err.Error(), "component is not a real directory") {
		t.Fatalf("createDirectory error = %q, want original open failure", err)
	}
	if !strings.Contains(err.Error(), created) {
		t.Fatalf("createDirectory error = %q, want residual path %q", err, created)
	}
	if info, statErr := os.Stat(displaced); statErr != nil || !info.IsDir() {
		t.Fatalf("displaced created directory = %v, %v, want retained directory", info, statErr)
	}
}

func TestCreateDirectoryRealDirectoryReplacementIsRejected(t *testing.T) {
	parent := t.TempDir()
	created := filepath.Join(parent, "created")
	displaced := created + ".displaced"
	marker := filepath.Join(created, "replacement.txt")
	directory, err := createDirectory(created, func() {
		if err := os.Rename(created, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(created, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if directory != nil {
		_ = directory.Close()
	}
	if err == nil {
		t.Fatal("createDirectory accepted a nonempty real-directory replacement")
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "replacement" {
		t.Fatalf("replacement marker = %q, %v, want untouched", got, readErr)
	}
	if entries, readErr := os.ReadDir(created); readErr != nil || len(entries) != 1 {
		t.Fatalf("replacement entries = %v, %v, want only marker", entries, readErr)
	}
	if info, statErr := os.Stat(displaced); statErr != nil || !info.IsDir() {
		t.Fatalf("displaced created directory = %v, %v, want retained directory", info, statErr)
	}
	if !strings.Contains(err.Error(), "created directory identity location is unknown") {
		t.Fatalf("createDirectory error = %v, want unknown identity location", err)
	}
	if strings.Contains(err.Error(), "residual directory") || strings.Contains(err.Error(), "remove the") {
		t.Fatalf("createDirectory error falsely locates displaced identity: %v", err)
	}
}

func TestRemoveOpenedEmptyKeepsDeletionBoundToOpenedIdentity(t *testing.T) {
	ownedPath := filepath.Join(t.TempDir(), "owned")
	directory, err := CreateDirectory(ownedPath)
	if err != nil {
		t.Fatal(err)
	}
	displaced := ownedPath + ".displaced"
	outside := t.TempDir()
	marker := filepath.Join(outside, "replacement.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, removeErr := directory.RemoveOpenedEmpty(func() {
		if err := os.Rename(ownedPath, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, ownedPath); err != nil {
			t.Fatal(err)
		}
	})
	closeErr := directory.Close()
	if err := errors.Join(removeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if removed != CanRemoveOpenedDirectory() {
		t.Fatalf("removed = %v, capability = %v", removed, CanRemoveOpenedDirectory())
	}
	if info, err := os.Lstat(ownedPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement = %v, %v, want symlink", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(ownedPath, "replacement.txt")); err != nil || string(got) != "outside" {
		t.Fatalf("replacement marker = %q, %v", got, err)
	}
	if CanRemoveOpenedDirectory() {
		if _, err := os.Stat(displaced); !os.IsNotExist(err) {
			t.Fatalf("disposed identity remains: %v", err)
		}
	} else if entries, err := os.ReadDir(displaced); err != nil || len(entries) != 0 {
		t.Fatalf("retained identity = %v, %v, want empty", entries, err)
	}
}

func TestRemoveOpenedEmptyRejectsNonemptyDirectoryBeforeBarrier(t *testing.T) {
	directory, err := CreateDirectory(filepath.Join(t.TempDir(), "owned"))
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	file, err := directory.OpenFile("marker", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	barrierCalled := false
	if removed, err := directory.RemoveOpenedEmpty(func() { barrierCalled = true }); err == nil || removed {
		t.Fatalf("RemoveOpenedEmpty() = %v, %v, want nonempty error", removed, err)
	}
	if barrierCalled {
		t.Fatal("nonempty directory reached disposition barrier")
	}
}
