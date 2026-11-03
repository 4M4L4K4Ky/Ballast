//go:build darwin

package disk

import (
	"os"
	"path/filepath"
	"syscall"
)

// list consulta / y cada volumen montado en /Volumes.
func list() ([]Filesystem, error) {
	mounts := []string{"/"}
	if entries, err := os.ReadDir("/Volumes"); err == nil {
		for _, entry := range entries {
			mounts = append(mounts, filepath.Join("/Volumes", entry.Name()))
		}
	}

	var out []Filesystem
	for _, mount := range mounts {
		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			continue
		}
		total := int64(st.Blocks) * int64(st.Bsize)
		free := int64(st.Bavail) * int64(st.Bsize)
		if total <= 0 {
			continue
		}
		out = append(out, newFilesystem(mount, "", "", total, free))
	}
	return out, nil
}
