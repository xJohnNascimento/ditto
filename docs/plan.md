# Ditto: work plan

Ditto's build tasks, in order. Each task is done when its code and tests are merged with CI green, its "done when" is shown, and the design is updated in the same pull request. Which Ditto releases Corridor waits for is in Corridor's [cross-repository plan](https://github.com/xJohnNascimento/corridor/blob/main/docs/plan.md).

**Status:** D0–D5 are done; their release tags are cut when the repository is published.

| Id | Task | Done when | Depends on |
| --- | --- | --- | --- |
| D0 | Repository and toolchain: Go module; the full OpenAPI contract, linted and the source of generated Go models, with every operation routed; Makefile, `compose.yaml` (PostgreSQL 17 and Ditto), GitHub Actions with lint, tests, an OpenAPI lint and an image build; on a release tag, push the image to GitHub Container Registry with an SBOM and a signature; README stub | A pull request runs green; tag `v0.0.1` publishes an image; `make up` reports Ditto healthy | none |
| D1 | Quotes, the reference registry, cash schema, `rates.yaml`, the token bucket | Same-reference concurrency tests pass; the bucket refuses past its burst with `Retry-After`; release `v0.1.0` (Corridor's quotes need it) | D0 |
| D2 | Trades, payouts, lookups, scheduled payout execution, admin funding | One effect survives a restart and a lost response; release `v0.2.0` (Corridor's walking skeleton needs it) | D1 |
| D3 | Trade close and payout cancel fences; Standard Webhooks delivery with retries; the fault framework with `TRADE_*`, `PAYOUT_FAIL`, `PAYOUT_COMMIT_DROP_REPLY`, `FX_FILL_DIFFERENCE`, `DELAY_BEFORE_EXECUTION`, `WEBHOOK_*`, `FALSE_FAILED_HINT` and `RATE_LIMIT` | The create-versus-cancel race tests pass in both orders; duplicate and refund tests pass; each fault fires exactly once on its target; release `v0.3.0` | D2 |
| D4 | Statements (frozen snapshots, pages, lines) and the faults `STATUS_API_LAG`, `PAYOUT_STUCK_CANCELLABLE` and `CONTRADICTORY_FINAL_HINT` | Statement balances roll forward exactly; every fault in the catalogue is reproducible; release `v0.4.0` | D3 |
| D5 | Terraform for the sandbox ([`infra/README.md`](../infra/README.md)) | `terraform validate` and `tflint` pass in CI without cloud credentials | D0 |

Corridor's provider contract suite runs against each Ditto release that Corridor pins; an unsupported guarantee fails it explicitly.
