// Package mockpsp is the mock payment provider, built strictly to the assignment spec.
package mockpsp

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Handler serves POST /charges. fast is the delay for tok_success and the failure
// tokens (about 100 ms in production); slow is the delay for tok_timeout (30 s).
type Handler struct {
	fast, slow time.Duration
	calls      sync.Map // token -> *atomic.Int64
}

func New(fast, slow time.Duration) *Handler { return &Handler{fast: fast, slow: slow} }

// Calls returns how many charge requests were received for a token.
func (h *Handler) Calls(token string) int64 {
	if c, ok := h.calls.Load(token); ok {
		return c.(*atomic.Int64).Load()
	}
	return 0
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/charges" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Token       string `json:"token"`
		AmountCents int64  `json:"amount_cents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, http.StatusBadRequest, map[string]any{"status": "failed", "code": "invalid_request"})
		return
	}
	c, _ := h.calls.LoadOrStore(req.Token, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)

	switch req.Token {
	case "tok_success":
		time.Sleep(h.fast)
		reply(w, http.StatusOK, map[string]any{"status": "succeeded", "psp_ref": uuid.NewString()})
	case "tok_timeout":
		time.Sleep(h.slow)
		reply(w, http.StatusOK, map[string]any{"status": "succeeded", "psp_ref": uuid.NewString()})
	case "tok_insufficient_funds":
		time.Sleep(h.fast)
		reply(w, http.StatusOK, map[string]any{"status": "failed", "code": "insufficient_funds"})
	case "tok_card_declined":
		time.Sleep(h.fast)
		reply(w, http.StatusOK, map[string]any{"status": "failed", "code": "card_declined"})
	case "tok_network_error":
		// drop the connection without answering
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	default:
		reply(w, http.StatusBadRequest, map[string]any{"status": "failed", "code": "invalid_token"})
	}
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
