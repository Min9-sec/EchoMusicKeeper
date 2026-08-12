package export

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func TestBuildFilename(t *testing.T) {
	tests := []struct {
		name  string
		track model.TrackInfo
		want  string
	}{
		{
			name:  "ordinary",
			track: model.TrackInfo{Artist: "Artist", Title: "Title", Quality: "320", Extension: "mp3"},
			want:  "Artist - Title [320].mp3",
		},
		{
			name:  "illegal characters",
			track: model.TrackInfo{Artist: `A/B`, Title: `T:itle?`, Quality: "flac", Extension: "flac"},
			want:  "A_B - T_itle_ [flac].flac",
		},
		{
			name:  "reserved device component",
			track: model.TrackInfo{Artist: "CON", Title: "Title", Quality: "320", Extension: "mp3"},
			want:  "_CON - Title [320].mp3",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := BuildFilename(test.track); got != test.want {
				t.Fatalf("BuildFilename() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCopyAtomicPreservesSourceAndAddsStableCollisionSuffix(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "source.mp3")
	contents := []byte("cache bytes")
	if err := os.WriteFile(source, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	track := model.TrackInfo{
		Hash: "a1b2c3d4e5f60708", Artist: "Artist", Title: "Title", Quality: "320", Extension: "mp3",
	}
	base := filepath.Join(root, "Artist - Title [320].mp3")
	if err := os.WriteFile(base, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := CopyAtomic(source, root, track)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "Artist - Title [320] [a1b2c3d4].mp3")
	if got != want {
		t.Fatalf("CopyAtomic() = %q, want %q", got, want)
	}
	if copied, err := os.ReadFile(got); err != nil || string(copied) != string(contents) {
		t.Fatalf("copied bytes = %q, %v", copied, err)
	}
	if cached, err := os.ReadFile(source); err != nil || string(cached) != string(contents) {
		t.Fatalf("source bytes = %q, %v", cached, err)
	}
	if matches, err := filepath.Glob(filepath.Join(root, "*.echodownload.part")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, %v", matches, err)
	}
}

func TestCopyAtomicContextCancellationCleansDestination(t *testing.T) {
	root := t.TempDir()
	track := model.TrackInfo{
		Hash: "a1b2c3d4e5f60708", Artist: "Artist", Title: "Title", Quality: "320", Extension: "mp3",
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader := &blockingExportReader{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := copyAtomicFrom(ctx, reader, 1024, root, track)
		done <- err
	}()
	<-reader.started
	cancel()
	close(reader.release)
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyAtomicFrom() error = %v, want context.Canceled", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("destination contains files after cancellation: %v", entries)
	}
}

func TestCopyAtomicRejectsDownloadRootReplacementAndCleansAnchoredTemp(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "music")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	track := model.TrackInfo{
		Hash: "a1b2c3d4e5f60708", Artist: "Artist", Title: "Title", Quality: "320", Extension: "mp3",
	}
	reader := &blockingExportReader{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := copyAtomicFrom(context.Background(), reader, 1024, root, track)
		done <- err
	}()
	<-reader.started
	displaced := root + ".displaced"
	if err := os.Rename(root, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	close(reader.release)
	if err := <-done; err == nil {
		t.Fatal("copy succeeded after the download root was replaced")
	}
	for _, directory := range []string{displaced, outside} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("replacement test left files in %q: %v", directory, entries)
		}
	}
}

type blockingExportReader struct {
	started chan struct{}
	release chan struct{}
	once    bool
}

func (reader *blockingExportReader) Read(buffer []byte) (int, error) {
	if !reader.once {
		reader.once = true
		close(reader.started)
		<-reader.release
		for index := range min(len(buffer), 1024) {
			buffer[index] = 'x'
		}
		return min(len(buffer), 1024), nil
	}
	return 0, io.EOF
}
