// Package store guarda el histórico de peticiones y tokens en SQLite.
//
// Es la única dependencia externa del proyecto (modernc.org/sqlite, Go puro,
// sin CGO). Si la base no se puede abrir, Ballast arranca igual y el proxy
// sigue funcionando: solo se pierden las métricas.
package store

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// Record es una petición de generación ya terminada.
type Record struct {
	TS               int64
	Method           string
	Path             string
	Model            string
	Status           int
	Streaming        bool
	PromptTokens     int64
	CompletionTokens int64
	TotalDurationMS  int64
	Error            string
}

// Totals son los contadores de un conjunto de peticiones.
type Totals struct {
	Requests   int64 `json:"requests"`
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

// TotalTokens suma entrada y salida.
func (t Totals) TotalTokens() int64 { return t.Prompt + t.Completion }

// ModelUsage es el consumo de un modelo.
type ModelUsage struct {
	Model string `json:"model"`
	Totals
	LastSeen int64 `json:"last_seen"`
}

// Summary es la respuesta de /api/v1/usage/summary.
type Summary struct {
	Total  Totals       `json:"total"`
	Today  Totals       `json:"today"`
	Models []ModelUsage `json:"models"`
}

// Day es un punto de la serie diaria.
type Day struct {
	Date       string `json:"date"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
}

// Store envuelve la base de datos.
type Store struct {
	db   *sql.DB
	path string
}

// Open abre (o crea) la base y aplica las migraciones.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Una sola conexión: escrituras sin bloqueos y consumo mínimo.
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// Path devuelve la ruta del archivo de la base.
func (s *Store) Path() string { return s.path }

// Close cierra la base.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS usage (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	ts                INTEGER NOT NULL,
	method            TEXT    NOT NULL DEFAULT '',
	path              TEXT    NOT NULL DEFAULT '',
	model             TEXT    NOT NULL DEFAULT '',
	status            INTEGER NOT NULL DEFAULT 0,
	streaming         INTEGER NOT NULL DEFAULT 0,
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	duration_ms       INTEGER NOT NULL DEFAULT 0,
	error             TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage(ts);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage(model);
`

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// user_version marca la versión del esquema para futuras migraciones.
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		return err
	}
	return nil
}

// Insert guarda una petición.
func (s *Store) Insert(ctx context.Context, r Record) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	streaming := 0
	if r.Streaming {
		streaming = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage (ts, method, path, model, status, streaming,
		                   prompt_tokens, completion_tokens, duration_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TS, r.Method, r.Path, r.Model, r.Status, streaming,
		r.PromptTokens, r.CompletionTokens, r.TotalDurationMS, r.Error)
	return err
}

// Summary devuelve totales históricos, de hoy y por modelo.
func (s *Store) Summary(ctx context.Context) (Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var out Summary

	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0) FROM usage`)
	if err := row.Scan(&out.Total.Requests, &out.Total.Prompt, &out.Total.Completion); err != nil {
		return out, err
	}

	start := startOfDay(time.Now()).UnixMilli()
	row = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0)
		 FROM usage WHERE ts >= ?`, start)
	if err := row.Scan(&out.Today.Requests, &out.Today.Prompt, &out.Today.Completion); err != nil {
		return out, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens),0),
		       COALESCE(SUM(completion_tokens),0), MAX(ts)
		FROM usage
		WHERE model <> ''
		GROUP BY model
		ORDER BY SUM(prompt_tokens) + SUM(completion_tokens) DESC, model`)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Requests, &m.Prompt, &m.Completion, &m.LastSeen); err != nil {
			return out, err
		}
		out.Models = append(out.Models, m)
	}
	return out, rows.Err()
}

// Series devuelve el consumo de los últimos días, un punto por día.
func (s *Store) Series(ctx context.Context, days int) ([]Day, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	from := startOfDay(time.Now().AddDate(0, 0, -(days - 1))).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `
		SELECT strftime('%Y-%m-%d', ts / 1000, 'unixepoch', 'localtime') AS day,
		       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0)
		FROM usage
		WHERE ts >= ?
		GROUP BY day
		ORDER BY day`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := make(map[string]Day, days)
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.Date, &d.Prompt, &d.Completion); err != nil {
			return nil, err
		}
		byDay[d.Date] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Se rellenan los días sin actividad para que el gráfico no tenga huecos.
	out := make([]Day, 0, days)
	for i := days - 1; i >= 0; i-- {
		key := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		day, ok := byDay[key]
		if !ok {
			day = Day{Date: key}
		}
		out = append(out, day)
	}
	return out, nil
}

// ExportCSV escribe todo el histórico en CSV.
func (s *Store) ExportCSV(ctx context.Context, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, method, path, model, status, streaming,
		       prompt_tokens, completion_tokens, duration_ms, error
		FROM usage ORDER BY ts`)
	if err != nil {
		return err
	}
	defer rows.Close()

	out := csv.NewWriter(w)
	defer out.Flush()
	if err := out.Write([]string{
		"fecha", "metodo", "ruta", "modelo", "estado", "streaming",
		"tokens_entrada", "tokens_salida", "duracion_ms", "error",
	}); err != nil {
		return err
	}

	for rows.Next() {
		var (
			ts, status, streaming        int64
			prompt, completion, duration int64
			method, path, model, errText string
		)
		if err := rows.Scan(&ts, &method, &path, &model, &status, &streaming,
			&prompt, &completion, &duration, &errText); err != nil {
			return err
		}
		record := []string{
			time.UnixMilli(ts).Format(time.RFC3339),
			method,
			path,
			model,
			strconv.FormatInt(status, 10),
			strconv.FormatInt(streaming, 10),
			strconv.FormatInt(prompt, 10),
			strconv.FormatInt(completion, 10),
			strconv.FormatInt(duration, 10),
			errText,
		}
		if err := out.Write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Stats resume el tamaño de la base para /api/v1/status.
func (s *Store) Stats() (requests int64, bytes int64) {
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM usage`).Scan(&requests)
	if info, err := os.Stat(s.path); err == nil {
		bytes = info.Size()
	}
	return requests, bytes
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// String imprime la ruta y el número de filas (útil en logs).
func (s *Store) String() string {
	requests, _ := s.Stats()
	return fmt.Sprintf("%s (%d peticiones)", s.path, requests)
}
