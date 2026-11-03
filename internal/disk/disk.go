// Package disk mide el espacio real de los discos, sin dependencias externas.
//
// Cada plataforma implementa list(); este archivo solo tiene los tipos y la
// lógica común: localizar el directorio de modelos y marcar su sistema de
// archivos.
package disk

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Filesystem es una fila del panel de almacenamiento.
type Filesystem struct {
	Mount          string  `json:"mount"`
	Device         string  `json:"device"`
	FSType         string  `json:"fstype"`
	TotalBytes     int64   `json:"total_bytes"`
	FreeBytes      int64   `json:"free_bytes"`
	UsedBytes      int64   `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
	ContainsModels bool    `json:"contains_models"`
}

// Report es la respuesta de GET /api/v1/disk.
type Report struct {
	ModelsDir       string       `json:"models_dir"`
	ModelsDirExists bool         `json:"models_dir_exists"`
	Remote          bool         `json:"remote"`
	Filesystems     []Filesystem `json:"filesystems"`
}

// ModelsDir devuelve dónde viven los modelos de Ollama.
// Orden: BALLAST_MODELS_DIR, OLLAMA_MODELS, ~/.ollama/models.
func ModelsDir() (string, bool) {
	if v := os.Getenv("BALLAST_MODELS_DIR"); v != "" {
		return v, isDir(v)
	}
	if v := os.Getenv("OLLAMA_MODELS"); v != "" {
		return v, isDir(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	models := filepath.Join(home, ".ollama", "models")
	if isDir(models) {
		return models, true
	}
	return home, false
}

// List enumera los sistemas de archivos reales y marca el de los modelos.
func List(modelsDir string) ([]Filesystem, error) {
	filesystems, err := list()
	if err != nil {
		return nil, err
	}

	target := realPath(modelsDir)
	best, bestLen := -1, -1
	for i, fs := range filesystems {
		mount := realPath(fs.Mount)
		rel, err := filepath.Rel(mount, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		// Si hay varios montajes anidados gana el más específico.
		if len(mount) > bestLen {
			best, bestLen = i, len(mount)
		}
	}
	if best >= 0 {
		filesystems[best].ContainsModels = true
	}

	sort.SliceStable(filesystems, func(i, j int) bool {
		if filesystems[i].ContainsModels != filesystems[j].ContainsModels {
			return filesystems[i].ContainsModels
		}
		return filesystems[i].Mount < filesystems[j].Mount
	})
	return filesystems, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// newFilesystem rellena los campos derivados de un sistema de archivos.
func newFilesystem(mount, device, fstype string, total, free int64) Filesystem {
	if free < 0 {
		free = 0
	}
	used := total - free
	percent := 0.0
	if total > 0 {
		percent = float64(used) / float64(total) * 100
	}
	return Filesystem{
		Mount:       mount,
		Device:      device,
		FSType:      fstype,
		TotalBytes:  total,
		FreeBytes:   free,
		UsedBytes:   used,
		UsedPercent: percent,
	}
}

// realPath resuelve enlaces simbólicos; si falla, limpia la ruta.
func realPath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}
