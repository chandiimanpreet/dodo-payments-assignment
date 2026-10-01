# AI Usage

AI wrote most of the code and the first draft of every document in this repo, including this one. I made the design decisions, and the sections below say which ones were mine and which were the AI's.

## Tools and what I used them for

- **Claude Code (desktop app) with Claude Opus 5.5**, for planning and review:
  - read the assignment and laid out options for each design question, with a recommendation; I picked;
  - wrote the implementation plan the code was built from, including a log of who decided what;
  - compared that plan against the assignment line by line before any code was written (this is where the biggest correction below came from);
  - reviewed the generated code and re-ran the tests and curl flows;
  - drafted DESIGN.md, README.md and API.md from the plan and the running code.
- **Claude Code with Claude Sonnet 5.5**, run as a sub-agent of the session above, for the implementation: the Go code, the migration, the Dockerfile and compose file, and the three required tests. It built and tested everything through Docker and made one commit per phase.

I used no other AI tools on this assignment.

## Three decisions I made myself

**1. No payment-pending state on the invoice.**
- AI proposed: a `PROCESSING` (payment-pending) invoice state to act as the lock while a payment is in flight. It laid this out as one of two options.
- I chose: the other option. The invoice has five states only (DRAFT, OPEN, PAID, VOID, UNCOLLECTIBLE), and "a payment is in flight" is recorded on the payment attempt (`pending`, `succeeded`, `failed`).
- Why:
  - **They are two different things.** The invoice state answers "what is the business status of this invoice?". A pending charge is a fact about one attempt to pay it. An invoice that is being charged is still simply unpaid, so it stays OPEN.
  - **An invoice has many attempts.** With a pending state on the invoice, three declined cards and a fourth that works would bounce the invoice OPEN → PROCESSING → OPEN six times before PAID. With the status on the attempt, the invoice makes one move, OPEN → PAID, and the attempts carry the history.
  - **One fact, one place.** Storing "pending" on both the invoice and the attempt creates two values that must always agree. A crash between the two updates leaves an invoice that says a payment is pending and an attempt that says it failed.
  - **A smaller state machine** has fewer transitions to get wrong, document and test.
- The cost I accepted: the invoice state no longer acts as the lock, so preventing two simultaneous charges had to be solved separately. That is the partial unique index on pending attempts plus the conditional update on the invoice (DESIGN.md §3).

**2. The idempotency fingerprint covers the method and path, not only the body.**
- AI proposed: fingerprint the request body.
- I chose: hash the method, the path and the body together.
- Why:
  - **Every `/pay` body looks the same.** It is `{"card_token":"tok_success"}` whichever invoice is being paid, so a body-only fingerprint cannot tell two invoices apart.
  - **The failure it prevents is silent.** If a client reused a key on a second invoice, a body-only check would see a match and replay the first invoice's stored "paid" response. The client would believe the second invoice was paid when it was never charged.
  - **With the path included, that mistake is a clear error.** The request is rejected with `422 idempotency_key_reused` and nothing runs.

**3. Build the mock PSP strictly to the given spec.**
- AI proposed: extend the mock with idempotency keys and a status-lookup endpoint, and add a reconciler that uses them.
- I chose: no additions to the mock. When the outcome of a charge is unknown, the invoice is blocked rather than risking a double charge, and reconciliation is described in DESIGN.md instead of built.
- Why:
  - **The mock stands in for a system I do not control.** The assignment says "We provide the spec. You build it" and "Treat it as a real external dependency". Adding features to the provider so that my own failure handling becomes easier would change the problem I was asked to solve.
  - **The service has to be correct against the provider as specified.** A real provider may offer idempotency keys and a status lookup, or it may not. The design question is what my service does when it cannot find out, and the answer here is that it never guesses in the direction of charging twice.
  - **Restraint is graded.** A reconciler is a real feature with its own failure modes. I described it in DESIGN.md §6 and §7 instead of half-building it.
- What changed later: I accepted the AI's recommendation that an explicit PSP *error* (5xx, dropped connection) should mark the attempt failed and leave the invoice payable. Blocking now applies only when there is no evidence at all (a crash, or no answer within the timeout). See "What the AI got wrong" below.

Other calls that were mine: Go instead of Rust (decided before I asked the AI anything); a single seeded API key with a `revoked_at` check, where the AI proposed rotate and revoke endpoints; UNCOLLECTIBLE is terminal, where the AI offered Stripe-style reopening; and the webhook sink only logs, where the AI proposed one that verified signatures.

Decisions that were the AI's recommendation and that I accepted: UUIDv7 primary keys, the status-conditional update plus unique index for concurrency, the `/pay` status codes (200 / 402 / 502 / 202), and cutting idempotency-key expiry.

## What the AI got wrong

**A connection-pool deadlock in the payment handler.**

The first AI-written version of the `/pay` handler could hang under concurrent load. It opened a database transaction to claim the invoice, and on the rejection path it asked the connection pool for a second connection while still holding the first. With 20 simultaneous requests, every request held one connection and waited for another, so none could proceed.

The handler was changed to finish its lookup inside the open transaction and roll it back before touching the pool again, so a request never holds one connection while waiting for a second.

It was caught by the required concurrency test, which hung on its first run. After the fix the test passes on repeated runs and under Go's race detector.

The fix was made before that code was first committed, so it does not appear as a separate commit. It is visible in the handler itself: the comment above the claim transaction in `internal/app/pay.go` states the rule, and the `abort` function below it rolls the transaction back before releasing the key.

**Other corrections**, all made before submission:

- **A plan and a test that contradicted the assignment.** From my rule "block the invoice when a charge's outcome is unknown", the AI wrote a plan in which `tok_network_error` left the invoice blocked forever, and a test asserting exactly that. The assignment requires a test that the invoice is "not stuck in a bad state". This surfaced when I had the plan compared against the assignment line by line, before any code was written. The same plan would also have blocked an invoice forever on a mistyped card token.
- **A fourth attempt status.** One of the options the AI laid out used an `unknown` payment-attempt status for timeouts. I adopted it at first, then checked the assignment, which defines an attempt as "success, failure, or pending", and removed it.
- **Two race conditions in its earlier designs,** both found by the AI on later review passes rather than by me. A unique index on pending attempts was the only guard against double payment, and a second request could slip in just after the first attempt finished. And "refuse void while a payment is pending" was planned as one SQL statement, which can miss a payment that starts at the same instant.
- **Two smaller bugs in the generated code,** found in code review and fixed in their own commits. A client disconnecting during `/pay` could strand a pending payment, and unknown routes returned plain text instead of the JSON error format.

## How the result was checked

Run by the AI (Opus) after the code was written, in my session:

- The three required tests passed on 3 consecutive runs, and once more under Go's race detector. `go vet` is clean.
- **The concurrency test was checked for teeth.** In a scratch copy, the two guards against a second charge were removed (the unique index on pending attempts and the `state='OPEN'` re-check). The test then failed, with 19 of 20 requests returning 500 instead of 409.
- A full run by curl on an empty database: customers, invoices, every state transition, each mock card token, key replay, key reuse with a different body, and paying a PAID invoice. Every status code matched API.md.
- `tok_timeout` live: `202` after 5.0 s, `void` refused with 409 meanwhile, invoice still OPEN at 25 s and PAID by 33 s.
- One webhook signature was recomputed independently with `openssl` from the logged timestamp and body, and matched.

Not checked: webhook retries beyond the first, the `dead` state, two workers running at once, and the crash-recovery path described in DESIGN.md §3(c).

Run by me: I ran the test suite in my own terminal, both the default run and the verbose run that lists each test, and saw all three required tests pass.
