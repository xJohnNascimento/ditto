# Ditto: design

Ditto is a synthetic FX provider and payout rail. It plays the outside world for Corridor: it quotes, trades, holds Corridor's prefunded balances, pays beneficiaries, sends webhooks and publishes statements. Its job is to create real external side effects, lose answers, delay observations and keep independent financial evidence, so that every failure Corridor claims to handle can be triggered, observed and checked.

It knows nothing about the ledger's schema and shares no accounting or checking code with Corridor. How Corridor uses Ditto (references, scenarios, the independent verifier) is in Corridor's [integration contract](https://github.com/xJohnNascimento/corridor/blob/main/docs/integration.md).

## 1. Deliverable

- One `ditto` image with the subcommands `serve`, `migrate`, `scenarios` (prints the fault catalogue) and `healthcheck` (probes `/healthz`, for the container health check), published to GitHub Container Registry on every release tag.
- Its own PostgreSQL database.
- An OpenAPI 3.0.3 description in `openapi/ditto.yaml`, for the provider API and the separately authenticated admin API: linted in CI (vacuum, failing on warnings) and the source of the generated Go models. Its `info.version` is set to the release's version by hand before a release is tagged. It is 3.0.3 rather than 3.1 because the Go tooling (oapi-codegen, kin-openapi) supports 3.0 fully. The server loads it at startup and routes every operation it declares.
- Non-production only: `serve` refuses to start unless `DITTO_PROFILE` is `local`, `ci` or `sandbox`, and refuses an account key equal to the admin key.
- Configuration: `deploy/accounts.yaml` names each provider account and the environment variables holding its key, its webhook endpoint and its webhook secret; `deploy/rates.yaml` holds the rates. `make up` writes random local keys to `.env`.

## 2. Provider contract

Corridor's correctness depends on these guarantees, not merely on HTTP being retryable. Each one is a case in Corridor's provider contract suite ([section 11](#11-provider-contract-suite)).

1. A reference identifies one immutable request and at most one external effect. The same reference and body return the known result; another body is a conflict. References and their tombstones are kept for the dataset's lifetime.
2. Create, execute, reject, close, cancel and scheduled work for one reference serialize on the same row. A final close or cancel prevents any later request or queued job from creating that effect.
3. Terminal economic outcomes never change. Observations may lag on purpose, but two different terminal observations for one reference are a contract breach, which Corridor detects.
4. Statements contain every committed cash movement and terminal outcome, with stable identities and balances that roll forward exactly.
5. A trade checks its quote's validity at its serialized execution decision. Quote expiry is not a substitute for resolving an already submitted reference.
6. A payout rejected, failed or cancelled after its debit is refunded exactly once. A cancelled unknown reference moves no cash.

A real provider adapter must show which of these it actually has. If its lookup or cancellation is weaker, Corridor keeps the uncertainty and the funds in review; replacing a URL is not an integration.

## 3. API

Base path `/v1`, JSON over HTTPS (HTTP locally). Each provider account authenticates with its own bearer key; the admin API uses a separate key. Amounts are canonical integer strings in minor units with explicit currencies, the same convention as Corridor; simulating a second money encoding would add little to the demonstration. Request bodies are capped at 1 MiB (413), and a path or method the contract does not declare answers the contract's JSON error (404 or 405) rather than plain text.

### 3.1 Quotes and trades

| Method and path | Request | Response | Notes |
| --- | --- | --- | --- |
| `POST /v1/quotes` | `client_reference, sell_currency, buy_currency, sell_amount` | 201 `quote_id, client_reference, sell_currency, buy_currency, sell_amount, rate_e8, buy_amount, expires_at` | A firm quote for a fixed sell amount, valid 60 s. `buy_amount = floor(sell_amount × rate_e8 / 10^8)`. Replays by `client_reference` return the same quote (200); another body is 409 |
| `POST /v1/trades` | `reference, quote_id, sell_currency, buy_currency, sell_amount` | 201 `trade_id, reference, status, sell_amount, buy_amount, rate_e8, reason?, executed_at` | `status` is `FILLED` or `REJECTED` (`LAST_LOOK`, `QUOTE_EXPIRED`, `INSUFFICIENT_BALANCE`), or `CLOSED` if the reference was closed first. A fill debits the sold currency and credits the delivered amount in one transaction. A quote fills at most once, as a real provider books a firm quote once: a trade with a new reference on a quote that already filled is 409 `quote_already_used` and moves no cash, while a rejected attempt does not consume the quote and the same reference replays its first answer |
| `GET /v1/trades/by-reference/{reference}` | | 200 the trade, or 404 | 404 means "not visible now", never "will never exist" |
| `POST /v1/trades/by-reference/{reference}/close` | | 200 `reference, status` | Returns the terminal outcome, or closes an unexecuted or unknown reference for good (`CLOSED`); serialized against create and execution |

### 3.2 Payouts

| Method and path | Request | Response | Notes |
| --- | --- | --- | --- |
| `POST /v1/payouts` | `reference, currency, amount, beneficiary {id, name, account, bank_code, country}, remittance_info` | 201 `payout_id, reference, status, sequence, currency, amount, failure_code?` | `ACCEPTED` debits the provider balance once; `REJECTED` (with an ISO 20022 code such as `AC01` or `AM04`) moves nothing. A closed reference answers its `CANCELLED` state |
| `GET /v1/payouts/by-reference/{reference}` | | 200 the payout, or 404 | Used for lookups before a re-post, for polling and for every read-back |
| `POST /v1/payouts/by-reference/{reference}/cancel` | | 200 `reference, status` | An unknown reference becomes `CANCELLED` for good; an `ACCEPTED` or not-yet-executed `PROCESSING` payout becomes `CANCELLED` and is refunded; anything else is returned unchanged |

**Payout lifecycle:** `ACCEPTED` (sequence 1) → `PROCESSING` (2) → `COMPLETED` or `FAILED` (3), or `CANCELLED` (3) before execution. `FAILED` carries an ISO 20022 reason; `AC04` (closed account) is the one Corridor maps specially. Every read returns `reference`, `currency` and `amount`, which Corridor's read-backs compare.

### 3.3 Statements

| Method and path | Request | Response |
| --- | --- | --- |
| `POST /v1/statements` | `after_statement_id?` | 201 a manifest: `statement_id, captured_at`, and per currency `opening_balance, closing_balance, from_sequence, to_sequence`, line counts and a content hash |
| `GET /v1/statements/{id}/pages?cursor=` | | 200 `lines[], next_cursor` |
| `GET /v1/statements/{id}/lines/{line_id}` | | 200 one immutable line |

A statement is captured from one `REPEATABLE READ` snapshot and frozen in `statement_line` as the exact JSON a page returns; pages read the frozen lines, never the live tables, and the manifest's content hash is a SHA-256 over those lines in order. Line ids are line numbers, a page holds 100 lines, and its cursor is the last line number read. Lines are `MOVEMENT` (sequence, currency, signed amount, balance after, kind, reference), `OUTCOME` (trade or payout reference, terminal status, amounts, finalized at; a filled trade reports the currency and amount actually delivered) and `BALANCE` (opening and closing per currency). Each currency's movements explain its closing balance exactly.

Per-currency sequences are allocated under the cash-account lock, so an uncommitted earlier movement can never appear behind a watermark already published. A statement that starts after a previous one opens at that one's closing balance and watermark, and reports only the outcomes its chain has not reported, so consecutive statements never repeat or skip a fact.

### 3.4 Admin (separate key)

| Method and path | Does |
| --- | --- |
| `POST /admin/scenarios` | Arms a fault on a target ([section 7](#7-faults)); returns the arm id |
| `GET /admin/scenarios` | Lists arms with their remaining uses and consumption evidence |
| `POST /admin/funding` | An idempotent funding movement `{account, reference, currency, amount}` (signed) on one provider account; the seed runner books the matching ledger entry |
| `POST /admin/rates` | Sets the rate for a currency pair |
| `GET /admin/requests` | Request observations and per-second counts, to check the rate budget |
| `POST /admin/reset` | Clears the dataset; refused with 403 `forbidden` unless the dataset id matches and the profile is local or CI |

## 4. Rates

`deploy/rates.yaml` lists a `rate_e8` for every ordered pair of the configured currencies (BRL, CAD, EUR and USD give 12 pairs). The values are synthetic; CAD→USD is 0.73500000 to match the worked example. `POST /admin/rates` changes a pair at runtime for a scenario; a quote keeps the rate it was given, and a restart seeds only the pairs not set yet, so a runtime change survives it.

## 5. Webhooks

Ditto sends `payout.status_changed` to Corridor's configured endpoint for every payout transition, following [Standard Webhooks](https://www.standardwebhooks.com/):

- Headers `webhook-id` (the event id, stable across retries), `webhook-timestamp` (the time of this delivery attempt) and `webhook-signature` (`v1,<base64 HMAC-SHA256 of "id.timestamp.body">`). Secrets use the Standard Webhooks form `whsec_<base64>` and the HMAC key is the decoded bytes. During a rotation the secret variable holds both secrets, space-separated, and both sign.
- Body: `event_id, type, data {payout_id, reference, status, sequence, currency, amount, failure_code}`.
- Delivery is at least once: retried with exponential backoff (2 s, doubling, capped at 1 h, however many attempts) on transport errors and non-2xx answers, for up to `DITTO_WEBHOOK_RETRY_WINDOW` (72 h; 2 minutes in the local stack). Events and their deliveries are written in the transaction of the payout transition (an outbox); a worker in every instance leases due deliveries with `SKIP LOCKED`, so a restart resumes pending deliveries and two instances never send the same copy at once. A batch is sent one delivery after another, so its lease lasts the client's timeout times the batch size plus a margin, and a claim abandoned by a crashed instance expires with its lease.
- A webhook is a hint. Corridor reads the payout back before acting on it.

## 6. Cash

Each provider account has one cash row per currency, never negative.

| Event | Movement |
| --- | --- |
| Funding | Signed amount, kind `FUNDING`, with the funding reference |
| Trade filled | Sold currency debited by the sold amount, bought currency credited by the delivered amount, in one transaction |
| Payout accepted | Debited once |
| Payout failed or cancelled after acceptance | Refunded once |

A reference's state change and the movement it causes always commit together.

## 7. Faults

A fault is armed on a target, consumed transactionally by the request it changes, and recorded with server-side proof of what actually committed. There are no global "fail the next request" switches, so concurrent tests cannot steal each other's faults.

| Target kind | Value | Known to the scenario runner when |
| --- | --- | --- |
| `quote` | Corridor's quote id (its client quote reference) | After CreateQuote, before CreatePayment: trade faults |
| `payout` | The payout id (its payout reference) | From the payment receipt, at least the cancel window before submission: payout faults |
| `account` | The provider account | Any time: rate limiting |

| Fault | What actually commits | What Corridor first sees |
| --- | --- | --- |
| `TRADE_REJECT` | A rejected trade (last look) | `REJECTED` |
| `TRADE_COMMIT_DROP_REPLY` | One filled trade | A timeout after the commit |
| `FX_FILL_DIFFERENCE {delta}` | A fill whose delivered amount differs by `delta` minor units | A `FILLED` trade with the real amount; the quote is unchanged |
| `DELAY_BEFORE_EXECUTION {delay}` | Nothing until a delayed worker takes the reference lock | A late create racing a close or cancel |
| `PAYOUT_FAIL {code}` | A payout that fails at the rail and is refunded | `FAILED` with the code |
| `PAYOUT_COMMIT_DROP_REPLY` | One accepted payout and one debit | A timeout after the commit |
| `PAYOUT_STUCK_CANCELLABLE` | A payout held in `PROCESSING`, never executed | No terminal status until a cancel |
| `STATUS_API_LAG` | A completed payout, present on the statement | Reads and webhooks still say `PROCESSING` |
| `WEBHOOK_DUPLICATE` | One event | The same event id delivered several times |
| `WEBHOOK_REORDER` | The normal progression | An earlier event delivered after a later one |
| `WEBHOOK_OMIT` | A completed payout | No webhook; polling must find it |
| `FALSE_FAILED_HINT` | A payout still processing or completed | A correctly signed `FAILED` webhook that the read-back contradicts |
| `CONTRADICTORY_FINAL_HINT` | A stable terminal fact | A second, different terminal event; Corridor raises a finding |
| `RATE_LIMIT {duration}` | Nothing for refused requests | 429 with `Retry-After` |

A dropped reply is different from a failure before execution; the dropped reply is the important ambiguous-commit case: the effect commits, then the connection closes with no response. A trade fault is consumed only by a trade it can still change: a trade on an expired quote leaves `TRADE_REJECT` and `FX_FILL_DIFFERENCE` armed, and a rejected trade keeps the quoted amount. Webhook faults change only deliveries: `WEBHOOK_DUPLICATE` and `WEBHOOK_REORDER` (default 5 s, or `delay_ms`) apply to the next event of the payout, `WEBHOOK_OMIT` to its terminal event, and `FALSE_FAILED_HINT` adds a signed `FAILED` event next to its `PROCESSING` event. Every request carries an `X-Request-Id`, and a consumption names the request that took it (or `scheduler`). Beneficiaries are synthetic ids, never magic account-number suffixes, and each scenario names its behaviour so a reviewer can reproduce it without hidden fixtures.

## 8. Rate limit

A transactional token bucket per provider account: 10 requests per second, burst 20, shared by every Ditto instance through its database. One transaction consumes any `RATE_LIMIT` arm and then runs a locked `UPDATE` that refills the bucket from the elapsed time and takes a token, so two instances can never spend the same token. Every provider endpoint counts, including lookups, closes, cancels and statement pages; admin endpoints do not. A refused request has no side effect and answers 429 with `Retry-After`. Ditto records refused requests so the scenario runner can show Corridor's retry recovery.

## 9. Data model

| Table | Key constraints |
| --- | --- |
| `quote` | Unique `(provider_account_id, client_reference)`; request hash, pair, sell amount, rate, buy amount, created and expiry times |
| `operation_reference` | PK `(provider_account_id, kind, reference)`; request hash (set once, nullable until a create arrives, so a close or cancel can fence a reference before any payload exists); state; execution version; timestamps |
| `provider_account` | PK id; the accounts that own every row below |
| `rate` | PK `(sell_currency, buy_currency)`; `rate_e8`, updated at; seeded from `rates.yaml` and changed by the admin API |
| `trade` | One per reference; quote id; pair, sold and delivered amounts, rate, rejection reason, executed at; unique `(provider_account_id, quote_id)` among trades without a rejection reason, so a quote has one fill |
| `payout` | One per reference; currency, amount, synthetic beneficiary, remittance info, failure code, due time (`infinity` for a stuck payout), finalized at, `lagged_until` (when `STATUS_API_LAG` ends); the state lives in `operation_reference` |
| `cash_account` | PK `(provider_account_id, currency)`; `balance check (balance >= 0)`; `last_sequence` |
| `cash_movement` | Unique `(provider_account_id, currency, sequence)`; signed amount, kind, balance after, effect identity; unique `(effect_id, leg)` |
| `funding` | PK `(provider_account_id, reference)`; request hash, and the cash movement it booked |
| `webhook_event` | Immutable id, operation reference, status, sequence, payload |
| `webhook_delivery` | `(event_id, copy)`, attempts, next attempt, lease, delivered at, gave up at, last error |
| `scenario_arm` | Fault, target kind and value, arguments, remaining uses |
| `scenario_consumption` | Identity key, arm, the id of the request that took a use, consumed at; written in the transaction of the effect it changed, and any number per arm and request |
| `statement`, `statement_line` | The frozen manifest and its lines, with content hash |
| `request_observation` | Request id, route, reference, received time, status, throttled flag, fault; no secrets |
| `request_budget` | PK provider account; rate, burst, tokens, updated at, throttled until; locked per admitted request |

Tombstones are states of `operation_reference`. There is deliberately no separate "cancelled references" table whose absence a racing create could check.

## 10. Transaction protocol

For every create, close, cancel and scheduled execution:

1. Insert or find the `operation_reference` row, then lock it. Under `READ COMMITTED`, read again in a new statement after a concurrent-insert conflict, so the winner is visible.
2. On a create, compare the request hash; a `CANCELLED`, `CLOSED` or `REJECTED` tombstone refuses execution even if it has no original payload.
3. Check the allowed predecessor state. A terminal state can be read and replayed, never overwritten, whatever sequence number arrives.
4. Lock the affected `cash_account` rows in `(provider_account_id, currency)` order.
5. Change cash, append the movements, advance the reference, and create webhook and work rows, all in one transaction. Commit before answering.

Scheduled jobs take the same reference lock and predecessor checks, so a job queued before a cancel can never pay after it. The scheduler runs in every instance and locks each due reference with `SKIP LOCKED`; a payout moves `ACCEPTED → PROCESSING → COMPLETED` one step per `DITTO_PAYOUT_STEP` (2 s by default), and a restart resumes from the stored state.

A create inserts its reference as `PENDING` in the same transaction that executes it, so `PENDING` is never committed; a request refused for a deterministic reason (an unknown quote, terms that differ from the quote, a quote that already filled) rolls back and leaves the reference free.

The race test arms `DELAY_BEFORE_EXECUTION`, which makes a create sleep before it claims its reference, runs a cancel during the delay, then lets the create proceed; it must be impossible for the cancel to report a permanent fence and the create to succeed afterwards. The inverse order is tested too. Both run concurrent HTTP requests against real PostgreSQL transactions.

## 11. Provider contract suite

The suite lives on the consumer side, in the `corridor` repository (`internal/provider/providertest`), because Corridor defines what it relies on. It runs in Corridor's CI against the Ditto image Corridor pins, so a Ditto release that breaks a guarantee is caught when Corridor bumps the pin, and it would run unchanged against a real provider's adapter. It covers: idempotent creates, lookup by reference, a lost create answer found by lookup, refusal of a trade on an expired quote, close and cancel fencing an unknown reference, cancel of an unexecuted payout, a processed payout left unchanged, terminal outcomes that never change, forward-only webhook sequences, signature verification on a retried delivery, and statements that roll forward exactly. A guarantee a sandbox cannot exercise is reported as skipped and listed as a launch risk.

Ditto's own tests drive the service over its HTTP API against real PostgreSQL, also call the service directly to run the scheduler and webhook delivery on demand, read the database for evidence, and use `provider.Sign` to verify signatures. They import no ledger or Corridor packages.

## 12. Code layout

```
ditto/
├── openapi/            ditto.yaml, the vacuum ruleset, embed.go
├── cmd/ditto/            serve, migrate, scenarios, healthcheck
├── deploy/               accounts.yaml, rates.yaml
├── internal/
│   ├── api/              routing and validation from the contract, keys, the request budget, handlers;
│   │                     its tests drive the whole service over HTTP against real PostgreSQL
│   ├── provider/         one transaction protocol for references, cash, quotes, trades, payouts,
│   │                     fences, faults, webhook events and delivery, statements
│   ├── config/           accounts and rates
│   └── pgtest/           a migrated template database per test binary
└── migrations/
```

## 13. Build tasks

D0 to D5 in [`plan.md`](plan.md). Ditto is done when it can independently prove what happened in every integrated scenario, including the ones where Corridor is deliberately wrong.
