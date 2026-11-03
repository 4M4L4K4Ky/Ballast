//go:build linux

package disk

import (
	"bufio"
	"os"
	"strings"
	"syscall"
)

// list lee /proc/self/mounts y descarta los sistemas de archivos virtuales.
func list() ([]Filesystem, error) {
	file, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var out []Filesystem
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		device, mount, fstype := fields[0], unescape(fields[1]), fields[2]
		if !isReal(fstype) || seen[mount] {
			continue
		}
		seen[mount] = true

		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			continue
		}
		total := int64(st.Blocks) * int64(st.Bsize)
		free := int64(st.Bavail) * int64(st.Bsize)
		if total <= 0 {
			continue
		}
		out = append(out, newFilesystem(mount, device, fstype, total, free))
	}
	return out, scanner.Err()
}

// isReal descarta tmpfs, overlay, snapshots y demás ruido de /proc/self/mounts.
func isReal(fstype string) bool {
	switch fstype {
	case "tmpfs", "devtmpfs", "devpts", "proc", "sysfs", "cgroup", "cgroup2",
		"overlay", "squashfs", "nsfs", "ramfs", "debugfs", "tracefs", "securityfs",
		"pstore", "bpf", "configfs", "mqueue", "hugetlbfs", "autofs", "efivarfs",
		"binfmt_misc", "rpc_pipefs", "fusectl", "selinuxfs", "nfsd", "fuse.gvfsd-fuse":
		return false
	}
	return !strings.HasPrefix(fstype, "fuse.")
}

// unescape convierte los \040, \011 y \134 de /proc/self/mounts.
func unescape(s string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(s)
}
