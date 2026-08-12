//go:build windows

package knownfolder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	shell32              = syscall.NewLazyDLL("shell32.dll")
	ole32                = syscall.NewLazyDLL("ole32.dll")
	shGetKnownFolderPath = shell32.NewProc("SHGetKnownFolderPath")
	coTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	folderIDMusic        = guid{0x4BD8D571, 0x6D19, 0x48D3, [8]byte{0xBE, 0x97, 0x42, 0x22, 0x20, 0x08, 0x0E, 0x43}}
)

type guid struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

func Music() (string, error) {
	var value *uint16
	result, _, callErr := shGetKnownFolderPath.Call(
		uintptr(unsafe.Pointer(&folderIDMusic)),
		0,
		0,
		uintptr(unsafe.Pointer(&value)),
	)
	if int32(result) >= 0 && value != nil {
		defer coTaskMemFree.Call(uintptr(unsafe.Pointer(value)))
		if path := utf16PointerString(value); path != "" {
			return path, nil
		}
	}
	if profile := strings.TrimSpace(os.Getenv("USERPROFILE")); profile != "" {
		return filepath.Join(profile, "Music"), nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Music"), nil
	}
	if callErr != nil && callErr != syscall.Errno(0) {
		return "", fmt.Errorf("resolve Windows Music known folder: %w", callErr)
	}
	return "", fmt.Errorf("resolve Windows Music known folder: HRESULT 0x%08x", uint32(result))
}

func utf16PointerString(value *uint16) string {
	units := make([]uint16, 0, 260)
	for offset := uintptr(0); ; offset += unsafe.Sizeof(*value) {
		unit := *(*uint16)(unsafe.Add(unsafe.Pointer(value), offset))
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	return syscall.UTF16ToString(units)
}
