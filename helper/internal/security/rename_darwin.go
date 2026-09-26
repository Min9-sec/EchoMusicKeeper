//go:build darwin

package security

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameBetweenRoots renames a single component below two already verified
// directory identities. Darwin does not export renameat through the standard
// syscall package, so the equivalent x/sys wrapper is used instead.
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
	return unix.Renameat(int(oldParent.Fd()), oldName, int(newParent.Fd()), newName)
}
