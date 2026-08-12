package export

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

const temporarySuffix = ".echodownload.part"

var destinationMu sync.Mutex

// BuildFilename returns a Windows-safe display name for a downloaded track.
func BuildFilename(track model.TrackInfo) string {
	artist := security.SanitizeWindowsName(track.Artist)
	if strings.TrimSpace(track.Artist) == "" {
		artist = "Unknown Artist"
	}
	title := security.SanitizeWindowsName(track.Title)
	if strings.TrimSpace(track.Title) == "" {
		title = "Untitled"
	}
	quality := security.SanitizeWindowsName(track.Quality)
	if strings.TrimSpace(track.Quality) == "" {
		quality = "unknown"
	}
	extension := normalizedExtension(track.Extension)
	stem := security.SanitizeWindowsName(fmt.Sprintf("%s - %s [%s]", artist, title, quality))
	return stem + "." + extension
}

// CopyAtomic copies source into root without removing or modifying source.
func CopyAtomic(source, root string, track model.TrackInfo) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open export source: %w", err)
	}
	info, statErr := input.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = input.Close()
		if statErr != nil {
			return "", fmt.Errorf("inspect export source: %w", statErr)
		}
		return "", fmt.Errorf("export source is not a regular file")
	}
	path, copyErr := CopyAtomicFrom(context.Background(), input, info.Size(), root, track)
	return path, errors.Join(copyErr, input.Close())
}

// CopyAtomicFrom exports a rooted or otherwise pre-validated source stream.
func CopyAtomicFrom(ctx context.Context, source io.Reader, sourceBytes int64, root string, track model.TrackInfo) (string, error) {
	return copyAtomicFrom(ctx, source, sourceBytes, root, track)
}

func copyAtomicFrom(ctx context.Context, source io.Reader, sourceBytes int64, root string, track model.TrackInfo) (destination string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if source == nil || sourceBytes < 0 {
		return "", fmt.Errorf("valid export source and byte count are required")
	}
	directory, err := security.OpenDirectory(root, true)
	if err != nil {
		return "", fmt.Errorf("open download root: %w", err)
	}
	name, copyErr := CopyAtomicTo(ctx, source, sourceBytes, directory, track)
	path := filepath.Join(directory.Path(), name)
	closeErr := directory.Close()
	return path, errors.Join(copyErr, closeErr)
}

// CopyAtomicTo exports into an already-opened destination capability and
// returns the relative filename. The caller retains ownership of directory.
func CopyAtomicTo(ctx context.Context, source io.Reader, sourceBytes int64, directory *security.Directory, track model.TrackInfo) (destination string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if source == nil || sourceBytes < 0 || directory == nil {
		return "", fmt.Errorf("valid export source, byte count, and destination are required")
	}

	destinationMu.Lock()
	defer destinationMu.Unlock()
	destinationName, temporaryName, err := selectDestination(directory, track)
	if err != nil {
		return "", err
	}
	output, err := directory.OpenFile(temporaryName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create temporary download: %w", err)
	}
	defer func() {
		if resultErr != nil {
			_ = output.Close()
			_ = directory.Cleanup(temporaryName)
		}
	}()

	written, err := io.CopyBuffer(output, contextReader{ctx: ctx, reader: source}, make([]byte, 64*1024))
	if err != nil {
		return "", fmt.Errorf("copy download: %w", err)
	}
	if written != sourceBytes {
		return "", fmt.Errorf("copied byte count %d does not match source size %d", written, sourceBytes)
	}
	if err := output.Sync(); err != nil {
		return "", fmt.Errorf("sync temporary download: %w", err)
	}
	if err := output.Close(); err != nil {
		return "", fmt.Errorf("close temporary download: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := directory.Rename(temporaryName, destinationName); err != nil {
		return "", fmt.Errorf("publish download: %w", err)
	}
	return destinationName, nil
}

func selectDestination(root *security.Directory, track model.TrackInfo) (string, string, error) {
	filename := BuildFilename(track)
	extension := path.Ext(filename)
	stem := strings.TrimSuffix(filename, extension)
	collision := collisionID(track)
	for attempt := 0; attempt < 1000; attempt++ {
		candidateStem := stem
		if attempt == 1 {
			candidateStem += " [" + collision + "]"
		} else if attempt > 1 {
			candidateStem += fmt.Sprintf(" [%s-%d]", collision, attempt)
		}
		candidate := candidateStem + extension
		temporary := candidate + temporarySuffix
		if pathAvailable(root, candidate) && pathAvailable(root, temporary) {
			return candidate, temporary, nil
		}
	}
	return "", "", fmt.Errorf("no collision-free download filename is available")
}

func pathAvailable(root *security.Directory, path string) bool {
	_, err := root.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func collisionID(track model.TrackInfo) string {
	value := strings.ToLower(strings.TrimSpace(track.Hash))
	if len(value) >= 8 {
		return value[:8]
	}
	sum := sha256.Sum256([]byte(track.Key + "\x00" + track.Artist + "\x00" + track.Title))
	return hex.EncodeToString(sum[:4])
}

func normalizedExtension(extension string) string {
	extension = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(extension), "."))
	if extension == "" || len(extension) > 16 {
		return "mp3"
	}
	for _, character := range extension {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return "mp3"
		}
	}
	return extension
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
