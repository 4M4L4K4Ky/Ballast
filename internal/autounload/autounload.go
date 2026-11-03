// Package autounload aplica la política de expulsión automática.
//
// Respeta las seis condiciones de SPEC.md §6: activada, N minutos sin tráfico,
// cero peticiones en vuelo, no fijado, Ollama confirma que sigue cargado y el
// proxy ve el tráfico. Ante la duda, no expulsa.
package autounload

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/4M4L4K4Ky/Ballast/internal/store"
)

const (
	// Cycle es la cadencia de comprobación y también el tiempo de espera al
	// arrancar antes de tocar nada.
	Cycle = time.Minute

	defaultIdleMinutes = 30
	minIdleMinutes     = 5
	maxIdleMinutes     = 240

	// uiGrace evita expulsar lo que el usuario acaba de cargar a mano.
	uiGrace = 10 * time.Minute

	keyEnabled = "auto_unload_enabled"
	keyIdle    = "auto_unload_idle_minutes"
	keyPaused  = "auto_unload_paused_until"
	eventType  = "auto-unload"
)

// Config es la política tal como la ve la interfaz.
type Config struct {
	Enabled     bool       `json:"enabled"`
	IdleMinutes int        `json:"idle_minutes"`
	Paused      bool       `json:"paused"`
	PausedUntil *time.Time `json:"paused_until,omitempty"`
}

// Tracker es lo único que hace falta saber del proxy: qué hay en vuelo y
// cuándo se usó cada modelo por última vez.
type Tracker interface {
	InFlight() map[string]int
	LastSeen() map[string]time.Time
}

// Manager guarda la política y la aplica en una goroutine.
type Manager struct {
	store  *store.Store
	proxy  Tracker
	loaded func(context.Context) ([]string, error)
	unload func(context.Context, string) error

	now     func() time.Time
	started time.Time

	mu        sync.Mutex
	firstSeen map[string]time.Time
	uiLoaded  map[string]time.Time

	cfgMu sync.RWMutex
	cfg   Config
}

// New crea el gestor. loaded devuelve los modelos cargados según Ollama y
// unload expulsa uno.
func New(st *store.Store, px Tracker, loaded func(context.Context) ([]string, error), unload func(context.Context, string) error) *Manager {
	m := &Manager{
		store:     st,
		proxy:     px,
		loaded:    loaded,
		unload:    unload,
		now:       time.Now,
		firstSeen: make(map[string]time.Time),
		uiLoaded:  make(map[string]time.Time),
		cfg:       Config{Enabled: false, IdleMinutes: defaultIdleMinutes},
	}
	m.started = m.now()
	return m
}

// Load lee la política guardada en SQLite.
func (m *Manager) Load(ctx context.Context) {
	cfg := Config{IdleMinutes: defaultIdleMinutes}

	if raw, ok, err := m.store.Setting(ctx, keyEnabled); err == nil && ok {
		cfg.Enabled, _ = strconv.ParseBool(raw)
	}
	if raw, ok, err := m.store.Setting(ctx, keyIdle); err == nil && ok {
		if n, err := strconv.Atoi(raw); err == nil {
			cfg.IdleMinutes = clampIdle(n)
		}
	}
	if raw, ok, err := m.store.Setting(ctx, keyPaused); err == nil && ok {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
			until := time.UnixMilli(ms)
			if until.After(m.now()) {
				cfg.PausedUntil = &until
			}
		}
	}
	cfg.Paused = cfg.PausedUntil != nil && cfg.PausedUntil.After(m.now())

	m.cfgMu.Lock()
	m.cfg = cfg
	m.cfgMu.Unlock()
}

// Config devuelve una copia de la política actual.
func (m *Manager) Config() Config {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	cfg := m.cfg
	// La pausa caduca sola: se recalcula al leer.
	if cfg.PausedUntil != nil && !cfg.PausedUntil.After(m.now()) {
		cfg.Paused = false
		cfg.PausedUntil = nil
	}
	return cfg
}

// SetEnabled activa o desactiva la auto-expulsión.
func (m *Manager) SetEnabled(ctx context.Context, enabled bool) error {
	if err := m.store.SetSetting(ctx, keyEnabled, strconv.FormatBool(enabled)); err != nil {
		return err
	}
	m.cfgMu.Lock()
	m.cfg.Enabled = enabled
	m.cfgMu.Unlock()
	return nil
}

// SetIdleMinutes ajusta el umbral, acotado a 5–240 minutos.
func (m *Manager) SetIdleMinutes(ctx context.Context, minutes int) error {
	minutes = clampIdle(minutes)
	if err := m.store.SetSetting(ctx, keyIdle, strconv.Itoa(minutes)); err != nil {
		return err
	}
	m.cfgMu.Lock()
	m.cfg.IdleMinutes = minutes
	m.cfgMu.Unlock()
	return nil
}

