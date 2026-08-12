//go:build !windows

package knownfolder

import (
	"fmt"
	"os"
	"path/filepath"
)

func Music() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Music"), nil
}
