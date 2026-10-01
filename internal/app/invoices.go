package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Invoice struct {
	ID         uuid.UUID `json:"id"`
	CustomerID uuid.UUID `json:"customer_id"`
	State      string    `json:"state"`
	TotalCents int64     `json:"total_cents"`
	DueDate    string    `json:"due_date"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type LineItem struct {
	ID              uuid.UUID `json:"id"`
	Description     string    `json:"description"`
	Quantity        int64     `json:"quantity"`
	UnitAmountCents int64     `json:"unit_amount_cents"`
	AmountCents     int64     `json:"amount_cents"`
}

type Attempt struct {
	ID          uuid.UUID `json:"id"`
	InvoiceID   uuid.UUID `json:"invoice_id"`
	AmountCents int64     `json:"amount_cents"`
	Status      string    `json:"status"`
	PSPRef      *string   `json:"psp_ref"`
	FailureCode *string   `json:"failure_code"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// invoiceDetail is the GET /invoices/{id} shape. The attempts are how a caller
// learns the outcome of a /pay that answered 202.
type invoiceDetail struct {
	Invoice
	LineItems       []LineItem `json:"line_items"`
	PaymentAttempts []Attempt  `json:"payment_attempts"`
}

// queryer is satisfied by both the pool and a transaction.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const invCols = `id, customer_id, state, total_cents, to_char(due_date, 'YYYY-MM-DD'), created_at, updated_at`

func scanInvoice(row pgx.Row) (Invoice, error) {
	var i Invoice
	err := row.Scan(&i.ID, &i.CustomerID, &i.State, &i.TotalCents, &i.DueDate, &i.CreatedAt, &i.UpdatedAt)
	return i, err
}

func getInvoice(ctx context.Context, q queryer, id, biz uuid.UUID) (Invoice, error) {
	return scanInvoice(q.QueryRow(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1 AND business_id = $2`, id, biz))
}

const attemptCols = `id, invoice_id, amount_cents, status, psp_ref, failure_code, created_at, updated_at`

func scanAttempt(row pgx.Row) (Attempt, error) {
	var t Attempt
	err := row.Scan(&t.ID, &t.InvoiceID, &t.AmountCents, &t.Status, &t.PSPRef, &t.FailureCode, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func (a *App) createInvoice(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	// A client-supplied "total" is ignored by the decoder; the server computes it.
	var req struct {
		CustomerID uuid.UUID `json:"customer_id"`
		DueDate    string    `json:"due_date"`
		LineItems  []struct {
			Description     string `json:"description"`
			Quantity        int64  `json:"quantity"`
			UnitAmountCents int64  `json:"unit_amount_cents"`
		} `json:"line_items"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	due, err := time.Parse("2006-01-02", req.DueDate)
	if err != nil || req.CustomerID == uuid.Nil || len(req.LineItems) == 0 {
		writeErr(w, http.StatusBadRequest, "validation_error",
			"customer_id, due_date (YYYY-MM-DD) and at least one line item are required")
		return
	}
	items := make([]LineItem, len(req.LineItems))
	var total int64
	for i, li := range req.LineItems {
		if li.Description == "" || li.Quantity < 1 || li.Quantity > math.MaxInt32 || li.UnitAmountCents < 0 {
			writeErr(w, http.StatusBadRequest, "validation_error",
				fmt.Sprintf("line_items[%d]: description required, 1 <= quantity <= 2147483647, unit_amount_cents >= 0", i))
			return
		}
		if li.UnitAmountCents > 0 && li.Quantity > math.MaxInt64/li.UnitAmountCents {
			writeErr(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("line_items[%d]: amount overflows", i))
			return
		}
		amount := li.Quantity * li.UnitAmountCents
		if total > math.MaxInt64-amount {
			writeErr(w, http.StatusBadRequest, "validation_error", "invoice total overflows")
			return
		}
		total += amount
		items[i] = LineItem{ID: newID(), Description: li.Description, Quantity: li.Quantity,
			UnitAmountCents: li.UnitAmountCents, AmountCents: amount}
	}
	if total <= 0 {
		writeErr(w, http.StatusBadRequest, "validation_error", "invoice total must be greater than zero")
		return
	}

	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		a.internalErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM customers WHERE id = $1 AND business_id = $2`, req.CustomerID, biz).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "customer not found")
		return
	}
	if err != nil {
		a.internalErr(w, err)
		return
	}
	inv, err := scanInvoice(tx.QueryRow(ctx,
		`INSERT INTO invoices (id, business_id, customer_id, state, total_cents, due_date)
		 VALUES ($1, $2, $3, 'DRAFT', $4, $5) RETURNING `+invCols, newID(), biz, req.CustomerID, total, due))
	if err != nil {
		a.internalErr(w, err)
		return
	}
	for _, li := range items {
		if _, err := tx.Exec(ctx,
			`INSERT INTO invoice_line_items (id, invoice_id, description, quantity, unit_amount_cents, amount_cents)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			li.ID, inv.ID, li.Description, li.Quantity, li.UnitAmountCents, li.AmountCents); err != nil {
			a.internalErr(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		a.internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, invoiceDetail{Invoice: inv, LineItems: items, PaymentAttempts: []Attempt{}})
}

func (a *App) getInvoiceHandler(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "invoice not found")
		return
	}
	ctx := r.Context()
	inv, err := getInvoice(ctx, a.db, id, biz)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "invoice not found")
		return
	}
	if err != nil {
		a.internalErr(w, err)
		return
	}
	d := invoiceDetail{Invoice: inv, LineItems: []LineItem{}, PaymentAttempts: []Attempt{}}

	rows, err := a.db.Query(ctx, `SELECT id, description, quantity, unit_amount_cents, amount_cents
		FROM invoice_line_items WHERE invoice_id = $1 ORDER BY id`, id)
	if err != nil {
		a.internalErr(w, err)
		return
	}
	for rows.Next() {
		var li LineItem
		if err := rows.Scan(&li.ID, &li.Description, &li.Quantity, &li.UnitAmountCents, &li.AmountCents); err != nil {
			rows.Close()
			a.internalErr(w, err)
			return
		}
		d.LineItems = append(d.LineItems, li)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		a.internalErr(w, err)
		return
	}

	rows, err = a.db.Query(ctx, `SELECT `+attemptCols+` FROM payment_attempts WHERE invoice_id = $1 ORDER BY id`, id)
	if err != nil {
		a.internalErr(w, err)
		return
	}
	for rows.Next() {
		t, err := scanAttempt(rows)
		if err != nil {
			rows.Close()
			a.internalErr(w, err)
			return
		}
		d.PaymentAttempts = append(d.PaymentAttempts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		a.internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *App) listInvoices(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", "DRAFT", "OPEN", "PAID", "VOID", "UNCOLLECTIBLE":
	default:
		writeErr(w, http.StatusBadRequest, "validation_error", "unknown state "+state)
		return
	}
	rows, err := a.db.Query(r.Context(),
		`SELECT `+invCols+` FROM invoices WHERE business_id = $1 AND ($2 = '' OR state = $2)
		 ORDER BY created_at DESC, id DESC LIMIT $3`, biz, state, limit)
	if err != nil {
		a.internalErr(w, err)
		return
	}
	defer rows.Close()
	out := []Invoice{}
	for rows.Next() {
		i, err := scanInvoice(rows)
		if err != nil {
			a.internalErr(w, err)
			return
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		a.internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (a *App) finalizeInvoice(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	a.transition(w, r, biz, "finalize", "OPEN", []string{"DRAFT"}, false)
}

func (a *App) voidInvoice(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	a.transition(w, r, biz, "void", "VOID", []string{"DRAFT", "OPEN"}, true)
}

func (a *App) markUncollectible(w http.ResponseWriter, r *http.Request, biz uuid.UUID) {
	a.transition(w, r, biz, "mark uncollectible", "UNCOLLECTIBLE", []string{"OPEN"}, true)
}

// transition moves an invoice to state `to` with a conditional UPDATE; 0 rows means
// 404 or 409. If lockFirst is set it also refuses while a payment attempt is pending.
func (a *App) transition(w http.ResponseWriter, r *http.Request, biz uuid.UUID, verb, to string, from []string, lockFirst bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "invoice not found")
		return
	}
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		a.internalErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	if lockFirst {
		// Lock the row, then check for a pending attempt as a separate statement.
		// A single "UPDATE ... AND NOT EXISTS (pending)" is unsafe under READ COMMITTED:
		// if it waits behind a /pay claim, Postgres re-checks the row but keeps the old
		// snapshot for the subquery, misses the new attempt, and voids an invoice that
		// is being charged. After the lock, the next statement sees a fresh snapshot.
		var locked uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM invoices WHERE id = $1 AND business_id = $2 FOR UPDATE`, id, biz).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "invoice not found")
			return
		}
		if err != nil {
			a.internalErr(w, err)
			return
		}
		var pending bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM payment_attempts WHERE invoice_id = $1 AND status = 'pending')`, id).Scan(&pending); err != nil {
			a.internalErr(w, err)
			return
		}
		if pending {
			writeErr(w, http.StatusConflict, "payment_in_progress", "a payment attempt is in flight for this invoice")
			return
		}
	}

	inv, err := scanInvoice(tx.QueryRow(ctx,
		`UPDATE invoices SET state = $3, updated_at = now()
		 WHERE id = $1 AND business_id = $2 AND state = ANY($4) RETURNING `+invCols, id, biz, to, from))
	if errors.Is(err, pgx.ErrNoRows) {
		cur, err := getInvoice(ctx, tx, id, biz)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "invoice not found")
			return
		}
		if err != nil {
			a.internalErr(w, err)
			return
		}
		writeErr(w, http.StatusConflict, "invalid_state_transition",
			fmt.Sprintf("cannot %s invoice in state %s", verb, cur.State))
		return
	}
	if err != nil {
		a.internalErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		a.internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}
