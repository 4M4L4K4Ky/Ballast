package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/4M4L4K4Ky/Ballast/internal/store"
)

// captureLimit es cuánto cuerpo se guarda para leer los contadores. Ollama
// pone los totales al final, así que se conserva la cola.
const captureLimit = 256 * 1024

// maxModelScan es cuánto cuerpo se lee para sacar el campo "model". Más allá
// de eso se asume que no es una petición de generación normal y no se toca.
const maxModelScan = 2 << 20

// Proxy reenvía las peticiones a Ollama y mide el consumo.
type Proxy struct {
	target *url.URL
	client *http.Client
	store  *store.Store
	queue  chan store.Record

	// Actividad por modelo, para la auto-expulsión. Solo cuenta el tráfico
	// que pasa por aquí: es la única visibilidad que hay.
	mu       sync.Mutex
	inflight map[string]int
	lastSeen map[string]time.Time
}

// New crea el proxy. store puede ser nil: en ese caso no se guardan métricas.
func New(target *url.URL, st *store.Store) *Proxy {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		ExpectContinueTimeout: time.Second,
	}
	p := &Proxy{
		target:   target,
		client:   &http.Client{Transport: transport},
		store:    st,
		inflight: make(map[string]int),
		lastSeen: make(map[string]time.Time),
	}
	if st != nil {
		// Cola con worker: la escritura en SQLite nunca frena la petición.
		p.queue = make(chan store.Record, 256)
		go p.worker()
	}
	return p
}

// worker guarda las métricas en segundo plano. Si la cola se llena, se
// descartan: la petición del usuario ya se ha completado.
func (p *Proxy) worker() {
	for record := range p.queue {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := p.store.Insert(ctx, record); err != nil {
			log.Printf("no se pudo guardar la métrica: %v", err)
		}
		cancel()
	}
}

// ServeHTTP implementa http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Se lee el cuerpo solo para saber qué modelo se va a usar y se restaura
	// tal cual: el cliente de Ollama recibe los mismos bytes.
	if model := readModel(r); model != "" {
		done := p.track(model)
		defer done()
	}

	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = p.target.Scheme
	outReq.URL.Host = p.target.Host
	outReq.Host = p.target.Host
	outReq.RequestURI = ""
	outReq.Header.Del("Accept-Encoding") // que Go negocie y descomprima
	removeHopByHop(outReq.Header)

	resp, err := p.client.Do(outReq)
	if err != nil {
		log.Printf("proxy %s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, "Ollama no responde: "+err.Error(), http.StatusBadGateway)
		p.record(r, http.StatusBadGateway, 0, 0, 0, start, err.Error())
		return
	}
	defer resp.Body.Close()

	removeHopByHop(resp.Header)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	streaming := isStreaming(resp.Header.Get("Content-Type"))
	capture := &captureWriter{max: captureLimit}
	var writer io.Writer = io.MultiWriter(w, capture)
	if streaming {
		if flusher, ok := w.(http.Flusher); ok {
			writer = &flushWriter{w: w, f: flusher, capture: capture}
		}
	}

	_, copyErr := io.Copy(writer, resp.Body)

	model, promptTokens, completionTokens, duration := extract(capture.bytes())
	if duration == 0 {
		duration = time.Since(start).Milliseconds()
	}
	errText := ""
	if copyErr != nil {
		errText = copyErr.Error()
	} else if resp.StatusCode >= 400 {
		errText = strings.TrimSpace(string(capture.bytes()))
	}

	if isGeneration(r.URL.Path) || promptTokens > 0 || completionTokens > 0 {
		p.recordModel(r, resp.StatusCode, model, promptTokens, completionTokens, duration, start, streaming, errText)
	}
}

// record guarda un fallo de conexión sin contadores.
func (p *Proxy) record(r *http.Request, status int, prompt, completion, duration int64, start time.Time, errText string) {
	p.recordModel(r, status, "", prompt, completion, duration, start, false, errText)
}

func (p *Proxy) recordModel(r *http.Request, status int, model string, prompt, completion, duration int64, start time.Time, streaming bool, errText string) {
	if p.queue == nil {
		return
	}
	select {
	case p.queue <- store.Record{
		TS:               start.UnixMilli(),
		Method:           r.Method,
		Path:             r.URL.Path,
		Model:            model,
		Status:           status,
		Streaming:        streaming,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalDurationMS:  duration,
		Error:            truncate(errText, 300),
	}:
	default:
		log.Printf("cola de métricas llena: se descarta %s %s", r.Method, r.URL.Path)
	}
}

