//go:build linux

package mem

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Read lee la memoria de la GPU desde sysfs (driver amdgpu).
func Read() (Snapshot, error) {
	device, dir, total := findDevice()
	if total <= 0 {
		return Snapshot{}, ErrUnsupported
	}

	used := readInt(dir, "mem_info_vram_used")
	return Snapshot{
		Supported: true,
		Device:    device,
		Total:     total,
		Used:      used,
		Free:      total - used,
		GTTTotal:  readInt(dir, "mem_info_gtt_total"),
		GTTUsed:   readInt(dir, "mem_info_gtt_used"),
	}, nil
}

// findDevice elige la tarjeta con más VRAM: en un equipo con iGPU y dGPU
// interesa la que realmente puede cargar modelos.
func findDevice() (name, dir string, total int64) {
	paths, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device/mem_info_vram_total")
	for _, path := range paths {
		candidate := filepath.Dir(path)
		if v := readInt(candidate, "mem_info_vram_total"); v > total {
			total = v
			dir = candidate
			name = filepath.Base(filepath.Dir(candidate))
		}
	}
	return name, dir, total
}

func readInt(dir, file string) int64 {
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
