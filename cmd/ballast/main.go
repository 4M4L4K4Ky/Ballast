// Ballast es un dashboard local para controlar Ollama.
// Este binario sirve la interfaz web y una API mínima sobre la API de Ollama.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/4M4L4K4Ky/Ballast"
	"github.com/4M4L4K4Ky/Ballast/internal/disk"
	"github.com/4M4L4K4Ky/Ballast/internal/mem"
	"github.com/4M4L4K4Ky/Ballast/internal/proxy"
	"github.com/4M4L4K4Ky/Ballast/internal/state"
	"github.com/4M4L4K4Ky/Ballast/internal/store"
)

// version se sobreescribe en tiempo de compilación con -ldflags.
var version = "0.0.1-dev"

const (
	defaultListen    = "127.0.0.1:11435"
	defaultOllamaURL = "http://127.0.0.1:11434"
)

type server struct {
	ollama   *url.URL
	client   *http.Client
	started  time.Time
	listenOn string
	mem      *mem.Sampler
	game     *state.GameMode
	store    *store.Store
	proxy    *proxy.Proxy

	diskMu     sync.Mutex
	diskAt     time.Time
	diskReport *disk.Report
}

func main() {
	listen := flag.String("listen", envOr("BALLAST_LISTEN", defaultListen), "dirección de escucha")
	ollamaURL := flag.String("ollama", envOr("BALLAST_OLLAMA_URL", defaultOllamaURL), "URL de Ollama")
	showVersion := flag.Bool("version", false, "muestra la versión y sale")
	flag.Parse()

	if *showVersion {
		fmt.Println("ballast", version)
		return
	}

	u, err := url.Parse(*ollamaURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Fatalf("URL de Ollama inválida: %q", *ollamaURL)
	}

	s := &server{
		ollama:   u,
		client:   &http.Client{Timeout: 60 * time.Second},
		started:  time.Now(),
		listenOn: *listen,
		mem:      mem.NewSampler(time.Second, 15*time.Minute),
		game:     state.NewGameMode(state.DataDir()),
	}

	memCtx, stopMem := context.WithCancel(context.Background())
	defer stopMem()
	go s.mem.Run(memCtx, s.ollamaVRAM)

	// La base de datos es opcional: si falla, el dashboard sigue funcionando.
	dbPath := filepath.Join(state.DataDir(), "ballast.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Printf("aviso: sin base de datos (%v); no se contarán tokens", err)
	} else {
		s.store = st
		defer st.Close()
		log.Printf("base de datos: %s", st)
	}
	s.proxy = proxy.New(u, s.store)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/models", s.handleModels)
	mux.HandleFunc("GET /api/v1/models/{name}", s.handleModel)
	mux.HandleFunc("GET /api/v1/ps", s.handlePS)
	mux.HandleFunc("GET /api/v1/memory", s.handleMemory)
	mux.HandleFunc("GET /api/v1/disk", s.handleDisk)
	mux.HandleFunc("GET /api/v1/usage/summary", s.handleUsageSummary)
	mux.HandleFunc("GET /api/v1/usage/series", s.handleUsageSeries)
	mux.HandleFunc("GET /api/v1/usage/export.csv", s.handleUsageCSV)
	mux.HandleFunc("GET /api/v1/game-mode", s.handleGameModeStatus)
	mux.HandleFunc("POST /api/v1/game-mode", s.handleGameMode)
	mux.HandleFunc("POST /api/v1/models/{name}/unload", s.handleUnload)
	mux.HandleFunc("POST /api/v1/models/{name}/load", s.handleLoad)
	mux.HandleFunc("POST /api/v1/vram/release", s.handleReleaseVRAM)
	mux.Handle("/", s.rootHandler())

	srv := &http.Server{
		Addr:              *listen,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("ballast %s escuchando en http://%s (Ollama en %s)", version, *listen, u)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("servidor: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("apagado: %v", err)
	}
	log.Println("ballast detenido")
}

// ---------- API ----------

type statusResponse struct {
	Version       string     `json:"version"`
	OllamaURL     string     `json:"ollama_url"`
	OllamaVersion string     `json:"ollama_version,omitempty"`
	Reachable     bool       `json:"reachable"`
	UptimeSeconds int64      `json:"uptime_seconds"`
	GameMode      bool       `json:"game_mode"`
	GameSince     *time.Time `json:"game_since,omitempty"`
	Error         string     `json:"error,omitempty"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := statusResponse{
		Version:       version,
		OllamaURL:     s.ollama.String(),
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		GameMode:      s.game.Enabled(),
	}
	if since := s.game.Since(); !since.IsZero() {
		resp.GameSince = &since
	}

	var v struct {
		Version string `json:"version"`
	}
	if err := s.ollamaJSON(r.Context(), http.MethodGet, "/api/version", nil, &v); err != nil {
		resp.Error = err.Error()
	} else {
		resp.Reachable = true
		resp.OllamaVersion = v.Version
	}
	writeJSON(w, http.StatusOK, resp)
}

type modelDetails struct {
	ParentModel       string   `json:"parent_model,omitempty"`
	Format            string   `json:"format,omitempty"`
	Family            string   `json:"family"`
	Families          []string `json:"families,omitempty"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
	ContextLength     int64    `json:"context_length"`
}

type model struct {
	Name         string       `json:"name"`
	Size         int64        `json:"size"`
	Digest       string       `json:"digest"`
	ModifiedAt   time.Time    `json:"modified_at"`
	Details      modelDetails `json:"details"`
	Capabilities []string     `json:"capabilities"`
	Loaded       bool         `json:"loaded"`
	SizeVRAM     int64        `json:"size_vram"`
	ExpiresAt    *time.Time   `json:"expires_at,omitempty"`
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.listModels(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// modelDetail es la ficha completa de un modelo: lo que da /api/tags más lo
// que solo aparece en /api/show (arquitectura, capas, parámetros del Modelfile).
type modelDetail struct {
	model
	Architecture    string   `json:"architecture,omitempty"`
	Format          string   `json:"format,omitempty"`
	Families        []string `json:"families,omitempty"`
	ParameterCount  int64    `json:"parameter_count,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	EmbeddingLength int64    `json:"embedding_length,omitempty"`
	BlockCount      int64    `json:"block_count,omitempty"`
	HeadCount       int64    `json:"head_count,omitempty"`
	Parameters      string   `json:"parameters,omitempty"`
}

// handleModel devuelve la ficha de un modelo concreto.
func (s *server) handleModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "falta el nombre del modelo"})
		return
	}

	var show struct {
		Details      modelDetails   `json:"details"`
		Capabilities []string       `json:"capabilities"`
		Parameters   string         `json:"parameters"`
		ModelInfo    map[string]any `json:"model_info"`
	}
	if err := s.ollamaJSON(r.Context(), http.MethodPost, "/api/show", map[string]string{"model": name}, &show); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	models, err := s.listModels(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	var found *model
	for i := range models {
		if models[i].Name == name {
			found = &models[i]
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "modelo no encontrado: " + name})
		return
	}

	resp := modelDetail{
		model:           *found,
		Architecture:    infoString(show.ModelInfo, "general.architecture"),
		Format:          show.Details.Format,
		Families:        show.Details.Families,
		ParameterCount:  infoInt(show.ModelInfo, ".parameter_count"),
		ContextLength:   infoInt(show.ModelInfo, ".context_length"),
		EmbeddingLength: infoInt(show.ModelInfo, ".embedding_length"),
		BlockCount:      infoInt(show.ModelInfo, ".block_count"),
		HeadCount:       infoInt(show.ModelInfo, ".attention.head_count"),
		Parameters:      strings.TrimSpace(show.Parameters),
	}
	// La ficha de /api/show es más rica que la de /api/tags; se prefiere.
	if show.Details.Family != "" {
		if show.Details.ContextLength == 0 {
			show.Details.ContextLength = resp.Details.ContextLength
		}
		resp.Details = show.Details
	}
	if len(show.Capabilities) > 0 {
		resp.Capabilities = show.Capabilities
	}
	if resp.ContextLength == 0 {
		resp.ContextLength = resp.Details.ContextLength
	}

	writeJSON(w, http.StatusOK, resp)
}

// infoInt busca una clave de model_info por sufijo, sin depender de la
// arquitectura del modelo (qwen3.context_length, llama.context_length…).
func infoInt(info map[string]any, suffix string) int64 {
	for k, v := range info {
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		if f, ok := v.(float64); ok {
			return int64(f)
		}
	}
	return 0
}

func infoString(info map[string]any, key string) string {
	if v, ok := info[key].(string); ok {
		return v
	}
	return ""
}

func (s *server) handlePS(w http.ResponseWriter, r *http.Request) {
	var ps struct {
		Models []struct {
			Name      string    `json:"name"`
			Size      int64     `json:"size"`
			SizeVRAM  int64     `json:"size_vram"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"models"`
	}
	if err := s.ollamaJSON(r.Context(), http.MethodGet, "/api/ps", nil, &ps); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (s *server) handleUnload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "falta el nombre del modelo"})
		return
	}

	body := map[string]any{"model": name, "keep_alive": 0}
	if err := s.ollamaJSON(r.Context(), http.MethodPost, "/api/generate", body, nil); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("modelo expulsado: %s", name)
	writeJSON(w, http.StatusOK, map[string]string{"unloaded": name})
}

// handleLoad carga un modelo en la VRAM con el keep_alive indicado.
// Se apoya en /api/generate con prompt vacío: Ollama carga y devuelve al momento.
func (s *server) handleLoad(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "falta el nombre del modelo"})
		return
	}
	if s.game.Enabled() {
		writeJSON(w, http.StatusLocked, map[string]string{
			"error": "modo juego activo: desactívalo para cargar modelos",
		})
		return
	}

	var req struct {
		KeepAlive string `json:"keep_alive"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.KeepAlive == "" {
		req.KeepAlive = "10m"
	}

	body := map[string]any{"model": name, "keep_alive": req.KeepAlive}
	if err := s.ollamaJSON(r.Context(), http.MethodPost, "/api/generate", body, nil); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("modelo cargado: %s (keep_alive %s)", name, req.KeepAlive)
	writeJSON(w, http.StatusOK, map[string]string{"loaded": name, "keep_alive": req.KeepAlive})
}

// handleReleaseVRAM expulsa de la VRAM todos los modelos cargados de golpe.
// Pensado para liberar la GPU al terminar de trabajar.
func (s *server) handleReleaseVRAM(w http.ResponseWriter, r *http.Request) {
	unloaded, failed, err := s.releaseVRAM(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"unloaded": unloaded,
		"failed":   failed,
	})
}

// releaseVRAM expulsa todos los modelos y devuelve qué se ha liberado.
func (s *server) releaseVRAM(ctx context.Context) ([]string, map[string]string, error) {
	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := s.ollamaJSON(ctx, http.MethodGet, "/api/ps", nil, &ps); err != nil {
		return nil, nil, err
	}

	unloaded := make([]string, 0, len(ps.Models))
	failed := make(map[string]string)
	for _, m := range ps.Models {
		body := map[string]any{"model": m.Name, "keep_alive": 0}
		if err := s.ollamaJSON(ctx, http.MethodPost, "/api/generate", body, nil); err != nil {
			failed[m.Name] = err.Error()
			continue
		}
		unloaded = append(unloaded, m.Name)
	}

	if len(unloaded) > 0 {
		log.Printf("VRAM liberada: %s", strings.Join(unloaded, ", "))
	}
	return unloaded, failed, nil
}

// handleGameModeStatus informa si el modo juego está activo.
func (s *server) handleGameModeStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"enabled": s.game.Enabled()}
	if since := s.game.Since(); !since.IsZero() {
		resp["since"] = since
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGameMode activa o desactiva el modo juego. Al activarlo libera la VRAM.
func (s *server) handleGameMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cuerpo JSON inválido"})
		return
	}
	if err := s.game.Set(req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp := map[string]any{"enabled": req.Enabled}
	if req.Enabled {
		unloaded, failed, err := s.releaseVRAM(r.Context())
		if err != nil {
			// El modo queda activo aunque Ollama no responda.
			resp["warning"] = err.Error()
		} else {
			resp["unloaded"] = unloaded
			resp["failed"] = failed
		}
		log.Printf("modo juego activado")
	} else {
		log.Printf("modo juego desactivado")
	}
	writeJSON(w, http.StatusOK, resp)
}

// rootHandler sirve los tres archivos de la interfaz y manda el resto a Ollama.
// Así el proxy queda en medio de todo el tráfico sin tocar la API del dashboard.
func (s *server) rootHandler() http.Handler {
	static := staticHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html", "/app.js", "/styles.css":
			static.ServeHTTP(w, r)
		default:
			s.proxy.ServeHTTP(w, r)
		}
	})
}

