package cache

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func normalizeEntry(key string, entry Entry) (Entry, error) {
	hash, quality, effect, err := parseCacheKey(key)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid cache entry key %q: %w", key, err)
	}
	for field, values := range map[string][2]string{
		"track key": {entry.Track.Key, key}, "track hash": {entry.Track.Hash, hash},
		"quality": {entry.Track.Quality, quality}, "effect": {entry.Track.Effect, effect},
	} {
		if values[0] != "" && values[0] != values[1] {
			return Entry{}, fmt.Errorf("cache entry %s does not match key %q", field, key)
		}
	}
	entry.Track.Key = key
	entry.Track.Hash = hash
	entry.Track.Quality = quality
	entry.Track.Effect = effect
	directory, basename, err := parseCacheRelativePath(entry.RelativePath)
	if err != nil {
		return Entry{}, err
	}
	if entry.Complete {
		if directory != "audio" {
			return Entry{}, fmt.Errorf("complete cache entry must be stored in audio")
		}
		pathKey, extension, err := parseAudioBasename(basename)
		if err != nil || pathKey != key || (entry.Track.Extension != "" && entry.Track.Extension != extension) {
			return Entry{}, fmt.Errorf("complete cache path does not match entry key and extension")
		}
		entry.Track.Extension = extension
	} else if directory != "temp" || basename != key+".part" {
		return Entry{}, fmt.Errorf("partial cache path does not match entry key")
	}
	if entry.Size < 0 || entry.TotalBytes < 0 {
		return Entry{}, fmt.Errorf("cache sizes cannot be negative")
	}
	if entry.Track.CatalogHash != "" && entry.Track.RequestedQuality != "" {
		wantAlias, err := normalizedAliasKey(entry.Track.CatalogHash, entry.Track.RequestedQuality, entry.Track.Effect)
		if err != nil || (entry.AliasKey != "" && entry.AliasKey != wantAlias) {
			return Entry{}, fmt.Errorf("invalid cache alias key")
		}
		entry.AliasKey = wantAlias
	} else if entry.AliasKey != "" {
		return Entry{}, fmt.Errorf("cache alias metadata is incomplete")
	}
	return entry, nil
}

func parseCacheRelativePath(relativePath string) (string, string, error) {
	if relativePath == "" || strings.Contains(relativePath, "\\") || path.IsAbs(relativePath) || path.Clean(relativePath) != relativePath {
		return "", "", fmt.Errorf("invalid cache relative path %q", relativePath)
	}
	parts := strings.Split(relativePath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid cache relative path %q", relativePath)
	}
	switch parts[0] {
	case "audio":
		if _, _, err := parseAudioBasename(parts[1]); err != nil {
			return "", "", err
		}
	case "temp":
		if !strings.HasSuffix(parts[1], ".part") {
			return "", "", fmt.Errorf("invalid partial cache filename")
		}
		if _, _, _, err := parseCacheKey(strings.TrimSuffix(parts[1], ".part")); err != nil {
			return "", "", err
		}
	default:
		return "", "", fmt.Errorf("unsupported cache directory %q", parts[0])
	}
	return parts[0], parts[1], nil
}

func parseAudioBasename(basename string) (string, string, error) {
	extensionSeparator := strings.LastIndexByte(basename, '.')
	if extensionSeparator <= 0 || extensionSeparator == len(basename)-1 {
		return "", "", fmt.Errorf("invalid audio cache filename")
	}
	key := basename[:extensionSeparator]
	extension := basename[extensionSeparator+1:]
	if _, _, _, err := parseCacheKey(key); err != nil || !validExtension(extension) {
		return "", "", fmt.Errorf("invalid audio cache filename")
	}
	return key, extension, nil
}

func parseCacheKey(key string) (string, string, string, error) {
	parts := strings.Split(key, ".")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("invalid cache key")
	}
	validated, err := model.CacheKey(parts[0], parts[1], parts[2])
	if err != nil || validated != key {
		return "", "", "", fmt.Errorf("invalid cache key")
	}
	return parts[0], parts[1], parts[2], nil
}

func normalizedAliasKey(catalogHash, requestedQuality, effect string) (string, error) {
	validated, err := model.CacheKey(catalogHash, requestedQuality, effect)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(validated, ".", ":"), nil
}

func validExtension(extension string) bool {
	if extension == "" || len(extension) > 16 {
		return false
	}
	for _, character := range extension {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func cloneEntry(entry Entry) Entry {
	entry.Ranges = append([]model.ByteRange(nil), entry.Ranges...)
	return entry
}

func cloneEntries(entries map[string]Entry) map[string]Entry {
	cloned := make(map[string]Entry, len(entries))
	for key, entry := range entries {
		cloned[key] = cloneEntry(entry)
	}
	return cloned
}

func sortedKeys(entries map[string]Entry) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
