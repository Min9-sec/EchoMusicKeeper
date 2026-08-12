package knownfolder

import (
	"path/filepath"
	"testing"
)

func TestMusicReturnsMusicDirectory(t *testing.T) {
	root, err := Music()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(root) != "Music" {
		t.Fatalf("Music() = %q, want a Music directory", root)
	}
}
