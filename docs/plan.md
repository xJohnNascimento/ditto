# Ditto: work plan

Ditto's build tasks, in order. Each task is done when its code and tests are merged with CI green, its "done when" is shown, and the design is updated in the same pull request. Which Ditto tasks Corridor waits for is in Corridor's [cross-repository plan](https://github.com/xJohnNascimento/corridor/blob/main/docs/plan.md).

**Status:** D0–D5 are done. Versions are on the [Releases page](https://github.com/xJohnNascimento/ditto/releases), each with a signed image on `ghcr.io/xjohnnascimento/ditto`.

| Id | Task | Done when | Depends on |
| --- | --- | --- | --- |
| D0 | Repository and toolchain: Go module; the full OpenAPI contract, linted and the source of generated Go models, with every operation routed; Makefile, `compose.yaml` (PostgreSQL 17 and Ditto), GitHub Actions with lint, tests, an OpenAPI lint and an image build; on a release tag, push the image to GitHub Container Registry with an SBOM and a signature; README stub | A pull request runs green; a release tag publishes an image; `make up` reports Ditto healthy | none |
| D1 | Quotes, the reference registry, cash schema, `rates.yaml`, the token bucket | Same-reference concurrency tests pass; the bucket refuses past its burst with `Retry-After`; released for Corridor's quotes (C1) | D0 |
| D2 | Trades, payouts, lookups, scheduled payout execution, admin funding | One effect survives a restart and a lost response; released for Corridor's walking skeleton (P1, C2) | D1 |
| D3 | Trade close and payout cancel fences; Standard Webhooks delivery with retries; the fault framework with `TRADE_*`, `PAYOUT_FAIL`, `PAYOUT_COMMIT_DROP_REPLY`, `FX_FILL_DIFFERENCE`, `DELAY_BEFORE_EXECUTION`, `WEBHOOK_*`, `FALSE_FAILED_HINT` and `RATE_LIMIT` | The create-versus-cancel race tests pass in both orders; duplicate and refund tests pass; each fault fires exactly once on its target; released for Corridor's failure paths (C3) | D2 |
| D4 | Statements (frozen snapshots, pages, lines) and the faults `STATUS_API_LAG`, `PAYOUT_STUCK_CANCELLABLE` and `CONTRADICTORY_FINAL_HINT` | Statement balances roll forward exactly; every fault in the catalogue is reproducible; released for Corridor's reviews and verifier (C4, P2) | D3 |
| D5 | Terraform for the sandbox ([`infra/README.md`](../infra/README.md)) | `terraform validate` and `tflint` pass in CI without cloud credentials | D0 |

Corridor's provider contract suite runs against each Ditto release that Corridor pins; an unsupported guarantee fails it explicitly.
