// Package mem mide la memoria real de la GPU y guarda una serie temporal.
//
// En Linux la fuente es sysfs (/sys/class/drm/card*/device/mem_info_*), que es
// lo que ve el driver amdgpu. No hay dependencias externas: si el sistema no
// expone esos archivos, Read devuelve Supported=false y la interfaz oculta el
// panel.
package mem

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrUnsupported indica que la plataforma no permite medir la VRAM.
var ErrUnsupported = errors.New("memoria de la GPU no disponible en este sistema")

// Snapshot es la foto instantánea de la memoria de la GPU.
type Snapshot struct {
	Supported  bool   `json:"supported"`
	Device     string `json:"device,omitempty"`
	Total      int64  `json:"total_bytes"`
	Used       int64  `json:"used_bytes"`
	Free       int64  `json:"free_bytes"`
	GTTTotal   int64  `json:"gtt_total_bytes"`
	GTTUsed    int64  `json:"gtt_used_bytes"`
	IntervalMS int64  `json:"interval_ms"`
	WindowSec  int64  `json:"window_seconds"`
}

// Sample es un punto de la serie. T va en milisegundos Unix.
type Sample struct {
	T      int64 `json:"t"`
	VRAM   int64 `json:"vram"`
	GTT    int64 `json:"gtt"`
	Ollama int64 `json:"ollama"`
}

// Sampler guarda los últimos N puntos en un búfer circular protegido.
type Sampler struct {
	mu       sync.RWMutex
	samples  []Sample
	max      int
	interval time.Duration
	snapshot Snapshot
}

// NewSampler crea un muestreador con el intervalo y la ventana indicados.
func NewSampler(interval, window time.Duration) *Sampler {
	if interval <= 0 {
		interval = time.Second
	}
	if window < interval {
		window = interval
	}
	return &Sampler{
		max:      int(window / interval),
		interval: interval,
		snapshot: Snapshot{
			IntervalMS: interval.Milliseconds(),
			WindowSec:  int64(window.Seconds()),
		},
	}
}

// Run muestrea hasta que se cancele el contexto. ollama devuelve la VRAM que
// ocupan los modelos cargados; puede ser nil si no interesa.
func (s *Sampler) Run(ctx context.Context, ollama func(context.Context) int64) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.tick(ctx, ollama)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx, ollama)
		}
	}
}

func (s *Sampler) tick(ctx context.Context, ollama func(context.Context) int64) {
	snap, err := Read()
	if err != nil {
		s.mu.Lock()
		s.snapshot.Supported = false
		s.mu.Unlock()
		return
	}

	var usedByOllama int64
	if ollama != nil {
		usedByOllama = ollama(ctx)
	}

	sample := Sample{
		T:      time.Now().UnixMilli(),
		VRAM:   snap.Used,
		GTT:    snap.GTTUsed,
		Ollama: usedByOllama,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = Snapshot{
		Supported:  snap.Supported,
		Device:     snap.Device,
		Total:      snap.Total,
		Used:       snap.Used,
		Free:       snap.Free,
		GTTTotal:   snap.GTTTotal,
		GTTUsed:    snap.GTTUsed,
		IntervalMS: s.snapshot.IntervalMS,
		WindowSec:  s.snapshot.WindowSec,
	}
	if !snap.Supported {
		return
	}
	s.samples = append(s.samples, sample)
	if len(s.samples) > s.max {
		s.samples = append([]Sample(nil), s.samples[len(s.samples)-s.max:]...)
	}
}

// Snapshot devuelve la última foto y los puntos posteriores a since (ms).
// since = 0 devuelve la ventana completa.
func (s *Sampler) Snapshot(since int64) (Snapshot, []Sample) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := s.snapshot
	series := make([]Sample, 0, len(s.samples))
	for _, sample := range s.samples {
		if sample.T > since {
			series = append(series, sample)
		}
	}
	return snap, series
}

// Latest devuelve la foto actual sin serie.
func (s *Sampler) Latest() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}
