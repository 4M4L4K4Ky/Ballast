// Package pull gestiona las descargas de modelos desde Ollama.
//
// Ollama responde a /api/pull con NDJSON, una línea por avance. Aquí se
// agrega el progreso de todas las capas (digest) en un único porcentaje y se
// publica como SSE para que la interfaz no tenga que entender el formato.
package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound indica que la descarga no existe o ya se ha purgado.
var ErrNotFound = errors.New("descarga no encontrada")

// validName acepta nombre, nombre:tag y rutas de registro (usuario/modelo:tag).
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*(:[a-zA-Z0-9._-]+)?$`)

// Event es el estado de una descarga en un instante.
type Event struct {
	Status    string  `json:"status"`
	Digest    string  `json:"digest,omitempty"`
	Completed int64   `json:"completed"`
	Total     int64   `json:"total"`
	Percent   float64 `json:"percent"`
	Done      bool    `json:"done"`
	Error     string  `json:"error,omitempty"`
}

// Snapshot es la vista pública de una descarga.
type Snapshot struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Completed int64      `json:"completed"`
	Total     int64      `json:"total"`
	Percent   float64    `json:"percent"`
	Done      bool       `json:"done"`
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Version   uint64     `json:"-"`
}

// Pull es una descarga en curso o terminada.
type Pull struct {
	ID        string
	Name      string
	StartedAt time.Time

	cancel    context.CancelFunc
	cancelled bool

	mu       sync.Mutex
	event    Event
	version  uint64
	done     bool
	endedAt  time.Time
	notify   chan struct{}
	progress map[string]layer
}

type layer struct {
	Completed int64
	Total     int64
}

// Manager mantiene las descargas activas y las recientes.
type Manager struct {
	target *url.URL
	client *http.Client

	mu    sync.Mutex
	pulls map[string]*Pull
	seq   uint64
}

// New crea el gestor. El cliente no lleva timeout global: una descarga puede
// durar minutos u horas.
func New(target *url.URL) *Manager {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: time.Minute,
	}
	return &Manager{
		target: target,
		client: &http.Client{Transport: transport},
		pulls:  make(map[string]*Pull),
	}
}

// Start lanza una descarga y devuelve su identificador.
func (m *Manager) Start(name string) (Snapshot, error) {
	name = strings.TrimSpace(name)
	if !validName.MatchString(name) || strings.Contains(name, "..") {
		return Snapshot{}, fmt.Errorf("nombre de modelo inválido: %q", name)
	}

	m.mu.Lock()
	m.prune()
	for _, existing := range m.pulls {
		if existing.Name == name && !existing.finished() {
			snapshot := existing.Snapshot()
			m.mu.Unlock()
			return snapshot, fmt.Errorf("%s ya se está descargando", name)
		}
	}
	m.seq++
	id := fmt.Sprintf("p%d-%d", time.Now().UnixMilli(), m.seq)

	ctx, cancel := context.WithCancel(context.Background())
	p := &Pull{
		ID:        id,
		Name:      name,
		StartedAt: time.Now(),
		cancel:    cancel,
		event:     Event{Status: "empezando"},
		version:   1,
		notify:    make(chan struct{}, 1),
		progress:  make(map[string]layer),
	}
	m.pulls[id] = p
	m.mu.Unlock()

	go m.run(ctx, p)
	return p.Snapshot(), nil
}

// Cancel pide a Ollama que deje de descargar. Si ya terminó, no hace nada.
func (m *Manager) Cancel(id string) error {
	p, ok := m.get(id)
	if !ok {
		return ErrNotFound
	}
	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		return fmt.Errorf("la descarga ya ha terminado")
	}
	p.cancelled = true
	p.mu.Unlock()
	p.cancel()
	return nil
}

// List devuelve todas las descargas, las activas primero y las más recientes
// dentro de cada grupo.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	m.prune()
	out := make([]Snapshot, 0, len(m.pulls))
	for _, p := range m.pulls {
		out = append(out, p.Snapshot())
	}
	m.mu.Unlock()

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Done != out[j].Done {
			return !out[i].Done
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// prune olvida las descargas terminadas hace más de 10 minutos. Se llama con
// m.mu tomado.
func (m *Manager) prune() {
	for id, p := range m.pulls {
		p.mu.Lock()
		stale := p.done && time.Since(p.endedAt) > 10*time.Minute
		p.mu.Unlock()
		if stale {
			delete(m.pulls, id)
		}
	}
}

// Get devuelve una descarga concreta.
func (m *Manager) Get(id string) (Snapshot, bool) {
	p, ok := m.get(id)
	if !ok {
		return Snapshot{}, false
	}
	return p.Snapshot(), true
}

// Subscribe devuelve la versión actual y un canal que se cierra al cambiar.
func (m *Manager) Subscribe(id string) (Snapshot, <-chan struct{}, error) {
	p, ok := m.get(id)
	if !ok {
		return Snapshot{}, nil, ErrNotFound
	}
	return p.Snapshot(), p.notify, nil
}

func (m *Manager) get(id string) (*Pull, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pulls[id]
	return p, ok
}

// Snapshot copia el estado actual de la descarga.
func (p *Pull) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := Snapshot{
		ID:        p.ID,
		Name:      p.Name,
		Status:    p.event.Status,
		Completed: p.event.Completed,
		Total:     p.event.Total,
		Percent:   p.event.Percent,
		Done:      p.done,
		Error:     p.event.Error,
		StartedAt: p.StartedAt,
		Version:   p.version,
	}
	if !p.endedAt.IsZero() {
		ended := p.endedAt
		snapshot.EndedAt = &ended
	}
	return snapshot
}

func (p *Pull) finished() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// update publica un estado nuevo y despierta a quien esté escuchando.
func (p *Pull) update(event Event) {
	p.mu.Lock()
	p.event = event
	p.version++
	p.mu.Unlock()

	select {
	case p.notify <- struct{}{}:
	default: // ya había un aviso pendiente
	}
}

func (p *Pull) finish(event Event) {
	p.mu.Lock()
	p.event = event
	p.version++
	p.done = true
	p.endedAt = time.Now()
	p.mu.Unlock()

	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// run hace la petición a Ollama y traduce el NDJSON a eventos.
func (m *Manager) run(ctx context.Context, p *Pull) {
	err := m.stream(ctx, p)
	if err == nil {
		return
	}

	p.mu.Lock()
	cancelled := p.cancelled
	p.mu.Unlock()

	event := p.event
	event.Done = true
	if cancelled || errors.Is(err, context.Canceled) {
		event.Status = "cancelada"
		event.Error = ""
		log.Printf("descarga cancelada: %s", p.Name)
	} else {
		event.Status = "error"
		event.Error = err.Error()
		log.Printf("descarga fallida (%s): %v", p.Name, err)
	}
	p.finish(event)
}

func (m *Manager) stream(ctx context.Context, p *Pull) error {
	body, err := json.Marshal(map[string]any{"model": p.Name, "stream": true})
	if err != nil {
		return err
	}
	endpoint := *m.target
	endpoint.Path = "/api/pull"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Ollama respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var line struct {
			Status    string `json:"status"`
			Digest    string `json:"digest"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if line.Error != "" {
			return errors.New(line.Error)
		}

		event := Event{Status: line.Status, Digest: line.Digest}
		if line.Digest != "" && line.Total > 0 {
			p.mu.Lock()
			p.progress[line.Digest] = layer{Completed: line.Completed, Total: line.Total}
			var completed, total int64
			for _, l := range p.progress {
				completed += l.Completed
				total += l.Total
			}
			p.mu.Unlock()

			event.Completed, event.Total = completed, total
			if total > 0 {
				event.Percent = float64(completed) / float64(total) * 100
			}
		}

		if line.Status == "success" {
			event.Status = "completada"
			event.Percent = 100
			p.finish(event)
			log.Printf("descarga completada: %s", p.Name)
			return nil
		}
		p.update(event)
	}

	// Sin línea "success" pero sin error: Ollama cerró el flujo.
	event := p.event
	event.Status = "completada"
	event.Percent = 100
	p.finish(event)
	return nil
}
