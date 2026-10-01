// Package app is the invoice service: HTTP handlers, pay flow, idempotency and the webhook worker.
package app

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoicesvc/migrations"
)

type App struct {
	db      *pgxpool.Pool
	log     *slog.Logger
	pspURL  string
	psp     *http.Client  // client for the PSP; its timeout is the 40 s PSP timeout in production
	payWait time.Duration // how long /pay waits for the PSP before answering 202 (5 s in production)
}

func New(db *pgxpool.Pool, log *slog.Logger, pspURL string, payWait, pspTimeout time.Duration) *App {
	return &App{db: db, log: log, pspURL: pspURL, payWait: payWait, psp: &http.Client{Timeout: pspTimeout}}
}

// Migrate applies the embedded migrations to the database at dbURL (postgres://...).
func Migrate(dbURL string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, strings.Replace(dbURL, "postgres://", "pgx5://", 1))
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	return a.logRequests(mux)
}

// recorder captures the status code and business id for the request log line.
type recorder struct {
	http.ResponseWriter
	status int
	biz    string
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"latency_ms", time.Since(start).Milliseconds(), "business_id", rec.biz)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, errBody("internal_error", "internal error", "")
	}
	writeRaw(w, status, b)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// errBody builds the standard error shape; attemptID is only set for /pay errors tied to an attempt.
func errBody(code, msg, attemptID string) []byte {
	e := map[string]string{"code": code, "message": msg}
	if attemptID != "" {
		e["attempt_id"] = attemptID
	}
	b, _ := json.Marshal(map[string]any{"error": e})
	return b
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeRaw(w, status, errBody(code, msg, ""))
}

// internalErr logs the real error and returns a generic 500.
func (a *App) internalErr(w http.ResponseWriter, err error) {
	a.log.Error("internal error", "err", err)
	writeErr(w, http.StatusInternalServerError, "internal_error", "internal error")
}
