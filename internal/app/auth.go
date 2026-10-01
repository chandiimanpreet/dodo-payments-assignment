package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// authed checks the Bearer API key and passes the owning business id to the handler.
func (a *App) authed(h func(http.ResponseWriter, *http.Request, uuid.UUID)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		hash := sha256.Sum256([]byte(key))
		var biz uuid.UUID
		err := a.db.QueryRow(r.Context(),
			`SELECT business_id FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`, hash[:]).Scan(&biz)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		if err != nil {
			a.internalErr(w, err)
			return
		}
		if rec, ok := w.(*recorder); ok {
			rec.biz = biz.String()
		}
		h(w, r, biz)
	}
}

// CreateBusiness inserts a business with one API key and returns the plaintext key.
// Only prefix and sha256(key) are stored; the key is high-entropy, so a fast hash is enough.
func (a *App) CreateBusiness(ctx context.Context, name string) (string, error) {
	prefix := "sk_" + randHex(4)
	key := prefix + "_" + randHex(16)
	hash := sha256.Sum256([]byte(key))

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	bizID, keyID := newID(), newID()
	if _, err := tx.Exec(ctx, `INSERT INTO businesses (id, name) VALUES ($1, $2)`, bizID, name); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO api_keys (id, business_id, prefix, key_hash) VALUES ($1, $2, $3, $4)`,
		keyID, bizID, prefix, hash[:]); err != nil {
		return "", err
	}
	return key, tx.Commit(ctx)
}

// SeedIfEmpty creates "Demo Business" and logs its key once, but only on a fresh database.
func (a *App) SeedIfEmpty(ctx context.Context) error {
	var n int
	if err := a.db.QueryRow(ctx, `SELECT count(*) FROM businesses`).Scan(&n); err != nil || n > 0 {
		return err
	}
	key, err := a.CreateBusiness(ctx, "Demo Business")
	if err != nil {
		return err
	}
	a.log.Info("seeded demo business; this API key is shown only once", "api_key", key)
	return nil
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err)
	}
	return id
}
