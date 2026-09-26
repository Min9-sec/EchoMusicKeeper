//go:build !windows && !darwin

package security

import (
	"os"
	"syscall"
)

func renameBetweenRoots(oldRoot *os.Root, oldName string, newRoot *os.Root, newName string) error {
	oldParent, err := oldRoot.Open(".")
	if err != nil {
		return err
	}
	defer oldParent.Close()
	newParent, err := newRoot.Open(".")
	if err != nil {
		return err
	}
	defer newParent.Close()
	return syscall.Renameat(int(oldParent.Fd()), oldName, int(newParent.Fd()), newName)
}
