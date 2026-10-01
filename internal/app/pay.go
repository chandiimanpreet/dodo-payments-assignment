package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// chargeResult is what the detached PSP goroutine reports back to the handler.
type chargeResult struct {
	attempt Attempt
	invoice Invoice
	err     error
}

type pspResult struct {
	status   string // "succeeded" or "failed"
	ref      string
	code     string
	timedOut bool // our client timeout hit: no evidence either way
}

func (a *App) pay(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	// 1. Header and body checks come first so a 400 never creates an idempotency row.
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "idempotency_key_missing", "Idempotency-Key header is required")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var req struct {
		CardToken string `json:"card_token"`
	}
	if err != nil || json.Unmarshal(raw, &req) != nil || req.CardToken == "" {
		writeErr(w, http.StatusBadRequest, "validation_error", "card_token must be a non-empty string")
		return
	}
	invID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "invoice not found")
		return
	}

	// 2. Idempotency, checked before the invoice state.
	// From here the request runs to the end even if the client disconnects: a cancelled
	// context during the claim commit could leave a pending attempt that nothing charges
	// or finalizes, or a key stuck in_progress.
	ctx := context.WithoutCancel(r.Context())
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", r.Method, r.URL.Path)
	h.Write(raw)
	fingerprint := h.Sum(nil)

	tag, err := a.db.Exec(ctx, `INSERT INTO idempotency_keys (business_id, key, fingerprint, status)
		VALUES ($1, $2, $3, 'in_progress') ON CONFLICT DO NOTHING`, biz, key, fingerprint)
	if err != nil {
		a.internalErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		a.replayOrReject(w, r, biz, key, fingerprint)
		return
	}

	// From here on this request owns the key. Until the attempt exists, a rejection
	// releases it so the same key can be retried; after that the response is stored.
	// The claim tx is rolled back before touching the pool again: holding a connection
	// while asking for a second one deadlocks the pool under concurrent requests.
	attemptID := newID()
	var total int64
	tx, err := a.db.Begin(ctx)
	if err != nil {
		a.releaseKey(ctx, biz, key)
		a.internalErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	abort := func() { tx.Rollback(ctx); a.releaseKey(ctx, biz, key) }

	// 3. Claim: one short tx that makes the pending attempt durable before the PSP is called.
	// The row lock plus the state re-check is what stops request B from inserting a new
	// pending attempt after attempt A has just marked the invoice PAID and left the
	// partial unique index.
	err = tx.QueryRow(ctx, `UPDATE invoices SET updated_at = now()
		WHERE id = $1 AND business_id = $2 AND state = 'OPEN' RETURNING total_cents`, invID, biz).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		cur, err := getInvoice(ctx, tx, invID, biz)
		abort()
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeErr(w, http.StatusNotFound, "not_found", "invoice not found")
		case err != nil:
			a.internalErr(w, err)
		default:
			writeErr(w, http.StatusConflict, "invalid_state_transition", fmt.Sprintf("cannot pay invoice in state %s", cur.State))
		}
		return
	}
	if err != nil {
		abort()
		a.internalErr(w, err)
		return
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_attempts (id, invoice_id, business_id, amount_cents, status)
		VALUES ($1, $2, $3, $4, 'pending')`, attemptID, invID, biz, total)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		abort()
		writeErr(w, http.StatusConflict, "payment_in_progress", "a payment attempt is already in flight for this invoice")
		return
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		abort()
		a.internalErr(w, err)
		return
	}
	a.log.Info("payment attempt", "attempt_id", attemptID, "invoice_id", invID, "from", "", "to", "pending")

	// 4. PSP call in a detached goroutine: it finishes (and finalizes the attempt) even if
	// this handler has already answered 202 or the client has gone away.
	ch := make(chan chargeResult, 1)
	go func() { ch <- a.charge(attemptID, invID, biz, total, req.CardToken) }()

	// 5. Wait up to payWait for the result, else answer 202.
	var status int
	var body []byte
	select {
	case res := <-ch:
		status, body = payResponse(res, attemptID)
	case <-time.After(a.payWait):
		status, body = pendingResponse(attemptID)
	}

	// 6. Store the response so the same key replays it.
	body = a.completeKey(ctx, biz, key, status, body)
	writeRaw(w, status, body)
}

func pendingResponse(attemptID uuid.UUID) (int, []byte) {
	b, _ := json.Marshal(map[string]any{"attempt_id": attemptID, "status": "pending"})
	return http.StatusAccepted, b
}

func payResponse(res chargeResult, attemptID uuid.UUID) (int, []byte) {
	if res.err != nil {
		return http.StatusInternalServerError, errBody("internal_error", "internal error", attemptID.String())
	}
	switch res.attempt.Status {
	case "succeeded":
		b, _ := json.Marshal(map[string]any{"attempt": res.attempt, "invoice": res.invoice})
		return http.StatusOK, b
	case "failed":
		code := *res.attempt.FailureCode
		if code == "psp_unavailable" {
			return http.StatusBadGateway, errBody(code,
				"the payment provider is unavailable; the invoice is still open and the payment can be retried with a new Idempotency-Key", attemptID.String())
		}
		return http.StatusPaymentRequired, errBody(code, "payment failed: "+code, attemptID.String())
	}
	return pendingResponse(attemptID)
}

// replayOrReject handles a request whose idempotency key already exists.
func (a *App) replayOrReject(w http.ResponseWriter, r *http.Request, biz uuid.UUID, key string, fingerprint []byte) {
	var fp []byte
	var status string
	var respStatus *int
	var respBody *string
	err := a.db.QueryRow(r.Context(), `SELECT fingerprint, status, response_status, response_body::text
		FROM idempotency_keys WHERE business_id = $1 AND key = $2`, biz, key).Scan(&fp, &status, &respStatus, &respBody)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// the owner released the key (rejected before creating an attempt) between our insert and read
		writeErr(w, http.StatusConflict, "idempotency_request_in_progress", "a request with this Idempotency-Key is in progress")
	case err != nil:
		a.internalErr(w, err)
	case !bytes.Equal(fp, fingerprint):
		writeErr(w, http.StatusUnprocessableEntity, "idempotency_key_reused", "this Idempotency-Key was used with a different request")
	case status == "in_progress" || respStatus == nil || respBody == nil:
		writeErr(w, http.StatusConflict, "idempotency_request_in_progress", "a request with this Idempotency-Key is in progress")
	default:
		writeRaw(w, *respStatus, []byte(*respBody)) // replay exactly, no PSP call
	}
}

func (a *App) releaseKey(ctx context.Context, biz uuid.UUID, key string) {
	if _, err := a.db.Exec(ctx, `DELETE FROM idempotency_keys WHERE business_id = $1 AND key = $2`, biz, key); err != nil {
		a.log.Error("release idempotency key", "err", err)
	}
}

// completeKey stores the response and returns it in the stored (jsonb-normalised) form,
// so the first response and every replay are byte-identical.
func (a *App) completeKey(ctx context.Context, biz uuid.UUID, key string, status int, body []byte) []byte {
	var stored string
	err := a.db.QueryRow(ctx, `UPDATE idempotency_keys SET status = 'completed', response_status = $3, response_body = $4
		WHERE business_id = $1 AND key = $2 RETURNING response_body::text`, biz, key, status, string(body)).Scan(&stored)
	if err != nil {
		a.log.Error("store idempotent response", "err", err)
		return body
	}
	return []byte(stored)
}

// charge calls the PSP and finalizes the attempt. It never returns before the attempt
// is final, except when the PSP timed out (attempt stays pending, invoice stays blocked).
func (a *App) charge(attemptID, invID, biz uuid.UUID, amount int64, token string) chargeResult {
	ctx := context.Background()
	start := time.Now()
	res := a.callPSP(token, amount)
	latency := time.Since(start).Milliseconds()

	if res.timedOut {
		a.log.Warn("payment attempt", "attempt_id", attemptID, "invoice_id", invID, "from", "pending", "to", "pending",
			"psp_latency_ms", latency, "note", "psp client timeout, attempt left pending")
		return chargeResult{attempt: Attempt{Status: "pending"}}
	}
	out, err := a.finalizeAttempt(ctx, attemptID, invID, biz, res)
	if err != nil {
		a.log.Error("finalize payment attempt", "attempt_id", attemptID, "invoice_id", invID, "err", err)
		return chargeResult{err: err}
	}
	a.log.Info("payment attempt", "attempt_id", attemptID, "invoice_id", invID, "from", "pending", "to", out.attempt.Status,
		"psp_latency_ms", latency, "failure_code", out.attempt.FailureCode)
	return out
}

func (a *App) callPSP(token string, amount int64) pspResult {
	body, _ := json.Marshal(map[string]any{"token": token, "amount_cents": amount})
	unavailable := pspResult{status: "failed", code: "psp_unavailable"}

	resp, err := a.psp.Post(a.pspURL+"/charges", "application/json", bytes.NewReader(body))
	if err != nil {
		if isTimeout(err) {
			return pspResult{timedOut: true}
		}
		return unavailable // dial error or dropped connection
	}
	defer resp.Body.Close()

	// Classify by the body first, whatever the HTTP status.
	var out struct {
		Status string `json:"status"`
		PSPRef string `json:"psp_ref"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		if isTimeout(err) {
			return pspResult{timedOut: true}
		}
		return unavailable
	}
	switch {
	case out.Status == "succeeded":
		return pspResult{status: "succeeded", ref: out.PSPRef}
	case out.Status == "failed" && out.Code != "":
		return pspResult{status: "failed", code: out.Code}
	}
	return unavailable
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// finalizeAttempt records the PSP outcome in one tx.
func (a *App) finalizeAttempt(ctx context.Context, attemptID, invID, biz uuid.UUID, res pspResult) (chargeResult, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return chargeResult{}, err
	}
	defer tx.Rollback(ctx)

	// Lock order is invoice first, then attempt, same as the claim tx. Taking the attempt
	// row first can deadlock with a concurrent claim, which holds the invoice lock and
	// waits on this attempt's unique-index entry.
	newState := "OPEN"
	if res.status == "succeeded" {
		newState = "PAID"
	}
	tag, err := tx.Exec(ctx, `UPDATE invoices SET state = $2, updated_at = now() WHERE id = $1 AND state = 'OPEN'`, invID, newState)
	if err != nil {
		return chargeResult{}, err
	}
	if tag.RowsAffected() != 1 {
		// never mark a charged invoice anything else; the attempt stays pending (blocked)
		return chargeResult{}, fmt.Errorf("invoice %s not OPEN at finalize", invID)
	}
	if res.status == "succeeded" {
		tag, err = tx.Exec(ctx, `UPDATE payment_attempts SET status = 'succeeded', psp_ref = $2, updated_at = now()
			WHERE id = $1 AND status = 'pending'`, attemptID, res.ref)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE payment_attempts SET status = 'failed', failure_code = $2, updated_at = now()
			WHERE id = $1 AND status = 'pending'`, attemptID, res.code)
	}
	if err != nil {
		return chargeResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return chargeResult{}, fmt.Errorf("attempt %s was not pending at finalize", attemptID)
	}

	attempt, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptCols+` FROM payment_attempts WHERE id = $1`, attemptID))
	if err != nil {
		return chargeResult{}, err
	}
	inv, err := getInvoice(ctx, tx, invID, biz)
	if err != nil {
		return chargeResult{}, err
	}
	typ := "invoice.paid"
	if res.status == "failed" {
		typ = "invoice.payment_failed"
	}
	if err := emitEvent(ctx, tx, biz, typ, map[string]any{"invoice": inv, "attempt": attempt}); err != nil {
		return chargeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return chargeResult{}, err
	}
	return chargeResult{attempt: attempt, invoice: inv}, nil
}