// ---------- uso y tokens ----------

func (s *server) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "base de datos no disponible"})
		return
	}
	summary, err := s.store.Summary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *server) handleUsageSeries(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "base de datos no disponible"})
		return
	}
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	series, err := s.store.Series(r.Context(), days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "series": series})
}

func (s *server) handleUsageCSV(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "base de datos no disponible"})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ballast-uso.csv"`)
	if err := s.store.ExportCSV(r.Context(), w); err != nil {
		log.Printf("exportar CSV: %v", err)
	}
}

// handleDisk devuelve el espacio de los discos reales. Se cachea 10 s.
func (s *server) handleDisk(w http.ResponseWriter, r *http.Request) {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()

	if s.diskReport != nil && time.Since(s.diskAt) < 10*time.Second {
		writeJSON(w, http.StatusOK, s.diskReport)
		return
	}

	modelsDir, exists := disk.ModelsDir()
	filesystems, err := disk.List(modelsDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	report := &disk.Report{
		ModelsDir:       modelsDir,
		ModelsDirExists: exists,
		Remote:          !isLocalHost(s.ollama.Hostname()),
		Filesystems:     filesystems,
	}
	s.diskReport = report
	s.diskAt = time.Now()
	writeJSON(w, http.StatusOK, report)
}

func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

type memoryResponse struct {
	mem.Snapshot
	Now    int64        `json:"now"`
	Series []mem.Sample `json:"series"`
}

// handleMemory devuelve la foto de la GPU y los puntos nuevos desde ?since=.
func (s *server) handleMemory(w http.ResponseWriter, r *http.Request) {
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}

	now := time.Now().UnixMilli()
	if since > now {
		since = 0 // el reloj cambió o el cliente venía de otra sesión
	}
	snap, series := s.mem.Snapshot(since)
	writeJSON(w, http.StatusOK, memoryResponse{Snapshot: snap, Now: now, Series: series})
}

