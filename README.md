# wallet-p2p

[![ci](https://github.com/Dhaval0607/wallet-p2p/actions/workflows/ci.yml/badge.svg)](https://github.com/Dhaval0607/wallet-p2p/actions/workflows/ci.yml)

A small wallet service with peer-to-peer transfers, built so that the interesting
properties hold **under concurrency and failure**, not just on the happy path.

Money is integer paise everywhere. There is no float in this program.

## Live

| | |
|---|---|
| **API** | https://wallet-p2p-0bza.onrender.com |
| **Live logs** (public, no login) | https://wallet-p2p-0bza.onrender.com/logs |
| **Dashboard** | https://wallet-p2p-0bza.onrender.com/dashboard |
| **Metrics** | https://wallet-p2p-0bza.onrender.com/metrics |
| **Invariant audit** | https://wallet-p2p-0bza.onrender.com/invariants |
| **Design write-up** | [WRITEUP.md](WRITEUP.md) |

Reproduce every invariant against the live service in one command:

```bash
ADMIN_TOKEN=<token supplied with the submission> \
  ./scripts/burst.sh https://wallet-p2p-0bza.onrender.com
```

That token gates `POST /admin/mint`, which is test funding only: it mints play
money into a single wallet and cannot move money between wallets, so it cannot
affect any invariant this service claims. It is kept out of the repo rather than
published, since it is a live credential on a public instance.

Everything else is open without it — `/invariants`, `/metrics`, `/logs` and the
dashboard need no auth, and `make up && make burst` reproduces all three gates
locally with no token at all.

> The free instance sleeps after ~15 minutes idle and takes ~30-50s to wake. The
> burst script waits on `/healthz` before it starts timing anything, so a cold
> start shows up as a slow first request rather than a failure.

---

## The four invariants

| # | invariant | enforced by |
|---|---|---|
| 1 | **Conservation** — the sum of balances never changes across a transfer | debit + credit in one transaction; double-entry ledger that must sum to 0 |
| 2 | **No overdraft** — a balance never goes negative | row lock → check → conditional `UPDATE … WHERE balance >= amount` → `CHECK (balance_paise >= 0)` |
| 3 | **Exactly-once** — a repeated `idempotency_key` applies once | `UNIQUE (requester_user_id, idempotency_key)` committed **in the same transaction** as the money |
| 4 | **Race-free get-or-create** — two concurrent creates yield one wallet | `UNIQUE (user_id)` + `INSERT … ON CONFLICT DO UPDATE … RETURNING` |

Don't take this table's word for it — `GET /invariants` recomputes all of it from
the base tables on every call, and answers **HTTP 500** if any of it is false.

---

## Run it

```bash
docker compose up --build -d --wait      # app + postgres, one command
./scripts/burst.sh http://localhost:8080 # prove the invariants
```

`make up` and `make burst` do the same. Nothing else is required — no `.env`, no
manual migration step, no seed script.

Against the deployed service:

```bash
./scripts/burst.sh https://<your-app>.onrender.com
```

---

## The burst script

`scripts/burst.sh` is the one-command adversarial probe. Bash + curl + awk only —
no jq, no python, no node. It exits non-zero if any check fails, which is why CI
runs it too.

```
GATE 1  race-free get-or-create   50 simultaneous POST /wallets for a brand-new
                                  user  →  expect exactly ONE wallet id

GATE 2  idempotent exactly-once   30 simultaneous identical transfers, same key
                                  →  expect ONE debit, ONE credit, 30 identical
                                     responses, 29 flagged as replays
                                  →  same key + different body  →  409, no money moved

GATE 3  conservation + overdraft  400 concurrent transfers among a small wallet
                                  set, A→B and B→A deliberately overlapping,
                                  every 7th one overdrawing
                                  →  total unchanged, nothing negative, every
                                     overdraft declined cleanly, zero 500s
```

Then it asks the server to audit itself, so the result does not depend on the
script's own arithmetic.

Turn it up:

```bash
N_TRANSFERS=1500 N_PARALLEL=100 N_WALLETS=6 K_IDEMPOTENT=60 \
  ./scripts/burst.sh https://<your-app>.onrender.com
```

Every request carries `X-Correlation-Id: <run-id>-…`, so you can paste the run id
into the filter box at `/logs` and watch only your own burst stream past.

---

## API

Auth is a bearer token per user: `Authorization: Bearer <token>`. The token **is**
the identity — first use provisions the user. Auth sophistication is explicitly
not what this exercise is about.

| method | path | notes |
|---|---|---|
| `POST` | `/wallets` | get-or-create the caller's wallet. `201` created, `200` already existed |
| `GET` | `/wallets/{id}` | current balance |
| `POST` | `/transfers` | `{from, to, amount_paise, idempotency_key}` |
| `GET` | `/transfers/{id}` | transfer status |
| `POST` | `/admin/mint` | test funding, admin token. **Not** a transfer — see the write-up |
| `GET` | `/invariants` | live audit, recomputed from base tables. `500` if broken |
| `GET` | `/metrics` | Prometheus |
| `GET` | `/dashboard` | live metrics dashboard |
| `GET` | `/logs` | public live log stream |
| `GET` | `/healthz` `/readyz` | liveness (no DB) / readiness (checks DB) |

### A transfer

```bash
curl -X POST "$URL/transfers" \
  -H "Authorization: Bearer alice" \
  -H 'Content-Type: application/json' \
  -d '{"from":"<alice-wallet>","to":"<bob-wallet>","amount_paise":25000,
       "idempotency_key":"order-8123"}'
```

```json
{"id":"c50848fe-…","status":"succeeded","from":"…","to":"…",
 "amount_paise":25000,"idempotency_key":"order-8123",
 "created_at":"…","completed_at":"…"}
```

Send it again with the same key and you get the identical document plus
`Idempotent-Replay: true`. Send it with the same key and a different amount and
you get `409`.

### Status codes worth knowing

| code | when |
|---|---|
| `201` | transfer recorded — **including `status:"declined"`** |
| `409` | idempotency key reused with a different body |
| `422` | validation failed; the key was **not** consumed, so it is safe to reuse |
| `403` | you don't own the source wallet |

A decline is a successfully recorded business outcome, not an HTTP failure. It
returns `201` with `"status":"declined"` so that retrying it returns the identical
document — which a 4xx body could not do.

---

## Observability

**Logs.** Structured JSON on stdout with a correlation id on every line, echoed
back as `X-Correlation-Id`. The process also tees its own log stream into a
bounded in-memory ring buffer and serves it over SSE at `/logs` — publicly
viewable, no login, no host-dashboard credentials to share. Domain events:
`wallet_get_or_create`, `transfer_created`, `transfer_succeeded`,
`transfer_declined`, `idempotent_replay`, `idempotency_conflict`,
`transfer_rejected`, `mint`, `db_tx_retry`, `invariant_violation`.

**Metrics.** `/metrics`, plus a dashboard at `/dashboard` that renders them with
no Prometheus to run.

- rate / latency / errors: `http_requests_total`, `http_request_duration_seconds`
  (p50/p95/p99 by route), `http_requests_in_flight`
- domain: `wallet_transfers_total{outcome=succeeded|declined_insufficient_funds|idempotent_replay|idempotency_key_conflict|rejected_invalid}`,
  `wallet_transferred_paise_total`, `wallet_wallets_created_total`,
  `wallet_wallets_reused_total`
- invariants as gauges: `wallet_total_balance_paise`, `wallet_ledger_sum_paise`
  (pinned at 0), sampled every 15s
- `wallet_db_retries_total` — deadlock/serialization retries. **This staying at 0
  is the evidence the locking discipline works.** CI fails if it is not.

---

## Tests

```bash
make test    # integration tests against a real postgres, race detector on
make check   # + vet + gofmt — exactly what CI runs
```

The tests are integration tests on purpose: the invariants live in Postgres, in
unique indexes and row locks and `CHECK` constraints, so a test with a mocked
database would verify nothing that matters. `TestDrainRaceNeverOverdraws` is the
sharpest one — 40 concurrent claimants against a balance that can fund 10, and the
naive read-modify-write implementation passes every other test and fails this one.

CI additionally boots the stack from a clean checkout with one command, runs both
burst profiles against it, and asserts zero deadlock retries and zero 5xx.

---

## Layout

```
cmd/wallet/          entrypoint, graceful shutdown, invariant sampler
internal/store/      every database interaction — all invariant logic is here
  transfer.go          the transaction that is the heart of the service
  schema.sql           the schema; the constraints are the real enforcement
internal/api/        routing, middleware, handlers, public log streaming
internal/obs/        structured logging + ring buffer, Prometheus metrics
web/                 the two operator pages, embedded into the binary
scripts/burst.sh     the one-command invariant probe
```

## Container

Multi-stage, static binary on `distroless/static:nonroot` — 23 MB, no shell, runs
as uid 65532. `HEALTHCHECK` invokes the binary's own `-healthcheck` flag, which is
what lets a health probe work on an image with no curl and no wget.
