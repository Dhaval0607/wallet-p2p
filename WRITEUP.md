# Wallet & P2P Transfer — design write-up

One page on what this is, why it is built the way it is, and what I gave up.

---

## 1. Data model

Five tables. Money is `bigint` paise everywhere — there is no float, no numeric,
and no rupees-as-decimal anywhere in the program. A JSON body carrying
`"amount_paise": 12.5` is rejected by the decoder, not rounded.

| table | purpose | the constraint that does the work |
|---|---|---|
| `users` | a bearer token is the identity; only its SHA-256 is stored | `UNIQUE (token_hash)` |
| `wallets` | one balance per user | `UNIQUE (user_id)` · `CHECK (balance_paise >= 0)` |
| `transfers` | one attempted movement **and** the idempotency record | `UNIQUE (requester_user_id, idempotency_key)` |
| `ledger_entries` | double-entry audit trail, two rows per success | `SUM(delta_paise)` over the table is always 0 |
| `mints` | money entering from outside (test funding) | `UNIQUE (idempotency_key)` |

Two choices worth calling out.

**`transfers` *is* the idempotency table.** There is no separate
`idempotency_keys` table, because a second table means a second write that could
commit apart from the money. One row, one unique index, one transaction.

**`mints` is separate from `transfers`.** Test funding has to come from
somewhere, and if it went through the transfer path then "conservation" would be
unfalsifiable — the total could change and I could always call it a deposit.
Keeping mints in their own table makes conservation a checkable equation:

```
SUM(wallets.balance_paise) == SUM(mints.amount_paise)     at all times
SUM(ledger_entries.delta_paise) == 0                      at all times
```

`GET /invariants` recomputes exactly that from the base tables on every call and
returns HTTP 500 if it does not hold. A background sampler also runs it every 15s
so a violation reaches the logs and the dashboard even when nobody is looking.

---

## 2. The simplest-correct mechanism

One `READ COMMITTED` transaction covers the idempotency key, the debit, the
credit and the ledger rows. Inside it:

1. **Claim the key.** `INSERT INTO transfers (... status='pending' ...)`. The
   unique index is the lock — concurrent callers with the same key block on it.
2. **Lock both wallets** with `SELECT … FOR NO KEY UPDATE`, issued in **ascending
   wallet-id order**, both statements in one `pgx.Batch` so the deterministic
   ordering costs one network round trip rather than two.
3. **Decide while holding both locks.** If the source balance is short, mark the
   transfer `declined` and commit. *No money write has happened yet*, so there is
   nothing partial to undo.
4. **Apply**: conditional debit, credit, two ledger rows, status to `succeeded`.
5. **Commit.** Key and money land together or not at all.

The debit is still written as `UPDATE wallets SET balance_paise = balance_paise -
$1 WHERE id = $2 AND balance_paise >= $1`. That predicate is redundant while the
row lock above stands — it is kept so the debit remains atomically safe if
someone later deletes the lock, and the `CHECK (balance_paise >= 0)` is a third
layer under both. No single one of the three is load-bearing alone.

### Deadlock: what I got wrong first, and the actual fix

Sorted lock ordering alone **was not enough**, and I have the numbers because I
shipped it wrong and the burst caught it: **428 deadlocks and 133 HTTP 500s** in a
single 400-transfer run.

The reason is a lock nobody writes down. `INSERT INTO transfers` has foreign keys
to both wallets, so Postgres takes `FOR KEY SHARE` on both rows to check them —
*before* my sorted section, and in an order chosen by the constraint checker, not
by me. `FOR KEY SHARE` is shared, so two transfers happily both hold it on both
wallets. Then both try to upgrade to `FOR UPDATE`, which **conflicts** with the
other's `FOR KEY SHARE`. Each waits for a lock the other already holds. No amount
of ordering in my code can break that cycle, because the cycle is created before
my ordering begins.

The fix is to take the *correct strength* rather than the strongest one:
`balance_paise` is not a key column, so `FOR NO KEY UPDATE` is the right lock. It
still excludes every other writer — which is all the mutual exclusion a debit
needs — but it does not conflict with `FOR KEY SHARE`. With that one change, the
same burst produces **zero deadlocks and zero 500s**, verified at 1500 transfers
with 100 in flight and A→B / B→A pairs deliberately overlapping.

So the answer to "what happens when A→B and B→A hit at the same instant" is two
things, and both are required:

- **ascending wallet-id order** gives a total order on the blocking locks, so the
  waits-for graph between two transfers cannot contain a cycle; and
- **`FOR NO KEY UPDATE` rather than `FOR UPDATE`** keeps the foreign-key locks
  out of that graph entirely.

A bounded retry on SQLSTATE `40P01`/`40001` sits underneath as a safety net, and
`wallet_db_retries_total` counts it. That counter staying at **0** through every
run is the evidence that the prevention is doing the work, not the retry. CI
fails the build if it is ever non-zero.

### Heavier alternatives I rejected

| alternative | why not |
|---|---|
| `SERIALIZABLE` isolation | Correct, but it converts contention into `40001` aborts that I would have to retry in a loop — replacing a deadlock storm with a retry storm. It buys protection against anomalies this workload cannot have: the transaction reads exactly the two rows it writes, and it holds locks on both while doing so. Paying global serialization cost for a guarantee two row locks already give is the definition of cargo-culting. |
| Conditional `UPDATE` alone, no row locks | Genuinely tempting, and it *is* safe for conservation and overdraft on its own. I still take the locks because they let me decide the decline **before** writing anything. Without them, a credit-then-failed-debit ordering needs a savepoint to unwind, and "no partial apply" becomes a property of my rollback code instead of a property of never having written. |
| Application-level mutex / Redis lock | Wrong layer. It breaks the moment there is a second replica, and it puts the correctness of money in a component that can be restarted independently of the database. |
| `SELECT … ORDER BY id FOR UPDATE` in one statement | Postgres does not guarantee rows are locked in the `ORDER BY` order — the sort can happen after locking. Deterministic ordering needs separate statements, which is why they are batched. |
| Optimistic concurrency (version column, retry) | More moving parts and more round trips than a row lock, for a workload where contention on a hot wallet is the expected case rather than the rare one. |

