# Ditto

A synthetic FX provider and payout rail. It quotes, trades, holds prefunded balances, pays beneficiaries, sends signed webhooks and publishes statements, and it fails on demand: it loses answers after committing, delays and reorders webhooks, lets its status API lag its statement, and fills trades at a different amount than quoted.

It exists so that every failure [Corridor](https://github.com/xJohnNascimento/corridor) claims to handle can be triggered, observed and checked against independent evidence. It is never deployed to production.

**Status:** Quotes, trades, payouts, webhooks, statements and fault injection are built and tested ([`docs/plan.md`](docs/plan.md)), and [`infra/`](infra/README.md) holds the Terraform for a disposable AWS test environment. Each [release](https://github.com/xJohnNascimento/ditto/releases) publishes a signed multi-architecture image to `ghcr.io/xjohnnascimento/ditto`.

## What it guarantees

- A reference identifies one request and at most one external effect.
- A close or cancel fences a reference for good, even one that does not exist yet.
- Terminal outcomes never change, and every refund happens exactly once.
- Statements are frozen snapshots whose movements explain each closing balance exactly.

## API

HTTP/JSON, described in [`openapi/ditto.yaml`](openapi/ditto.yaml): quotes, trades, payouts, statements, and a separate admin API to arm faults, fund balances and set rates. Details: [design, section 3](docs/design.md#3-api).

## Run it

To see it driving Corridor's failure scenarios, use [Corridor's quick start](https://github.com/xJohnNascimento/corridor#quick-start). On its own:

```bash
make up
```

| Command | Does |
| --- | --- |
| `docker compose exec ditto /ditto scenarios` | Prints the fault catalogue |
| `make test` | Drives the whole service over HTTP against real PostgreSQL |
| `make lint` | golangci-lint, actionlint and shellcheck, as CI runs them |
| `make openapi-check`, `make tf-validate` | Contract lint and generated-code drift, and Terraform checks, as CI runs them |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | Known vulnerabilities in dependencies, as CI runs it |

## Try it by hand

Run Ditto on its own (`make up`); it uses ports 9002 and 5433, so stop Corridor's stack first if it is running. Amounts are integers in minor units. Load the local keys from the `ditto` directory:

```bash
set -a; . ./.env; set +a
```

Fund the `corridor` account with CAD 10,000.00 through the admin API:

```bash
curl -s localhost:9002/admin/funding -H "Authorization: Bearer $DITTO_ADMIN_KEY" -H "Content-Type: application/json" -d '{"account":"corridor","reference":"fund-1","currency":"CAD","amount":"1000000"}'
```

Ask for a quote, then trade against its `quote_id` before it expires:

```bash
curl -s localhost:9002/v1/quotes -H "Authorization: Bearer $DITTO_KEY_CORRIDOR" -H "Content-Type: application/json" -d '{"client_reference":"q-1","sell_currency":"CAD","buy_currency":"USD","sell_amount":"100000"}'
```

```bash
curl -s localhost:9002/v1/trades -H "Authorization: Bearer $DITTO_KEY_CORRIDOR" -H "Content-Type: application/json" -d '{"reference":"t-1","quote_id":"QUOTE_ID","sell_currency":"CAD","buy_currency":"USD","sell_amount":"100000"}'
```

Pay out the USD it bought, then read the payout by its reference as it moves from ACCEPTED to COMPLETED:

```bash
curl -s localhost:9002/v1/payouts -H "Authorization: Bearer $DITTO_KEY_CORRIDOR" -H "Content-Type: application/json" -d '{"reference":"o-1","currency":"USD","amount":"73500","beneficiary":{"id":"b-1","name":"Northwind Traders","account":"000123456789","bank_code":"NWBKUS33","country":"US"}}'
```

```bash
curl -s localhost:9002/v1/payouts/by-reference/o-1 -H "Authorization: Bearer $DITTO_KEY_CORRIDOR"
```

Arm a fault on the next quote's reference, so the trade against it is rejected (the catalogue lists every fault and its targets):

```bash
curl -s localhost:9002/admin/scenarios -H "Authorization: Bearer $DITTO_ADMIN_KEY" -H "Content-Type: application/json" -d '{"fault":"TRADE_REJECT","target":{"kind":"quote","value":"q-2"}}'
```

Repeating a request with the same reference returns the original result; the same reference with a different body is refused. Every field is described in [`openapi/ditto.yaml`](openapi/ditto.yaml).

## Releases

Every pull request carries one label that says what merging it releases: `release: major`, `release: minor`, `release: patch` or `release: none`. Once it merges and CI passes on `main`, the next `vX.Y.Z` is tagged, with GitHub release notes and a signed multi-architecture image on GitHub Container Registry. A change that breaks the OpenAPI contract compared with the last release must be labelled `release: major`.

A tag pushed by hand builds, signs and publishes the image after CI passes, and nothing more: it gets no GitHub release.

## Documentation

| Document | Contents |
| --- | --- |
| [`docs/design.md`](docs/design.md) | Provider contract, API, webhooks, cash, faults, data model, transaction protocol, [code layout](docs/design.md#12-code-layout) |
| [`docs/plan.md`](docs/plan.md) | Build tasks D0–D5 |
| [`infra/README.md`](infra/README.md) | Terraform for a disposable AWS test environment |

## License

[MIT](LICENSE)
