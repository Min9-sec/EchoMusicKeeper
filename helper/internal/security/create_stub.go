//go:build !windows

package security

import (
	"fmt"
	"os"
)

func createChildDirectoryPlatform(parent *os.Root, name string, afterCreate func()) (*os.Root, os.FileInfo, error) {
	if err := parent.Mkdir(name, 0o700); err != nil {
		return nil, nil, err
	}
	if afterCreate != nil {
		afterCreate()
	}
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, unprovenDirectoryOwnership(name, err)
	}
	child, info, err := openExistingChildDirectory(parent, name, before)
	if err != nil {
		return nil, nil, unprovenDirectoryOwnership(name, err)
	}
	entries, err := readRoot(child)
	if err != nil {
		_ = child.Close()
		return nil, nil, unprovenDirectoryOwnership(name, err)
	}
	if len(entries) != 0 {
		_ = child.Close()
		return nil, nil, unprovenDirectoryOwnership(name, fmt.Errorf("candidate directory is not empty"))
	}
	return child, info, nil
}
