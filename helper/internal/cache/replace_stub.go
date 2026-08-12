//go:build !windows

package cache

import (
	"os"
	"syscall"
)

type unlockedRootPath struct{}

func (unlockedRootPath) Close() error { return nil }

func lockRootPath(string) (rootPathLock, error) {
	return unlockedRootPath{}, nil
}

func replaceFile(root *os.Root, _ string, source, destination string) error {
	return root.Rename(source, destination)
}

func openRootFile(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	return root.OpenFile(name, flag|syscall.O_NOFOLLOW, perm)
}
