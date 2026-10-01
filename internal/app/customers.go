package app

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Customer struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

func (a *App) createCustomer(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	var req struct{ Name, Email string }
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || !strings.Contains(req.Email, "@") {
		writeErr(w, http.StatusBadRequest, "validation_error", "name and a valid email are required")
		return
	}
	c := Customer{ID: newID(), Name: req.Name, Email: req.Email}
	err := a.db.QueryRow(r.Context(),
		`INSERT INTO customers (id, business_id, name, email) VALUES ($1, $2, $3, $4) RETURNING created_at`,
		c.ID, biz, c.Name, c.Email).Scan(&c.CreatedAt)
	if err != nil {
		a.internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (a *App) getCustomer(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "customer not found")
		return
	}
	var c Customer
	err = a.db.QueryRow(r.Context(),
		`SELECT id, name, email, created_at FROM customers WHERE id = $1 AND business_id = $2`, id, biz).
		Scan(&c.ID, &c.Name, &c.Email, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "customer not found")
		return
	}
	if err != nil {
		a.internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *App) listCustomers(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(r.Context(),
		`SELECT id, name, email, created_at FROM customers WHERE business_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT $2`, biz, limit)
	if err != nil {
		a.internalErr(w, err)
		return
	}
	defer rows.Close()
	out := []Customer{}
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.Name, &c.Email, &c.CreatedAt); err != nil {
			a.internalErr(w, err)
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		a.internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
