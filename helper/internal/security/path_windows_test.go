//go:build windows

package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWithinWindowsDrivePath(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "audio", "song.mp3")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveWithin(root, candidate)
	if err != nil || got != candidate {
		t.Fatalf("expected Windows drive path %q, got %q, %v", candidate, got, err)
	}
}
