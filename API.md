# API Reference

Base URL: `http://localhost:8090`. Request and response bodies are JSON.

## Conventions

- **Authentication:** every endpoint requires `Authorization: Bearer <api key>`. A key belongs to one business, and every resource is scoped to that business. A resource that belongs to another business is reported as `404`.
- **Money:** integer cents in USD (`*_cents` fields). There are no decimal amounts anywhere.
- **IDs:** UUIDs. **Timestamps:** RFC 3339, UTC. **Dates:** `YYYY-MM-DD`.
- **Lists:** `{"data": [...]}`, newest first. `?limit=` defaults to 50 and is capped at 100.
- **Unknown JSON fields are ignored.** In particular, a `total` sent with an invoice is dropped; the server computes the total.

## Errors

Every non-2xx response has this shape:

```json
{ "error": { "code": "invalid_state_transition", "message": "cannot pay invoice in state PAID" } }
```

`/pay` errors that concern a payment attempt (402, 502, and a 500 after the attempt was created) also carry `attempt_id` inside `error`.

| HTTP | `code` | When |
|---|---|---|
| 400 | `validation_error` | Malformed JSON, a missing or invalid field, an unknown `state` filter, a bad `limit` |
| 400 | `idempotency_key_missing` | `POST /pay` without an `Idempotency-Key` header |
| 401 | `unauthorized` | Missing, unknown or revoked API key |
| 402 | `card_declined`, `insufficient_funds`, `invalid_token` | The payment provider refused the charge. The code is the provider's own |
| 404 | `not_found` | No such resource for this business, or no such route |
| 409 | `invalid_state_transition` | The invoice's current state does not allow the action. The message names the state |
| 409 | `payment_in_progress` | A payment attempt is still pending for this invoice |
| 409 | `idempotency_request_in_progress` | Another request with the same `Idempotency-Key` has not finished |
| 422 | `idempotency_key_reused` | The `Idempotency-Key` was already used with a different request |
| 500 | `internal_error` | Unexpected server error |
| 502 | `psp_unavailable` | The payment provider errored or could not be reached. The invoice is still open |

## Objects

**Customer**

```json
{ "id": "01a0f8aa-4186-75bd-aab3-042fb8b941da", "name": "Ada Lovelace",
  "email": "ada@example.com", "created_at": "2026-10-01T18:11:42.598617Z" }
```

**Invoice**

```json
{ "id": "01a0f8aa-4195-798d-aa31-c9e8aff49dff",
  "customer_id": "01a0f8aa-4186-75bd-aab3-042fb8b941da",
  "state": "OPEN", "total_cents": 11300, "due_date": "2026-12-31",
  "created_at": "2026-10-01T18:11:42.61334Z", "updated_at": "2026-10-01T18:11:42.627701Z" }
```

`state` is one of `DRAFT`, `OPEN`, `PAID`, `VOID`, `UNCOLLECTIBLE`. See DESIGN.md §2 for the transitions.

**Invoice detail** is an invoice plus two arrays:

```json
{ "...invoice fields": "...",
  "line_items": [
    { "id": "…", "description": "Pro plan", "quantity": 2, "unit_amount_cents": 4900, "amount_cents": 9800 }
  ],
  "payment_attempts": [
    { "id": "…", "invoice_id": "…", "amount_cents": 11300, "status": "failed",
      "psp_ref": null, "failure_code": "card_declined",
      "created_at": "…", "updated_at": "…" }
  ] }
```

A payment attempt's `status` is `pending`, `succeeded` or `failed`. `psp_ref` is set when it succeeded; `failure_code` is set when it failed.

## Customers

### `POST /customers`

Request: `{ "name": "Ada Lovelace", "email": "ada@example.com" }`. Both are required.

Response `201`: the customer.

### `GET /customers/{id}`

Response `200`: the customer. `404` if it does not exist for this business.

### `GET /customers`

Response `200`: `{ "data": [customer, …] }`.

## Invoices

### `POST /invoices`

```json
{ "customer_id": "01a0f8aa-4186-75bd-aab3-042fb8b941da",
  "due_date": "2026-12-31",
  "line_items": [
    { "description": "Pro plan", "quantity": 2, "unit_amount_cents": 4900 },
    { "description": "Setup fee", "quantity": 1, "unit_amount_cents": 1500 }
  ] }
```

