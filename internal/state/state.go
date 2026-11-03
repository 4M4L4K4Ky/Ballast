// Package state guarda el estado pequeño que debe sobrevivir a un reinicio:
// de momento, el modo juego. Se escribe un JSON mínimo, sin base de datos.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DataDir devuelve el directorio de datos de Ballast.
// Orden: BALLAST_DATA_DIR, XDG_DATA_HOME/ballast, ~/.local/share/ballast.
func DataDir() string {
	if v := os.Getenv("BALLAST_DATA_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "ballast")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ballast"
	}
	return filepath.Join(home, ".local", "share", "ballast")
}

type file struct {
	Enabled bool      `json:"enabled"`
	Since   time.Time `json:"since"`
}

// GameMode es el interruptor que libera la GPU y bloquea cargas.
type GameMode struct {
	mu      sync.RWMutex
	enabled bool
	since   time.Time
	path    string
}

// NewGameMode carga el estado previo de dir/state.json.
func NewGameMode(dir string) *GameMode {
	g := &GameMode{path: filepath.Join(dir, "state.json")}
	raw, err := os.ReadFile(g.path)
	if err != nil {
		return g
	}
	var f file
	if json.Unmarshal(raw, &f) == nil {
		g.enabled = f.Enabled
		g.since = f.Since
	}
	return g
}

// Enabled indica si el modo juego está activo.
func (g *GameMode) Enabled() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.enabled
}

// Since es el momento en que se activó.
func (g *GameMode) Since() time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.since
}

// Set activa o desactiva el modo y lo persiste.
func (g *GameMode) Set(enabled bool) error {
	g.mu.Lock()
	g.enabled = enabled
	if enabled {
		g.since = time.Now()
	} else {
		g.since = time.Time{}
	}
	snapshot := file{Enabled: g.enabled, Since: g.since}
	g.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(g.path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	// Escritura atómica: si se corta la luz no queda un JSON a medias.
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, g.path); err != nil {
		return err
	}
	return nil
}