// ---------- actividad por modelo ----------

// track marca una petición en vuelo y devuelve la función que la cierra.
func (p *Proxy) track(model string) func() {
	p.mu.Lock()
	p.inflight[model]++
	p.lastSeen[model] = time.Now()
	p.mu.Unlock()

	return func() {
		p.mu.Lock()
		if n := p.inflight[model] - 1; n > 0 {
			p.inflight[model] = n
		} else {
			delete(p.inflight, model)
		}
		p.lastSeen[model] = time.Now()
		p.mu.Unlock()
	}
}

// InFlight devuelve una copia del contador de peticiones en vuelo por modelo.
func (p *Proxy) InFlight() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.inflight))
	for model, n := range p.inflight {
		out[model] = n
	}
	return out
}

// LastSeen devuelve la última vez que cada modelo recibió tráfico.
func (p *Proxy) LastSeen() map[string]time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]time.Time, len(p.lastSeen))
	for model, at := range p.lastSeen {
		out[model] = at
	}
	return out
}

// readModel saca el campo "model" del cuerpo sin consumirlo. Si el cuerpo es
// demasiado grande o no es JSON, devuelve "".
func readModel(r *http.Request) string {
	if r.Body == nil || !isGeneration(r.URL.Path) {
		return ""
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, maxModelScan))
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))
	if len(head) >= maxModelScan {
		return ""
	}

	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(head, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Model)
}

// ---------- lectura de contadores ----------

// payload cubre los campos de Ollama (/api/generate, /api/chat) y de la API
// compatible con OpenAI (/v1/chat/completions).
type payload struct {
	Model           string `json:"model"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
	TotalDuration   int64  `json:"total_duration"` // nanosegundos
	Usage           *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func (p payload) values() (string, int64, int64, int64) {
	prompt, completion := p.PromptEvalCount, p.EvalCount
	if p.Usage != nil {
		if p.Usage.PromptTokens > 0 {
			prompt = p.Usage.PromptTokens
		}
		if p.Usage.CompletionTokens > 0 {
			completion = p.Usage.CompletionTokens
		}
	}
	return p.Model, prompt, completion, p.TotalDuration / int64(time.Millisecond)
}

// extract entiende tanto una respuesta JSON única como un flujo NDJSON.
func extract(body []byte) (model string, prompt, completion, duration int64) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return
	}

	var single payload
	if json.Unmarshal(body, &single) == nil {
		if m, p, c, d := single.values(); m != "" || p > 0 || c > 0 {
			return m, p, c, d
		}
	}

	// Flujo NDJSON: interesa la última línea con contadores.
	lines := bytes.Split(body, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var chunk payload
		if json.Unmarshal(line, &chunk) != nil {
			continue
		}
		if m, p, c, d := chunk.values(); m != "" || p > 0 || c > 0 {
			if model == "" {
				model = m
			}
			if p > 0 {
				prompt = p
			}
			if c > 0 {
				completion = c
			}
			if d > 0 {
				duration = d
			}
			if prompt > 0 || completion > 0 {
				return model, prompt, completion, duration
			}
		}
	}
	return model, prompt, completion, duration
}

// ---------- utilidades HTTP ----------

type captureWriter struct {
	buf []byte
	max int
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.append(p)
	return len(p), nil
}

func (c *captureWriter) append(p []byte) {
	if len(p) >= c.max {
		c.buf = append(c.buf[:0], p[len(p)-c.max:]...)
		return
	}
	if overflow := len(c.buf) + len(p) - c.max; overflow > 0 {
		c.buf = append(c.buf[:0], c.buf[overflow:]...)
	}
	c.buf = append(c.buf, p...)
}

func (c *captureWriter) bytes() []byte { return c.buf }

// flushWriter escribe en el cliente, vacía el búfer y guarda copia.
type flushWriter struct {
	w       io.Writer
	f       http.Flusher
	capture *captureWriter
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.capture.append(p[:n])
	fw.f.Flush()
	return n, err
}

func isStreaming(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "ndjson") || strings.Contains(ct, "event-stream")
}

func isGeneration(path string) bool {
	switch path {
	case "/api/generate", "/api/chat", "/api/embed", "/api/embeddings",
		"/v1/chat/completions", "/v1/completions", "/v1/embeddings":
		return true
	}
	return false
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func removeHopByHop(header http.Header) {
	for _, key := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(key)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