- `customer_id` must be a customer of this business (`404` otherwise).
- At least one line item. `quantity` is 1 to 2147483647, `unit_amount_cents` is 0 or more, and the total must be greater than zero.
- `amount_cents` per line and `total_cents` are computed by the server.

Response `201`: the invoice detail, in state `DRAFT`. Emits `invoice.created`.

### `GET /invoices/{id}`

Response `200`: the invoice detail, including every payment attempt. This is how a caller learns the outcome of a payment that returned `202`.

### `GET /invoices?state=OPEN`

Response `200`: `{ "data": [invoice, …] }` (without line items or attempts). `state` is optional; an unknown value is a `400`.

### `POST /invoices/{id}/finalize`

`DRAFT` → `OPEN`. No body. Response `200`: the invoice.

### `POST /invoices/{id}/void`

`DRAFT` or `OPEN` → `VOID`. No body. Response `200`: the invoice. `409 payment_in_progress` while an attempt is pending.

### `POST /invoices/{id}/mark-uncollectible`

`OPEN` → `UNCOLLECTIBLE`. No body. Response `200`: the invoice. `409 payment_in_progress` while an attempt is pending.

Any transition from a state not listed returns `409 invalid_state_transition`.

## Payments

### `POST /invoices/{id}/pay`

Headers: `Idempotency-Key: <any string unique to this payment>` (required).

Request: `{ "card_token": "tok_success" }`

The invoice must be `OPEN` and have no pending attempt. The service records a pending attempt, calls the payment provider, and waits up to 5 seconds for the answer.

| Outcome | Response |
|---|---|
| Charged | `200` `{ "attempt": {…status "succeeded"…}, "invoice": {…state "PAID"…} }` |
| Provider refused the card | `402` error with the provider's code and `attempt_id`. Invoice stays `OPEN` |
| Provider errored or unreachable | `502 psp_unavailable` with `attempt_id`. Invoice stays `OPEN` |
| Provider has not answered in 5 s | `202` `{ "attempt_id": "…", "status": "pending" }` |

After a `202` the charge is still being processed in the background. The invoice cannot be paid, voided or marked uncollectible until it resolves. Get the result from `GET /invoices/{id}` or from the `invoice.paid` / `invoice.payment_failed` webhook.

**Idempotency rules**

- The same key with the same request always returns the **original response**, byte for byte, without calling the provider again. This includes a stored `202`: the replay still says `pending` even after the payment has resolved, so read the outcome from `GET /invoices/{id}`.
- The same key with a different request (another body, or another invoice) returns `422 idempotency_key_reused`.
- A request rejected before any attempt was created (`404`, `409`) does not consume the key.
- To retry after a failed payment, send a **new** key.
- Keys are scoped to the business and do not expire.

## Webhooks

### `POST /webhook-endpoints`

Request: `{ "url": "https://example.com/hooks" }` (an absolute `http` or `https` URL).

Response `201`: `{ "id": "…", "url": "…", "secret": "whsec_…" }`. **The secret is returned only here.** Events created after registration are delivered to the endpoint.

### Events

| Type | When | `data` |
|---|---|---|
| `invoice.created` | An invoice is created | `{ "invoice": {…} }` |
| `invoice.paid` | A payment attempt succeeds | `{ "invoice": {…}, "attempt": {…} }` |
| `invoice.payment_failed` | A payment attempt fails, for any reason | `{ "invoice": {…}, "attempt": {…} }` |

### Delivery

A `POST` to the endpoint URL with this body:

```json
{ "id": "01a0f8aa-4216-7db7-8fd8-b0095d17b773", "type": "invoice.paid",
  "created_at": "2026-10-01T18:11:42.74Z", "data": { "invoice": {…}, "attempt": {…} } }
```

and these headers:

| Header | Value |
|---|---|
| `Webhook-Id` | The event ID (same as `id` in the body). Stable across retries |
| `Webhook-Timestamp` | Unix seconds when this delivery attempt was sent |
| `Webhook-Signature` | `v1=` + hex of HMAC-SHA256(secret, timestamp + `.` + raw body) |

- Any `2xx` within 10 seconds counts as delivered. Anything else is retried after 10 s, 1 m, 5 m, 30 m, 2 h, 6 h, 12 h and 24 h (9 attempts over about 45 hours), after which the delivery is marked dead.
- Delivery is at-least-once and unordered. Deduplicate on `Webhook-Id`, and reject timestamps more than 5 minutes old.
- README.md has a verification snippet.
