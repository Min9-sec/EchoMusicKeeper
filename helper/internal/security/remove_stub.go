//go:build !windows

package security

import "os"

func CanRemoveOpenedDirectory() bool { return false }

func removeOpenedDirectory(_ *os.Root, afterCheck func()) (bool, error) {
	if afterCheck != nil {
		afterCheck()
	}
	return false, nil
}
