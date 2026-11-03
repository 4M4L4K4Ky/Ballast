//go:build windows

package disk

import (
	"syscall"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	getLogicalDrives    = kernel32.NewProc("GetLogicalDrives")
	getDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
	getDriveTypeW       = kernel32.NewProc("GetDriveTypeW")
)

const (
	driveFixed     = 3
	driveRemovable = 4
)

// list enumera solo las unidades fijas y extraíbles.
func list() ([]Filesystem, error) {
	mask, _, _ := getLogicalDrives.Call()
	var out []Filesystem
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		ptr, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		kind, _, _ := getDriveTypeW.Call(uintptr(unsafe.Pointer(ptr)))
		if kind != driveFixed && kind != driveRemovable {
			continue
		}

		var freeAvail, total, totalFree uint64
		ok, _, _ := getDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(ptr)),
			uintptr(unsafe.Pointer(&freeAvail)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&totalFree)),
		)
		if ok == 0 || total == 0 {
			continue
		}
		out = append(out, newFilesystem(root, root, "", int64(total), int64(freeAvail)))
	}
	return out, nil
}
