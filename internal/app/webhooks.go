package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// retryDelays are the waits after failed attempts 1..8; a 9th failure marks the delivery dead.
var retryDelays = []time.Duration{
	10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute,
	2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

func (a *App) createWebhookEndpoint(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	var req struct{ URL string }
	if !decodeJSON(w, r, &req) {
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeErr(w, http.StatusBadRequest, "validation_error", "url must be an absolute http(s) URL")
		return
	}
	id, secret := newID(), "whsec_"+randHex(24)
	if _, err := a.db.Exec(r.Context(),
		`INSERT INTO webhook_endpoints (id, business_id, url, secret) VALUES ($1, $2, $3, $4)`,
		id, biz, req.URL, secret); err != nil {
		a.internalErr(w, err)
		return
	}
	// the secret is shown only here
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "url": req.URL, "secret": secret})
}

// emitEvent writes the event and one pending delivery per endpoint inside the caller's
// tx (outbox pattern), so an event exists if and only if the state change committed.
func emitEvent(ctx context.Context, tx pgx.Tx, biz uuid.UUID, typ string, data any) error {
	eventID := newID()
	payload, err := json.Marshal(map[string]any{"id": eventID, "type": typ, "created_at": time.Now().UTC(), "data": data})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (id, business_id, type, payload) VALUES ($1, $2, $3, $4)`,
		eventID, biz, typ, string(payload)); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM webhook_endpoints WHERE business_id = $1`, biz)
	if err != nil {
		return err
	}
	endpoints, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, ep := range endpoints {
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (id, event_id, endpoint_id, status) VALUES ($1, $2, $3, 'pending')`,
			newID(), eventID, ep); err != nil {
			return err
		}
	}
	return nil
}

type delivery struct {
	id        uuid.UUID
	attempts  int
	eventID   uuid.UUID
	eventType string
	payload   string
	url       string
	secret    string
}

// RunWebhookWorker polls for due deliveries until ctx is cancelled.
func (a *App) RunWebhookWorker(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := a.deliverDue(ctx); err != nil {
			a.log.Error("webhook worker", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) deliverDue(ctx context.Context) error {
	due, err := a.claimDeliveries(ctx)
	if err != nil {
		return err
	}
	for _, d := range due {
		a.send(ctx, d)
	}
	return nil
}

// claimDeliveries locks due rows with SKIP LOCKED and leases them by pushing next_attempt_at
// 60 s out (longer than the 10 s send timeout), so HTTP happens outside the tx and two
// workers never send the same delivery at once.
func (a *App) claimDeliveries(ctx context.Context) ([]delivery, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT d.id, d.attempt_count, e.id, e.type, e.payload::text, w.url, w.secret
		FROM webhook_deliveries d
		JOIN events e ON e.id = d.event_id
		JOIN webhook_endpoints w ON w.id = d.endpoint_id
		WHERE d.status = 'pending' AND d.next_attempt_at <= now()
		ORDER BY d.next_attempt_at LIMIT 10
		FOR UPDATE OF d SKIP LOCKED`)
	if err != nil {
		return nil, err
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (d delivery, err error) {
		err = row.Scan(&d.id, &d.attempts, &d.eventID, &d.eventType, &d.payload, &d.url, &d.secret)
		return
	})
	if err != nil || len(due) == 0 {
		return nil, err
	}
	ids := make([]uuid.UUID, len(due))
	for i, d := range due {
		ids[i] = d.id
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = now() + interval '60 seconds', updated_at = now()
		WHERE id = ANY($1)`, ids); err != nil {
		return nil, err
	}
	return due, tx.Commit(ctx)
}

func (a *App) send(ctx context.Context, d delivery) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(d.secret))
	mac.Write([]byte(ts + "." + d.payload))

	var code *int
	var errMsg *string
	ok := false
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader([]byte(d.payload)))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Webhook-Id", d.eventID.String())
		req.Header.Set("Webhook-Timestamp", ts)
		req.Header.Set("Webhook-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
		var resp *http.Response
		if resp, err = a.hook.Do(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			code = &resp.StatusCode
			ok = resp.StatusCode >= 200 && resp.StatusCode < 300
		}
	}
	if err != nil {
		s := err.Error()
		errMsg = &s
	} else if !ok {
		s := "non-2xx response"
		errMsg = &s
	}

	n := d.attempts + 1
	status, next := "succeeded", time.Now()
	if !ok {
		status = "pending"
		if n > len(retryDelays) {
			status = "dead"
		} else {
			next = time.Now().Add(retryDelays[n-1])
		}
	}
	_, uerr := a.db.Exec(context.WithoutCancel(ctx), `UPDATE webhook_deliveries
		SET status = $2, attempt_count = $3, next_attempt_at = $4, last_status_code = $5, last_error = $6, updated_at = now()
		WHERE id = $1`, d.id, status, n, next, code, errMsg)
	if uerr != nil {
		a.log.Error("update webhook delivery", "delivery_id", d.id, "err", uerr)
	}
	attrs := []any{"delivery_id", d.id, "event_type", d.eventType, "attempt", n, "status_code", code, "result", status}
	if status == "pending" {
		attrs = append(attrs, "next_attempt_at", next)
	}
	a.log.Info("webhook delivery", attrs...)
}
