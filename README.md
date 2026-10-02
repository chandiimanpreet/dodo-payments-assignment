# Invoice & Payment Service

A small billing backend: a business creates customers and invoices, customers pay invoices through a (mock) payment provider, and the business is notified of state changes by signed webhooks.

- [DESIGN.md](DESIGN.md): data model, state machine, failure modes, webhook and API-key design.
- [API.md](API.md): every endpoint, with request and response shapes and the error format.
- [AI_USAGE.md](AI_USAGE.md): how AI was used.

## Run

```bash
docker compose up --build
```

That is the only step. It starts four containers and runs the database migrations:

| Service | What it is | Reachable at |
|---|---|---|
| `api` | The invoice service and its webhook worker | `http://localhost:8090` |
| `db` | PostgreSQL 16 | compose network only |
| `mockpsp` | The mock payment provider | compose network only |
| `webhook-sink` | Demo receiver that logs every webhook it gets | compose network only |

On first start the API seeds a "Demo Business" and logs its API key **once**. Put it in a shell variable:

```bash
export KEY=$(docker compose logs api | grep -o 'sk_[0-9a-f]*_[0-9a-f]*' | head -1)
```

To wipe everything and start again (a new key is seeded):

```bash
docker compose down -v
```

## Try it

Register the demo receiver first, so the examples below produce webhook deliveries you can see:

```bash
curl -s -X POST localhost:8090/webhook-endpoints \
  -H "Authorization: Bearer $KEY" \
  -d '{"url":"http://webhook-sink:9000/"}'
```

**1. Create a customer**

```bash
curl -s -X POST localhost:8090/customers \
  -H "Authorization: Bearer $KEY" \
  -d '{"name":"Ada Lovelace","email":"ada@example.com"}'
```

Copy the `id` from the response:

```bash
export CUSTOMER_ID=<id from the response>
```

**2. Create an invoice and finalize it**

The server computes the total (here 2 × 4900 + 1 × 1500 = 11300 cents). A new invoice is a `DRAFT`; finalizing makes it `OPEN` and payable.

```bash
curl -s -X POST localhost:8090/invoices \
  -H "Authorization: Bearer $KEY" \
  -d '{"customer_id":"'$CUSTOMER_ID'","due_date":"2026-12-31","line_items":[
        {"description":"Pro plan","quantity":2,"unit_amount_cents":4900},
        {"description":"Setup fee","quantity":1,"unit_amount_cents":1500}]}'
```

```bash
export INVOICE_ID=<id from the response>
```

```bash
curl -s -X POST localhost:8090/invoices/$INVOICE_ID/finalize \
  -H "Authorization: Bearer $KEY"
```

**3. Pay it (success)**

```bash
curl -s -X POST localhost:8090/invoices/$INVOICE_ID/pay \
  -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: pay-1" \
  -d '{"card_token":"tok_success"}'
```

This returns `200` with the succeeded attempt and the invoice in state `PAID`. Run the same command again and you get the identical response without a second charge.

**4. Pay another invoice (failure)**

Repeat step 2 to get a second open invoice, then:

```bash
curl -s -X POST localhost:8090/invoices/$INVOICE_ID/pay \
  -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: pay-2" \
  -d '{"card_token":"tok_card_declined"}'
```

This returns `402` with error code `card_declined`. The invoice stays `OPEN` and can be paid again with a **new** `Idempotency-Key`.

**5. Pay when the provider is slow (timeout)**

The invoice from step 4 is still `OPEN`, so it can be reused. `tok_timeout` makes the mock provider wait 30 seconds before it answers.

```bash
time curl -s -X POST localhost:8090/invoices/$INVOICE_ID/pay \
  -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: pay-3" \
  -d '{"card_token":"tok_timeout"}'
```

The endpoint does not hang. After about 5 seconds it returns `202` with `{"attempt_id": "…", "status": "pending"}`, and the charge carries on in the background. While it is pending, another `pay`, a `void` or a `mark-uncollectible` on this invoice returns `409 payment_in_progress`.

Wait about 25 seconds, then read the invoice:

```bash
curl -s localhost:8090/invoices/$INVOICE_ID \
  -H "Authorization: Bearer $KEY"
```

It is now `PAID`, and `payment_attempts` shows the attempt as `succeeded`. An `invoice.paid` webhook is delivered as well.

**See the webhooks**

```bash
docker compose logs webhook-sink
```

You will see `invoice.created`, `invoice.paid` and `invoice.payment_failed` deliveries with their `Webhook-Id`, `Webhook-Timestamp` and `Webhook-Signature` headers.

**Other card tokens**

| Token | What the API returns |
|---|---|
| `tok_insufficient_funds` | `402 insufficient_funds`; invoice stays `OPEN` |
| `tok_network_error` | `502 psp_unavailable`; invoice stays `OPEN` and payable |
| `tok_timeout` | `202` after 5 seconds, then `PAID` about 25 seconds later (step 5 above) |
| anything else | `402 invalid_token` |

## Verifying a webhook signature

The signature is HMAC-SHA256, keyed with the endpoint's secret (returned once by `POST /webhook-endpoints`), over the timestamp, a dot, and the raw request body:

```go
func verify(secret string, h http.Header, rawBody []byte) bool {
	ts := h.Get("Webhook-Timestamp")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(rawBody)))
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(h.Get("Webhook-Signature")))
}
```

A receiver should also reject a `Webhook-Timestamp` more than 5 minutes old and ignore a `Webhook-Id` it has already processed (delivery is at-least-once).

## Tests

```bash
docker compose --profile test run --rm --build test
```

Go does not need to be installed. The tests use a separate `invoices_test` database on the same Postgres and run the service in-process against the mock PSP with shortened delays. There are three, the ones the assignment asks for:

- **Concurrency:** 20 simultaneous `POST /pay` calls on one invoice. Exactly one succeeds, the PSP is called once, and the invoice ends `PAID` with one attempt.
- **Idempotency:** the same request with the same key returns a byte-identical response and the PSP is called once.
- **PSP failure:** after `tok_network_error` the invoice is still `OPEN` and a second payment succeeds with exactly one charge; with `tok_timeout` the endpoint answers `202` without hanging and the invoice later becomes `PAID`.

## Why Go and not Rust

The assignment prefers Rust; I chose Go because it fits what this service actually does: wait on a database and an external payment provider, and stay correct when either is slow or fails.

- **Go is the language I know best.** Payment code leaves little room for error, so I chose the language in which I can review every line with confidence and stand behind each design decision, rather than one in which I have less depth.
- **Its concurrency model keeps the payment flow simple.** The hard part of `/pay` is "call the provider in the background, answer the client after 5 seconds either way". In Go that is a goroutine, a channel and a `select` with a timer, about ten lines in `internal/app/pay.go`, with no async runtime to choose or configure.
- **The standard library covers almost everything.** HTTP server and client, routing, HMAC-SHA256 signing, secure random keys, structured logging and the test server are all built in. The only third-party code is the Postgres driver, the migration library and a UUID library, which keeps the dependency set small enough to audit.

Rust would give stronger compile-time guarantees and no garbage collector. For an I/O-bound service whose correctness lives in database transactions and constraints rather than in memory management, I judged Go's simplicity the better trade.

## Demo Video

The walkthrough is in two parts. Both open without a login.

1. [Part 1](https://drive.google.com/file/d/1-W4th1kGf8u43yFhK0aPtneAANcGSwV8/view?usp=sharing)
2. [Part 2](https://drive.google.com/file/d/10m8yWCwrL_gxJzdsDxLXyNZ_dMbPOyFI/view?usp=sharing)