// ollamaVRAM suma la VRAM que Ollama dice tener ocupada por modelos.
func (s *server) ollamaVRAM(ctx context.Context) int64 {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var ps struct {
		Models []struct {
			SizeVRAM int64 `json:"size_vram"`
		} `json:"models"`
	}
	if err := s.ollamaJSON(ctx, http.MethodGet, "/api/ps", nil, &ps); err != nil {
		return 0
	}
	var total int64
	for _, m := range ps.Models {
		total += m.SizeVRAM
	}
	return total
}

func (s *server) listModels(ctx context.Context) ([]model, error) {
	var tags struct {
		Models []model `json:"models"`
	}
	if err := s.ollamaJSON(ctx, http.MethodGet, "/api/tags", nil, &tags); err != nil {
		return nil, err
	}

	var ps struct {
		Models []struct {
			Name      string    `json:"name"`
			Size      int64     `json:"size"`
			SizeVRAM  int64     `json:"size_vram"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"models"`
	}
	if err := s.ollamaJSON(ctx, http.MethodGet, "/api/ps", nil, &ps); err != nil {
		return nil, err
	}

	loaded := make(map[string]struct {
		vram    int64
		expires time.Time
	}, len(ps.Models))
	for _, m := range ps.Models {
		loaded[m.Name] = struct {
			vram    int64
			expires time.Time
		}{m.SizeVRAM, m.ExpiresAt}
	}

	for i := range tags.Models {
		if l, ok := loaded[tags.Models[i].Name]; ok {
			tags.Models[i].Loaded = true
			tags.Models[i].SizeVRAM = l.vram
			exp := l.expires
			tags.Models[i].ExpiresAt = &exp
		}
	}
	return tags.Models, nil
}

// ---------- cliente de Ollama ----------

func (s *server) ollamaJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(raw))
	}

	target := s.ollama.JoinPath(path).String()
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("no se pudo hablar con Ollama en %s: %w", s.ollama, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Ollama respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---------- utilidades HTTP ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func staticHandler() http.Handler {
	sub, err := fs.Sub(ballast.WebFS, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := fs.Stat(sub, path); err != nil {
				// Fallback de SPA: cualquier ruta desconocida sirve index.html.
				r.URL.Path = "/"
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
