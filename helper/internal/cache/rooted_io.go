package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

type rootPathLock interface {
	Close() error
}

// openOrCreateRoot walks from the volume root without following path links.
func openOrCreateRoot(rootPath string) (*os.Root, string, error) {
	return openRootPath(rootPath, true)
}

func openExistingRoot(rootPath string) (*os.Root, string, error) {
	return openRootPath(rootPath, false)
}

func openRootPath(rootPath string, create bool) (*os.Root, string, error) {
	absolute, err := filepath.Abs(filepath.Clean(rootPath))
	if err != nil {
		return nil, "", fmt.Errorf("resolve cache root: %w", err)
	}
	volume := filepath.VolumeName(absolute)
	anchor := volume + string(filepath.Separator)
	if volume == "" {
		anchor = string(filepath.Separator)
	}
	current, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, "", fmt.Errorf("open cache root anchor: %w", err)
	}
	remainder := strings.TrimPrefix(absolute, anchor)
	if remainder == absolute {
		_ = current.Close()
		return nil, "", fmt.Errorf("cache root is not an absolute path")
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		next, _, err := security.OpenChildDirectory(current, component, create)
		if err != nil {
			_ = current.Close()
			return nil, "", fmt.Errorf("open cache root component %q: %w", component, err)
		}
		_ = current.Close()
		current = next
	}
	return current, absolute, nil
}

func verifyRootPath(root *os.Root, rootPath string) error {
	pathInfo, err := os.Lstat(rootPath)
	if err != nil {
		return fmt.Errorf("inspect locked cache root: %w", err)
	}
	if !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("locked cache root is not a real directory")
	}
	opened, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("inspect opened cache root: %w", err)
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil {
		return fmt.Errorf("stat opened cache root: %w", err)
	}
	if !os.SameFile(pathInfo, openedInfo) {
		return fmt.Errorf("cache root path changed while it was locked")
	}
	return nil
}
