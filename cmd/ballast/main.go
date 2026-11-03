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
	"strings"
	"syscall"
	"time"

	"github.com/4M4L4K4Ky/Ballast"
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/models", s.handleModels)
	mux.HandleFunc("GET /api/v1/models/{name}", s.handleModel)
	mux.HandleFunc("GET /api/v1/ps", s.handlePS)
	mux.HandleFunc("POST /api/v1/models/{name}/unload", s.handleUnload)
	mux.HandleFunc("POST /api/v1/models/{name}/load", s.handleLoad)
	mux.HandleFunc("POST /api/v1/vram/release", s.handleReleaseVRAM)
	mux.Handle("/", staticHandler())

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
	Version       string `json:"version"`
	OllamaURL     string `json:"ollama_url"`
	OllamaVersion string `json:"ollama_version,omitempty"`
	Reachable     bool   `json:"reachable"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Error         string `json:"error,omitempty"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := statusResponse{
		Version:       version,
		OllamaURL:     s.ollama.String(),
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
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
	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := s.ollamaJSON(r.Context(), http.MethodGet, "/api/ps", nil, &ps); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	unloaded := make([]string, 0, len(ps.Models))
	failed := make(map[string]string)
	for _, m := range ps.Models {
		body := map[string]any{"model": m.Name, "keep_alive": 0}
		if err := s.ollamaJSON(r.Context(), http.MethodPost, "/api/generate", body, nil); err != nil {
			failed[m.Name] = err.Error()
			continue
		}
		unloaded = append(unloaded, m.Name)
	}

	if len(unloaded) > 0 {
		log.Printf("VRAM liberada: %s", strings.Join(unloaded, ", "))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"unloaded": unloaded,
		"failed":   failed,
	})
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
