package autounload

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/4M4L4K4Ky/Ballast/internal/store"
)

// fakeTracker simula el tráfico que ve el proxy.
type fakeTracker struct {
	inflight map[string]int
	lastSeen map[string]time.Time
}

func (f fakeTracker) InFlight() map[string]int       { return f.inflight }
func (f fakeTracker) LastSeen() map[string]time.Time { return f.lastSeen }

type harness struct {
	manager  *Manager
	clock    time.Time
	unloaded []string
	loaded   []string
	loadErr  error
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("abrir store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{
		clock:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		loaded: []string{"qwen3:4b"},
	}

	m := New(st, fakeTracker{}, func(context.Context) ([]string, error) {
		return h.loaded, h.loadErr
	}, func(_ context.Context, model string) error {
		h.unloaded = append(h.unloaded, model)
		return nil
	})
	m.now = func() time.Time { return h.clock }
	m.started = h.clock
	h.manager = m
	return h
}

// advance mueve el reloj y devuelve el gestor con el tracker indicado.
func (h *harness) advance(d time.Duration, tracker Tracker) {
	h.clock = h.clock.Add(d)
	h.manager.proxy = tracker
}

func (h *harness) enable(t *testing.T, idle int) {
	t.Helper()
	ctx := context.Background()
	if err := h.manager.SetEnabled(ctx, true); err != nil {
		t.Fatalf("activar: %v", err)
	}
	if err := h.manager.SetIdleMinutes(ctx, idle); err != nil {
		t.Fatalf("minutos: %v", err)
	}
}

func TestDisabledNoExpulsa(t *testing.T) {
	h := newHarness(t)
	h.advance(24*time.Hour, fakeTracker{})

	if got := h.manager.Evaluate(context.Background()); len(got) != 0 {
		t.Fatalf("expulsó con la política desactivada: %v", got)
	}
}

func TestEsperaUnCicloAlArrancar(t *testing.T) {
	h := newHarness(t)
	h.enable(t, 5)
	h.advance(30*time.Second, fakeTracker{})

	if got := h.manager.Evaluate(context.Background()); len(got) != 0 {
		t.Fatalf("expulsó durante el arranque: %v", got)
	}
}

func TestRespetaElUmbralDeInactividad(t *testing.T) {
	h := newHarness(t)
	h.enable(t, 30)
	h.advance(Cycle, fakeTracker{})
	h.manager.Evaluate(context.Background()) // fija firstSeen

	h.advance(20*time.Minute, fakeTracker{})
	if got := h.manager.Evaluate(context.Background()); len(got) != 0 {
		t.Fatalf("expulsó con 20 min de un umbral de 30: %v", got)
	}

	h.advance(15*time.Minute, fakeTracker{})
	if got := h.manager.Evaluate(context.Background()); len(got) != 1 {
		t.Fatalf("no expulsó tras 35 min: %v", got)
	}
}

func TestNoExpulsaConPeticionesEnVuelo(t *testing.T) {
	h := newHarness(t)
	h.enable(t, 5)
	h.advance(Cycle, fakeTracker{})
	h.manager.Evaluate(context.Background())

	h.advance(time.Hour, fakeTracker{inflight: map[string]int{"qwen3:4b": 1}})
	if got := h.manager.Evaluate(context.Background()); len(got) != 0 {
		t.Fatalf("expulsó con una petición en vuelo: %v", got)
	}

	h.advance(time.Minute, fakeTracker{})
	if got := h.manager.Evaluate(context.Background()); len(got) != 1 {
		t.Fatalf("no expulsó al quedar libre: %v", got)
	}
}

func TestNoExpulsaModeloFijado(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.enable(t, 5)
	if err := h.manager.store.SetPinned(ctx, "qwen3:4b", true); err != nil {
		t.Fatalf("fijar: %v", err)
	}
	h.advance(Cycle, fakeTracker{})
	h.manager.Evaluate(ctx)

	h.advance(time.Hour, fakeTracker{})
	if got := h.manager.Evaluate(ctx); len(got) != 0 {
		t.Fatalf("expulsó un modelo fijado: %v", got)
	}
}

func TestNoExpulsaRecienCargadoDesdeLaUI(t *testing.T) {
	h := newHarness(t)
	h.enable(t, 5)
	h.advance(Cycle, fakeTracker{})
	h.manager.Evaluate(context.Background())

	h.manager.MarkLoaded("qwen3:4b")
	h.advance(6*time.Minute, fakeTracker{})
	if got := h.manager.Evaluate(context.Background()); len(got) != 0 {
		t.Fatalf("expulsó un modelo recién cargado: %v", got)
	}

	h.advance(5*time.Minute, fakeTracker{})
	if got := h.manager.Evaluate(context.Background()); len(got) != 1 {
		t.Fatalf("no expulsó pasada la gracia de 10 min: %v", got)
	}
}

func TestNoExpulsaSiOllamaNoResponde(t *testing.T) {
	h := newHarness(t)
	h.enable(t, 5)
	h.advance(Cycle, fakeTracker{})
	h.manager.Evaluate(context.Background())

	h.loadErr = errors.New("ollama caído")
	h.advance(time.Hour, fakeTracker{})
	if got := h.manager.Evaluate(context.Background()); len(got) != 0 {
		t.Fatalf("expulsó sin confirmación de Ollama: %v", got)
	}
}

func TestPausaBloquea(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.enable(t, 5)
	h.advance(Cycle, fakeTracker{})
	h.manager.Evaluate(ctx)

	if err := h.manager.Pause(ctx, time.Hour); err != nil {
		t.Fatalf("pausar: %v", err)
	}
	h.advance(30*time.Minute, fakeTracker{})
	if got := h.manager.Evaluate(ctx); len(got) != 0 {
		t.Fatalf("expulsó en pausa: %v", got)
	}

	if err := h.manager.Resume(ctx); err != nil {
		t.Fatalf("reanudar: %v", err)
	}
	h.advance(time.Hour, fakeTracker{})
	if got := h.manager.Evaluate(ctx); len(got) != 1 {
		t.Fatalf("no expulsó tras reanudar: %v", got)
	}
}
