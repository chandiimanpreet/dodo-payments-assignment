-- All ids are UUIDv7 generated in Go; no DB defaults.

CREATE TABLE businesses (
    id         uuid PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id          uuid PRIMARY KEY,
    business_id uuid NOT NULL REFERENCES businesses (id),
    prefix      text NOT NULL,
    key_hash    bytea NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

CREATE TABLE customers (
    id          uuid PRIMARY KEY,
    business_id uuid NOT NULL REFERENCES businesses (id),
    name        text NOT NULL,
    email       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, business_id)
);
CREATE INDEX customers_business_created_idx ON customers (business_id, created_at DESC);

CREATE TABLE invoices (
    id          uuid PRIMARY KEY,
    business_id uuid NOT NULL REFERENCES businesses (id),
    customer_id uuid NOT NULL,
    state       text NOT NULL CHECK (state IN ('DRAFT', 'OPEN', 'PAID', 'VOID', 'UNCOLLECTIBLE')),
    total_cents bigint NOT NULL CHECK (total_cents > 0),
    due_date    date NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- the DB enforces that an invoice and its customer belong to the same business
    FOREIGN KEY (customer_id, business_id) REFERENCES customers (id, business_id)
);
CREATE INDEX invoices_business_state_created_idx ON invoices (business_id, state, created_at DESC);

CREATE TABLE invoice_line_items (
    id                uuid PRIMARY KEY,
    invoice_id        uuid NOT NULL REFERENCES invoices (id),
    description       text NOT NULL,
    quantity          int NOT NULL CHECK (quantity > 0),
    unit_amount_cents bigint NOT NULL CHECK (unit_amount_cents >= 0),
    amount_cents      bigint NOT NULL
);
CREATE INDEX invoice_line_items_invoice_idx ON invoice_line_items (invoice_id);

CREATE TABLE payment_attempts (
    id           uuid PRIMARY KEY,
    invoice_id   uuid NOT NULL REFERENCES invoices (id),
    business_id  uuid NOT NULL REFERENCES businesses (id),
    amount_cents bigint NOT NULL,
    status       text NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    psp_ref      text,
    failure_code text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
-- at most one in-flight attempt per invoice
CREATE UNIQUE INDEX payment_attempts_one_pending_idx ON payment_attempts (invoice_id) WHERE status = 'pending';
-- backstop: an invoice can never be charged successfully twice
CREATE UNIQUE INDEX payment_attempts_one_succeeded_idx ON payment_attempts (invoice_id) WHERE status = 'succeeded';

CREATE TABLE idempotency_keys (
    business_id     uuid NOT NULL REFERENCES businesses (id),
    key             text NOT NULL,
    fingerprint     bytea NOT NULL,
    status          text NOT NULL CHECK (status IN ('in_progress', 'completed')),
    response_status int,
    response_body   jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (business_id, key)
);

CREATE TABLE webhook_endpoints (
    id          uuid PRIMARY KEY,
    business_id uuid NOT NULL REFERENCES businesses (id),
    url         text NOT NULL,
    secret      text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE events (
    id          uuid PRIMARY KEY,
    business_id uuid NOT NULL REFERENCES businesses (id),
    type        text NOT NULL,
    payload     jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    id               uuid PRIMARY KEY,
    event_id         uuid NOT NULL REFERENCES events (id),
    endpoint_id      uuid NOT NULL REFERENCES webhook_endpoints (id),
    status           text NOT NULL CHECK (status IN ('pending', 'succeeded', 'dead')),
    attempt_count    int NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_status_code int,
    last_error       text,
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