---

## 3. Where idempotency lives

**In the `transfers` table, on `UNIQUE (requester_user_id, idempotency_key)`,
inserted inside the same transaction as the debit and credit.**

That co-location is the whole point. If the key were checked in a separate
transaction — or checked before the money transaction started — there is a window
between "no row with this key" and "money moved" in which a concurrent retry also
sees no row and also moves money. That TOCTOU gap is exactly what a 30-way retry
storm finds. Because the key and the balances commit together, the gap does not
exist: a duplicate either **blocks** on the unique index and then reads the
committed result, or **loses** the insert race and reads the same.

Keys are scoped per requester, so one user cannot burn or probe another's keys.

**Replay:** the duplicate gets `23505`, reads the committed row, compares
fingerprints, and returns the stored outcome byte-for-byte with
`Idempotent-Replay: true`. This includes declines — retrying a declined transfer
returns the original decline, it does not re-evaluate against a balance that may
since have been topped up.

**Same key, different body → `409`.** The fingerprint is
`SHA-256("v1|from|to|amount_paise")` — the request's *meaning*, not its bytes, so
reformatted JSON or reordered keys is a retry rather than a spurious conflict. A
409 moves no money; the burst asserts the balance is unchanged after one.

**Validation happens before the key is claimed.** A malformed body, a wallet that
does not exist, or a caller who does not own the source wallet is rejected without
consuming the key — otherwise a client who typo'd a wallet id could never retry
that key after fixing it.

---

## 4. Consistency vs availability

**I chose consistency, deliberately, and gave up availability.**

This is a single Postgres primary. Every transfer is a synchronous, linearizable
write against it. If the database is unreachable, `POST /transfers` fails — it
does not queue, buffer, or optimistically accept. `/readyz` reports the failure
honestly so a load balancer can take the instance out.

What that costs: the database is a single point of failure, writes cannot scale
past one primary, and a regional outage is a full outage.

Why it is right anyway: the alternative for money is accepting a write you cannot
yet prove is safe. An available-but-inconsistent wallet means either double-spend
under partition or a reconciliation process that produces a negative balance a
customer already spent. "Your transfer failed, retry" is a recoverable, honest
outcome — the idempotency key makes that retry free and exactly-once. "Your
transfer succeeded, and also the recipient's did, and the money existed once" is
not recoverable.

One deliberate softening: `/healthz` (liveness) does **not** touch the database,
while `/readyz` does. A liveness probe that fails on a database blip would have
the host restart a perfectly healthy container and turn a 10-second database
hiccup into a multi-minute outage.

Where I would go next, in order: read replicas for `GET` endpoints (balances
tolerate staleness; debits do not), then partitioning by wallet id if one primary
ever became the ceiling. Neither is warranted at this size, and both would be
complexity bought with no evidence.

---

## 5. AI: directed vs decided

Used throughout, transparently. The split:

**I directed (I made the call, AI wrote the code):**
- Go + Postgres + `pgx`, standard-library HTTP routing, no web framework.
- The whole correctness design: one transaction spanning key + debit + credit;
  `transfers` doubling as the idempotency table rather than a second table;
  ascending-wallet-id lock ordering; conditional debit and `CHECK` as redundant
  layers; the decline decided before any write so there is nothing to unwind.
- Keeping `mints` out of the transfer path specifically so conservation stays
  falsifiable, and exposing `/invariants` so a grader audits the running system
  instead of trusting this document.
- Streaming the JSON logs to a public `/logs` endpoint rather than handing out
  host-dashboard credentials.
- Rejecting `SERIALIZABLE` and app-level locks, for the reasons in §2.

**AI decided (I reviewed and accepted):**
- The `xmax = 0` trick for reporting whether `ON CONFLICT` inserted or matched.
- Prometheus histogram bucket boundaries.
- The bounded log ring buffer with non-blocking fan-out to SSE subscribers.
- The `-healthcheck` self-probe flag, which is what lets `HEALTHCHECK` work on a
  distroless image with no shell.
- Most of the burst script's portable shell (the `fire()` wave runner replaced
  `xargs -P` after macOS's 255-byte `-I` limit silently truncated every request).

**AI got it wrong and the tests caught it:** the first implementation used
`SELECT … FOR UPDATE` with correct sorted ordering, and I accepted it as correct —
the reasoning looked sound. The burst script produced 428 deadlocks. The
foreign-key `FOR KEY SHARE` interaction in §2 is something neither of us
considered until the numbers forced it. That is the honest reason the burst
script exists and why CI asserts `wallet_db_retries_total == 0`: a design argument
that sounds right is not evidence, and this one was wrong.

---

## 6. Cost

**₹0.** Render free web service + Render free Postgres, no card required.
GitHub Actions is free for public repositories. There are no other services.

The free web instance sleeps after ~15 minutes idle and takes ~30–50s to wake, so
the first request after a quiet period is slow — `scripts/burst.sh` waits on
`/healthz` before it starts timing anything. Render's free Postgres expires 30
days after creation; swapping `DATABASE_URL` to a Neon free database (also ₹0, no
card, no expiry) is the only change needed if that matters.