// Pause silencia la política durante d.
func (m *Manager) Pause(ctx context.Context, d time.Duration) error {
	until := m.now().Add(d)
	if err := m.store.SetSetting(ctx, keyPaused, strconv.FormatInt(until.UnixMilli(), 10)); err != nil {
		return err
	}
	m.cfgMu.Lock()
	m.cfg.Paused = true
	m.cfg.PausedUntil = &until
	m.cfgMu.Unlock()
	return nil
}

// Resume quita la pausa.
func (m *Manager) Resume(ctx context.Context) error {
	if err := m.store.SetSetting(ctx, keyPaused, "0"); err != nil {
		return err
	}
	m.cfgMu.Lock()
	m.cfg.Paused = false
	m.cfg.PausedUntil = nil
	m.cfgMu.Unlock()
	return nil
}

// MarkLoaded apunta que el usuario cargó un modelo desde la interfaz: no se
// tocará durante uiGrace.
func (m *Manager) MarkLoaded(model string) {
	m.mu.Lock()
	m.uiLoaded[model] = m.now()
	m.mu.Unlock()
}

// Run comprueba la política cada minuto hasta que se cancele el contexto.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(Cycle)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Evaluate(ctx)
		}
	}
}

// Evaluate aplica la política una vez y devuelve los modelos expulsados.
func (m *Manager) Evaluate(ctx context.Context) []string {
	cfg := m.Config()
	if !cfg.Enabled || cfg.Paused {
		return nil
	}
	// Salvaguarda de arranque: un ciclo entero antes de decidir nada.
	if m.now().Sub(m.started) < Cycle {
		return nil
	}

	loaded, err := m.loaded(ctx)
	if err != nil {
		// Sin confirmación de Ollama no se expulsa nada.
		return nil
	}
	pinned, err := m.store.Pinned(ctx)
	if err != nil {
		return nil
	}

	inflight := m.proxy.InFlight()
	lastSeen := m.proxy.LastSeen()
	now := m.now()

	var unloaded []string
	for _, model := range loaded {
		if pinned[model] || inflight[model] > 0 {
			continue
		}

		last := m.firstSeenOf(model, now)
		if t, ok := lastSeen[model]; ok && t.After(last) {
			last = t
		}
		// Lo que el usuario cargó a mano tiene diez minutos de gracia, aunque
		// el umbral de inactividad sea menor.
		if t, ok := m.uiLoadedOf(model); ok {
			if now.Sub(t) < uiGrace {
				continue
			}
			if t.After(last) {
				last = t
			}
		}

		idle := now.Sub(last)
		if idle < time.Duration(cfg.IdleMinutes)*time.Minute {
			continue
		}

		detail := fmt.Sprintf("idle %s, 0 en vuelo", roundMinutes(idle))
		if err := m.unload(ctx, model); err != nil {
			// Un único reintento; nunca en bucle.
			if err := m.unload(ctx, model); err != nil {
				m.event(ctx, model, detail+"; error: "+err.Error())
				continue
			}
		}

		m.forget(model)
		m.event(ctx, model, detail)
		log.Printf("auto-expulsión: %s (%s)", model, detail)
		unloaded = append(unloaded, model)
	}

	m.prune(loaded)
	return unloaded
}

// Event registra una acción manual para que quede en la auditoría.
func (m *Manager) Event(ctx context.Context, eventType, model, detail string) {
	m.eventType(ctx, eventType, model, detail)
}

func (m *Manager) event(ctx context.Context, model, detail string) {
	m.eventType(ctx, eventType, model, detail)
}

func (m *Manager) eventType(ctx context.Context, eventType, model, detail string) {
	if err := m.store.AddEvent(ctx, eventType, model, detail); err != nil {
		log.Printf("no se pudo registrar el evento %s: %v", eventType, err)
	}
}

func (m *Manager) firstSeenOf(model string, now time.Time) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen, ok := m.firstSeen[model]
	if !ok {
		seen = now
		m.firstSeen[model] = now
	}
	return seen
}

func (m *Manager) uiLoadedOf(model string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.uiLoaded[model]
	return t, ok
}

func (m *Manager) forget(model string) {
	m.mu.Lock()
	delete(m.firstSeen, model)
	delete(m.uiLoaded, model)
	m.mu.Unlock()
}

// prune olvida los modelos que ya no están cargados.
func (m *Manager) prune(loaded []string) {
	live := make(map[string]bool, len(loaded))
	for _, model := range loaded {
		live[model] = true
	}
	m.mu.Lock()
	for model := range m.firstSeen {
		if !live[model] {
			delete(m.firstSeen, model)
		}
	}
	for model := range m.uiLoaded {
		if !live[model] {
			delete(m.uiLoaded, model)
		}
	}
	m.mu.Unlock()
}

func clampIdle(n int) int {
	if n < minIdleMinutes {
		return minIdleMinutes
	}
	if n > maxIdleMinutes {
		return maxIdleMinutes
	}
	return n
}

func roundMinutes(d time.Duration) time.Duration {
	return d.Round(time.Minute)
}
