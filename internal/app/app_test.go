package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoicesvc/internal/mockpsp"
)

var testDB *pgxpool.Pool

// TestMain uses a separate database so the running api and its webhook worker never see test data.
func TestMain(m *testing.M) {
	ctx := context.Background()
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil || u.Host == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL must point at the compose Postgres")
		os.Exit(1)
	}
	if err := ensureTestDB(ctx, u.String()); err != nil {
		fmt.Fprintln(os.Stderr, "create test database:", err)
		os.Exit(1)
	}
	u.Path = "/invoices_test"
	if err := Migrate(u.String()); err != nil {
		fmt.Fprintln(os.Stderr, "migrate test database:", err)
		os.Exit(1)
	}
	testDB, err = pgxpool.New(ctx, u.String())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	testDB.Close()
	os.Exit(code)
}

func ensureTestDB(ctx context.Context, adminURL string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'invoices_test')`).Scan(&exists); err != nil || exists {
		return err
	}
	_, err = conn.Exec(ctx, `CREATE DATABASE invoices_test`)
	return err
}

type env struct {
	t   *testing.T
	srv *httptest.Server
	psp *mockpsp.Handler
	key string
}

// newEnv runs the app in-process against a mock PSP. Each test gets its own business,
// so tests don't interfere. Durations are shrunk so the suite runs in seconds.
func newEnv(t *testing.T, fast, slow, payWait, pspTimeout time.Duration) *env {
	t.Helper()
	psp := mockpsp.New(fast, slow)
	pspSrv := httptest.NewServer(psp)
	a := New(testDB, slog.New(slog.NewTextHandler(io.Discard, nil)), pspSrv.URL, payWait, pspTimeout)
	key, err := a.CreateBusiness(context.Background(), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(func() { srv.Close(); pspSrv.Close() })
	return &env{t: t, srv: srv, psp: psp, key: key}
}

func (e *env) do(method, path, idemKey, body string) (int, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.key)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Error(err)
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) openInvoice() string {
	e.t.Helper()
	var c Customer
	status, b := e.do("POST", "/customers", "", `{"name":"Ada","email":"ada@example.com"}`)
	if status != 201 || json.Unmarshal(b, &c) != nil {
		e.t.Fatalf("create customer: %d %s", status, b)
	}
	var inv Invoice
	status, b = e.do("POST", "/invoices", "", fmt.Sprintf(
		`{"customer_id":"%s","due_date":"2030-01-01","line_items":[{"description":"x","quantity":2,"unit_amount_cents":500}]}`, c.ID))
	if status != 201 || json.Unmarshal(b, &inv) != nil {
		e.t.Fatalf("create invoice: %d %s", status, b)
	}
	if status, b = e.do("POST", "/invoices/"+inv.ID.String()+"/finalize", "", ""); status != 200 {
		e.t.Fatalf("finalize: %d %s", status, b)
	}
	return inv.ID.String()
}

func (e *env) pay(invID, idemKey, token string) (int, []byte) {
	return e.do("POST", "/invoices/"+invID+"/pay", idemKey, fmt.Sprintf(`{"card_token":%q}`, token))
}

func (e *env) invoice(invID string) invoiceDetail {
	e.t.Helper()
	status, b := e.do("GET", "/invoices/"+invID, "", "")
	var d invoiceDetail
	if status != 200 || json.Unmarshal(b, &d) != nil {
		e.t.Fatalf("get invoice: %d %s", status, b)
	}
	return d
}

func countStatus(attempts []Attempt, status string) int {
	n := 0
	for _, a := range attempts {
		if a.Status == status {
			n++
		}
	}
	return n
}

func errCode(b []byte) string {
	var r struct{ Error struct{ Code string } }
	json.Unmarshal(b, &r)
	return r.Error.Code
}

// N concurrent /pay calls on one invoice with distinct keys: exactly one charge.
func TestConcurrentPayChargesOnce(t *testing.T) {
	e := newEnv(t, 50*time.Millisecond, time.Second, 5*time.Second, 10*time.Second)
	invID := e.openInvoice()

	const n = 20
	statuses := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses[i], _ = e.pay(invID, fmt.Sprintf("key-%d", i), "tok_success")
		}()
	}
	close(start)
	wg.Wait()

	ok, conflict := 0, 0
	for _, s := range statuses {
		switch s {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("want 1x200 and %dx409, got statuses %v", n-1, statuses)
	}
	if got := e.psp.Calls("tok_success"); got != 1 {
		t.Errorf("PSP charge count = %d, want 1", got)
	}
	d := e.invoice(invID)
	if d.State != "PAID" {
		t.Errorf("invoice state = %s, want PAID", d.State)
	}
	if got := countStatus(d.PaymentAttempts, "succeeded"); got != 1 || len(d.PaymentAttempts) != 1 {
		t.Errorf("attempts = %d total, %d succeeded; want 1 and 1", len(d.PaymentAttempts), got)
	}
}

// Same key, same request: identical response, no second PSP call.
func TestIdempotentRetryReplaysResponse(t *testing.T) {
	e := newEnv(t, 50*time.Millisecond, time.Second, 5*time.Second, 10*time.Second)
	invID := e.openInvoice()

	s1, b1 := e.pay(invID, "same-key", "tok_success")
	s2, b2 := e.pay(invID, "same-key", "tok_success")
	if s1 != 200 {
		t.Fatalf("first pay = %d %s", s1, b1)
	}
	if s1 != s2 || !bytes.Equal(b1, b2) {
		t.Errorf("replay differs:\n first  %d %s\n second %d %s", s1, b1, s2, b2)
	}
	if got := e.psp.Calls("tok_success"); got != 1 {
		t.Errorf("PSP charge count = %d, want 1", got)
	}
}

// PSP failures must not leave the invoice stuck in a bad state.
func TestPSPFailureDoesNotStickInvoice(t *testing.T) {
	t.Run("network_error", func(t *testing.T) {
		e := newEnv(t, 20*time.Millisecond, time.Second, 5*time.Second, 10*time.Second)
		invID := e.openInvoice()

		status, b := e.pay(invID, "k1", "tok_network_error")
		if status != 502 || errCode(b) != "psp_unavailable" {
			t.Fatalf("pay = %d %s, want 502 psp_unavailable", status, b)
		}
		d := e.invoice(invID)
		if d.State != "OPEN" || len(d.PaymentAttempts) != 1 || d.PaymentAttempts[0].Status != "failed" {
			t.Fatalf("after failure: state %s, attempts %+v; want OPEN with 1 failed attempt", d.State, d.PaymentAttempts)
		}

		// not stuck: a new key with a good token pays the invoice
		if status, b = e.pay(invID, "k2", "tok_success"); status != 200 {
			t.Fatalf("retry pay = %d %s, want 200", status, b)
		}
		d = e.invoice(invID)
		if d.State != "PAID" || countStatus(d.PaymentAttempts, "succeeded") != 1 {
			t.Errorf("state %s, attempts %+v; want PAID with 1 succeeded attempt", d.State, d.PaymentAttempts)
		}
		if got := e.psp.Calls("tok_success"); got != 1 {
			t.Errorf("tok_success charge count = %d, want 1", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		// PSP answers after 1 s: later than the 150 ms handler wait, earlier than the 10 s client timeout
		e := newEnv(t, 20*time.Millisecond, time.Second, 150*time.Millisecond, 10*time.Second)
		invID := e.openInvoice()

		begin := time.Now()
		status, b := e.pay(invID, "k1", "tok_timeout")
		if status != 202 {
			t.Fatalf("pay = %d %s, want 202", status, b)
		}
		if time.Since(begin) > 800*time.Millisecond {
			t.Errorf("202 took %v, the handler should not wait for the PSP", time.Since(begin))
		}
		d := e.invoice(invID)
		if d.State != "OPEN" || len(d.PaymentAttempts) != 1 || d.PaymentAttempts[0].Status != "pending" {
			t.Fatalf("right after 202: state %s, attempts %+v; want OPEN with 1 pending attempt", d.State, d.PaymentAttempts)
		}
		if status, b = e.pay(invID, "k2", "tok_success"); status != 409 || errCode(b) != "payment_in_progress" {
			t.Errorf("second pay = %d %s, want 409 payment_in_progress", status, b)
		}

		deadline := time.Now().Add(10 * time.Second)
		for d.State != "PAID" && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			d = e.invoice(invID)
		}
		if d.State != "PAID" || countStatus(d.PaymentAttempts, "succeeded") != 1 || len(d.PaymentAttempts) != 1 {
			t.Errorf("final: state %s, attempts %+v; want PAID with 1 succeeded attempt", d.State, d.PaymentAttempts)
		}
		if got := e.psp.Calls("tok_timeout"); got != 1 {
			t.Errorf("tok_timeout charge count = %d, want 1", got)
		}
	})
}
