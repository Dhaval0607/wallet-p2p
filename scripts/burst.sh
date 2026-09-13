#!/usr/bin/env bash
#
# burst.sh -- one-command adversarial probe of the wallet service's invariants.
#
#   ./scripts/burst.sh                          # against http://localhost:8080
#   ./scripts/burst.sh https://your-app.onrender.com
#
# Every gate is reproduced against a LIVE url over plain HTTP. Exit code is 0
# only if all of them pass, so this drops straight into CI.
#
# Dependencies: bash, curl, awk, sed. No jq, no python, no node, no xargs.

set -uo pipefail

BASE_URL="${1:-${BASE_URL:-http://localhost:8080}}"
BASE_URL="${BASE_URL%/}"
ADMIN_TOKEN="${ADMIN_TOKEN:-dev-admin-token}"

# Tunables -- raise these to push harder.
N_WALLET_RACERS="${N_WALLET_RACERS:-50}"   # gate 1: concurrent get-or-create
K_IDEMPOTENT="${K_IDEMPOTENT:-30}"         # gate 2: same-key storm
N_TRANSFERS="${N_TRANSFERS:-400}"          # gate 3: concurrent transfers
N_PARALLEL="${N_PARALLEL:-50}"             # concurrency width
SEED_PAISE="${SEED_PAISE:-500000}"         # per-wallet float, in paise
N_WALLETS="${N_WALLETS:-4}"                # gate 3: wallets in the contention set

RUN_ID="burst-$(date +%s)-$$"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if [ -t 1 ]; then
  R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; B=$'\033[34m'; D=$'\033[2m'; N=$'\033[0m'
else
  R=; G=; Y=; B=; D=; N=
fi

FAILURES=0
pass() { printf "  %sPASS%s  %s\n" "$G" "$N" "$1"; }
fail() { printf "  %sFAIL%s  %s\n" "$R" "$N" "$1"; FAILURES=$((FAILURES+1)); }
info() { printf "  %s·%s     %s\n" "$D" "$N" "$1"; }
head_() { printf "\n%s== %s ==%s\n" "$B" "$1" "$N"; }

# jget FIELD < json   -- extract a flat top-level JSON string/number field.
jget() {
  awk -v k="\"$1\":" '
    { s = $0
      i = index(s, k); if (i == 0) next
      s = substr(s, i + length(k))
      if (substr(s, 1, 1) == "\"") { s = substr(s, 2); print substr(s, 1, index(s, "\"") - 1) }
      else { n = ""; for (j = 1; j <= length(s); j++) { c = substr(s, j, 1)
               if (c ~ /[-0-9]/) n = n c; else break } ; print n }
      exit }'
}

# fire CONCURRENCY < commands
# Runs the commands on stdin in simultaneous waves of CONCURRENCY.
#
# Deliberately not xargs -P: macOS caps the -I replacement string at 255 bytes,
# which silently truncates every curl here. A wave of background jobs is also a
# closer match to what we are testing -- N requests genuinely in flight at once,
# not a sliding window that never quite overlaps.
fire() {
  _max=$1; _n=0
  while IFS= read -r _cmd; do
    eval "$_cmd" &
    _n=$((_n + 1))
    if [ "$_n" -ge "$_max" ]; then wait; _n=0; fi
  done
  wait
}

api() { # api METHOD PATH TOKEN [BODY]
  if [ $# -ge 4 ]; then
    curl -sS -X "$1" "$BASE_URL$2" -H "Authorization: Bearer $3" \
         -H 'Content-Type: application/json' -H "X-Correlation-Id: $RUN_ID" -d "$4"
  else
    curl -sS -X "$1" "$BASE_URL$2" -H "Authorization: Bearer $3" -H "X-Correlation-Id: $RUN_ID"
  fi
}

printf "%s\n" "wallet-p2p invariant burst"
printf "%s  target      %s%s\n" "$D" "$BASE_URL" "$N"
printf "%s  run id      %s%s\n" "$D" "$RUN_ID" "$N"

# --------------------------------------------------------------------------
head_ "Reachability"
# --------------------------------------------------------------------------
if ! curl -fsS --max-time 30 "$BASE_URL/healthz" > "$WORK/health" 2>"$WORK/health.err"; then
  fail "cannot reach $BASE_URL/healthz -- $(cat "$WORK/health.err")"
  exit 1
fi
pass "healthz: $(cat "$WORK/health")"

READY="$(curl -sS --max-time 30 "$BASE_URL/readyz")"
case "$READY" in
  *'"ready"'*) pass "readyz: database reachable" ;;
  *) fail "readyz: $READY"; exit 1 ;;
esac

BEFORE="$(curl -sS "$BASE_URL/invariants")"
START_TOTAL="$(printf '%s' "$BEFORE" | jget total_balance_paise)"
info "system total before run: ${START_TOTAL} paise"

# ==========================================================================
head_ "GATE 1 -- race-free get-or-create ($N_WALLET_RACERS concurrent POST /wallets)"
# ==========================================================================
FRESH_TOKEN="usr_${RUN_ID}_$(date +%N 2>/dev/null || echo $RANDOM)"
info "brand-new bearer token, never seen by the server before"

: > "$WORK/goc-cmds"
n=1
while [ "$n" -le "$N_WALLET_RACERS" ]; do
  echo "curl -sS -X POST '$BASE_URL/wallets' -H 'Authorization: Bearer $FRESH_TOKEN' -H 'X-Correlation-Id: $RUN_ID-goc-$n' -o '$WORK/goc-$n.json'" >> "$WORK/goc-cmds"
  n=$((n + 1))
done
fire "$N_WALLET_RACERS" < "$WORK/goc-cmds"

for f in "$WORK"/goc-*.json; do jget id < "$f"; done | sort -u > "$WORK/goc-ids"
GOC_COUNT="$(grep -c . < "$WORK/goc-ids" || true)"
GOC_OK="$(grep -l '"balance_paise"' "$WORK"/goc-*.json 2>/dev/null | wc -l | tr -d ' ')"

info "$N_WALLET_RACERS requests fired, $GOC_OK returned a wallet"
if [ "$GOC_COUNT" = "1" ]; then
  pass "exactly one wallet id across $N_WALLET_RACERS concurrent creates: $(cat "$WORK/goc-ids")"
else
  fail "expected 1 distinct wallet id, got $GOC_COUNT:"; sed 's/^/          /' "$WORK/goc-ids"
fi
if [ "$GOC_OK" = "$N_WALLET_RACERS" ]; then
  pass "no request errored (no unhandled unique-violation 500s)"
else
  fail "only $GOC_OK/$N_WALLET_RACERS requests returned a wallet"
  grep -h -m2 'error' "$WORK"/goc-*.json 2>/dev/null | sed 's/^/          /' | head -3
fi

# --------------------------------------------------------------------------
head_ "Setup -- $N_WALLETS funded wallets for the contention gates"
# --------------------------------------------------------------------------
: > "$WORK/wallets"
: > "$WORK/tokens"
i=1
while [ "$i" -le "$N_WALLETS" ]; do
  TOK="usr_${RUN_ID}_w${i}"
  WID="$(api POST /wallets "$TOK" | jget id)"
  if [ -z "$WID" ]; then fail "could not create wallet $i"; exit 1; fi
  api POST /admin/mint "$ADMIN_TOKEN" \
      "{\"wallet_id\":\"$WID\",\"amount_paise\":$SEED_PAISE,\"idempotency_key\":\"$RUN_ID-seed-$i\"}" > /dev/null
  echo "$WID" >> "$WORK/wallets"
  echo "$TOK" >> "$WORK/tokens"
  i=$((i+1))
done

W1="$(sed -n 1p "$WORK/wallets")"; T1="$(sed -n 1p "$WORK/tokens")"
W2="$(sed -n 2p "$WORK/wallets")"; T2="$(sed -n 2p "$WORK/tokens")"
SEEDED_TOTAL=$((SEED_PAISE * N_WALLETS))
pass "$N_WALLETS wallets seeded with $SEED_PAISE paise each (total $SEEDED_TOTAL)"

sum_balances() {
  total=0
  n=1
  while [ "$n" -le "$N_WALLETS" ]; do
    w="$(sed -n "${n}p" "$WORK/wallets")"; t="$(sed -n "${n}p" "$WORK/tokens")"
    b="$(api GET "/wallets/$w" "$t" | jget balance_paise)"
    total=$((total + ${b:-0}))
    n=$((n+1))
  done
  echo "$total"
}

# ==========================================================================
head_ "GATE 2 -- idempotent exactly-once ($K_IDEMPOTENT concurrent identical transfers)"
# ==========================================================================
IDEM_KEY="$RUN_ID-exactly-once"
AMOUNT=7777
FROM_BEFORE="$(api GET "/wallets/$W1" "$T1" | jget balance_paise)"
TO_BEFORE="$(api GET "/wallets/$W2" "$T2" | jget balance_paise)"
info "same idempotency_key, same body, fired $K_IDEMPOTENT ways at once"

BODY="{\"from\":\"$W1\",\"to\":\"$W2\",\"amount_paise\":$AMOUNT,\"idempotency_key\":\"$IDEM_KEY\"}"
: > "$WORK/idem-cmds"
n=1
while [ "$n" -le "$K_IDEMPOTENT" ]; do
  echo "curl -sS -X POST '$BASE_URL/transfers' -H 'Authorization: Bearer $T1' -H 'Content-Type: application/json' -H 'X-Correlation-Id: $RUN_ID-idem-$n' -d '$BODY' -D '$WORK/idem-$n.head' -o '$WORK/idem-$n.json'" >> "$WORK/idem-cmds"
  n=$((n + 1))
done
fire "$K_IDEMPOTENT" < "$WORK/idem-cmds"

for f in "$WORK"/idem-*.json; do jget id < "$f"; done | sort -u > "$WORK/idem-ids"
IDEM_IDS="$(grep -c . < "$WORK/idem-ids" || true)"
for f in "$WORK"/idem-*.json; do jget status < "$f"; done | sort -u > "$WORK/idem-status"

FROM_AFTER="$(api GET "/wallets/$W1" "$T1" | jget balance_paise)"
TO_AFTER="$(api GET "/wallets/$W2" "$T2" | jget balance_paise)"
DEBITED=$((FROM_BEFORE - FROM_AFTER))
CREDITED=$((TO_AFTER - TO_BEFORE))

if [ "$IDEM_IDS" = "1" ]; then
  pass "all $K_IDEMPOTENT responses carry ONE transfer id: $(cat "$WORK/idem-ids")"
else
  fail "expected 1 transfer id across $K_IDEMPOTENT responses, got $IDEM_IDS"; sed 's/^/          /' "$WORK/idem-ids"
fi
if [ "$(grep -c . < "$WORK/idem-status")" = "1" ]; then
  pass "all $K_IDEMPOTENT responses report the same status: $(cat "$WORK/idem-status")"
else
  fail "responses disagree on status: $(tr '\n' ' ' < "$WORK/idem-status")"
fi
if [ "$DEBITED" = "$AMOUNT" ] && [ "$CREDITED" = "$AMOUNT" ]; then
  pass "exactly ONE debit and ONE credit applied ($AMOUNT paise), not $((K_IDEMPOTENT)) "
else
  fail "double-apply: expected -$AMOUNT/+$AMOUNT, saw -$DEBITED/+$CREDITED"
fi

REPLAYS="$(grep -li '^Idempotent-Replay:' "$WORK"/idem-*.head 2>/dev/null | wc -l | tr -d ' ')"
WORKERS=$((K_IDEMPOTENT - REPLAYS))
if [ "$WORKERS" = "1" ]; then
  pass "exactly 1 of $K_IDEMPOTENT requests did the work; $REPLAYS were served as idempotent replays"
else
  fail "$WORKERS requests claim to have done the work (expected 1); $REPLAYS flagged as replays"
fi

CONFLICT_CODE="$(curl -sS -o "$WORK/conflict.json" -w '%{http_code}' -X POST "$BASE_URL/transfers" \
  -H "Authorization: Bearer $T1" -H 'Content-Type: application/json' -H "X-Correlation-Id: $RUN_ID-conflict" \
  -d "{\"from\":\"$W1\",\"to\":\"$W2\",\"amount_paise\":$((AMOUNT + 1)),\"idempotency_key\":\"$IDEM_KEY\"}")"
if [ "$CONFLICT_CODE" = "409" ]; then
  pass "same key + different body => 409 Conflict (not a second debit)"
else
  fail "same key + different body returned HTTP $CONFLICT_CODE, expected 409: $(cat "$WORK/conflict.json")"
fi

FROM_AFTER2="$(api GET "/wallets/$W1" "$T1" | jget balance_paise)"
if [ "$FROM_AFTER2" = "$FROM_AFTER" ]; then
  pass "the 409 moved no money"
else
  fail "the conflicting request changed the balance: $FROM_AFTER -> $FROM_AFTER2"
fi

# ==========================================================================
head_ "GATE 3 -- conservation + no-overdraft under contention"
# ==========================================================================
TOTAL_BEFORE="$(sum_balances)"
info "total across the $N_WALLETS wallets before the storm: $TOTAL_BEFORE paise"
info "firing $N_TRANSFERS transfers, $N_PARALLEL at a time, including A->B and B->A simultaneously"
info "plus deliberate overdraft attempts that must decline cleanly"

# Build the work list up front so the storm is pure I/O: each line is a
# self-contained curl. Pairs alternate direction so that for every A->B in
# flight there is a B->A racing it -- the exact case that deadlocks an
# implementation with no deterministic lock ordering.
: > "$WORK/jobs"
n=1
while [ "$n" -le "$N_TRANSFERS" ]; do
  a=$(( (n % N_WALLETS) + 1 ))
  b=$(( ((n + 1) % N_WALLETS) + 1 ))
  [ "$a" = "$b" ] && b=$(( (b % N_WALLETS) + 1 ))
  # Every 4th job is reversed, so opposite-direction pairs overlap in flight.
  if [ $((n % 4)) -eq 0 ]; then tmp=$a; a=$b; b=$tmp; fi
  # Every 7th job tries to move more than any wallet holds: must be DECLINED.
  if [ $((n % 7)) -eq 0 ]; then amt=$((SEEDED_TOTAL * 2)); else amt=$(( (n * 13 % 900) + 100 )); fi

  fw="$(sed -n "${a}p" "$WORK/wallets")"; ft="$(sed -n "${a}p" "$WORK/tokens")"
  tw="$(sed -n "${b}p" "$WORK/wallets")"
  printf "%s\t%s\t%s\t%s\t%s\n" "$fw" "$tw" "$amt" "$ft" "$RUN_ID-x-$n" >> "$WORK/jobs"
  n=$((n+1))
done

awk -F'\t' -v base="$BASE_URL" -v work="$WORK" '{
  printf "curl -sS -o %s/x-%d.json -w %c%%{http_code}\\n%c -X POST %s/transfers -H %cAuthorization: Bearer %s%c -H %cContent-Type: application/json%c -H %cX-Correlation-Id: %s%c -d %c{\"from\":\"%s\",\"to\":\"%s\",\"amount_paise\":%s,\"idempotency_key\":\"%s\"}%c >> %s/codes\n", work, NR, 39, 39, base, 39, $4, 39, 39, 39, 39, $5, 39, 39, $1, $2, $3, $5, 39, work
}' "$WORK/jobs" > "$WORK/cmds"

: > "$WORK/codes"
STORM_START="$(date +%s)"
fire "$N_PARALLEL" < "$WORK/cmds"
STORM_SECS=$(( $(date +%s) - STORM_START ))

BAD_CODES="$(grep -cv '^201$' "$WORK/codes" || true)"
SUCCEEDED="$(cat "$WORK"/x-*.json 2>/dev/null | grep -o '"status":"succeeded"' | wc -l | tr -d ' ')"
DECLINED="$(cat "$WORK"/x-*.json 2>/dev/null | grep -o '"status":"declined"' | wc -l | tr -d ' ')"

info "storm finished in ${STORM_SECS}s -- $SUCCEEDED succeeded, $DECLINED declined"
if [ "$BAD_CODES" = "0" ]; then
  pass "every one of $N_TRANSFERS requests returned 201 (no 500s, no deadlock storm)"
else
  fail "$BAD_CODES/$N_TRANSFERS requests did NOT return 201"
  sort "$WORK/codes" | uniq -c | sed 's/^/          /'
fi
if [ "$DECLINED" -gt 0 ]; then
  pass "$DECLINED overdraft attempts were declined cleanly (recorded, no partial apply)"
else
  fail "expected some overdraft declines, saw none -- were the seeds too large?"
fi

TOTAL_AFTER="$(sum_balances)"
if [ "$TOTAL_AFTER" = "$TOTAL_BEFORE" ]; then
  pass "CONSERVATION: total unchanged at $TOTAL_AFTER paise across $SUCCEEDED successful transfers"
else
  fail "CONSERVATION BROKEN: $TOTAL_BEFORE -> $TOTAL_AFTER (delta $((TOTAL_AFTER - TOTAL_BEFORE)) paise)"
fi

NEG=0
n=1
while [ "$n" -le "$N_WALLETS" ]; do
  w="$(sed -n "${n}p" "$WORK/wallets")"; t="$(sed -n "${n}p" "$WORK/tokens")"
  b="$(api GET "/wallets/$w" "$t" | jget balance_paise)"
  info "wallet $n  ${b} paise"
  case "${b:-0}" in -*) NEG=$((NEG+1)) ;; esac
  n=$((n+1))
done
if [ "$NEG" = "0" ]; then
  pass "NO OVERDRAFT: no wallet is negative"
else
  fail "NO OVERDRAFT BROKEN: $NEG wallet(s) went negative"
fi

# ==========================================================================
head_ "Server-side audit (recomputed from base tables, not from our arithmetic)"
# ==========================================================================
AFTER="$(curl -sS "$BASE_URL/invariants")"
printf "  %s%s%s\n" "$D" "$AFTER" "$N"

for check in conservation_holds no_overdraft_holds all_invariants_hold; do
  case "$AFTER" in
    *"\"$check\":true"*)  pass "server reports $check = true" ;;
    *)                    fail "server reports $check = false" ;;
  esac
done

LEDGER="$(printf '%s' "$AFTER" | jget ledger_sum_paise)"
if [ "${LEDGER:-x}" = "0" ]; then
  pass "double-entry ledger nets to exactly 0"
else
  fail "double-entry ledger sums to $LEDGER, must be 0"
fi

STUCK="$(printf '%s' "$AFTER" | jget transfers_stuck_pending)"
if [ "${STUCK:-x}" = "0" ]; then
  pass "no transfer left stuck in 'pending'"
else
  fail "$STUCK transfers stuck in 'pending'"
fi

# --------------------------------------------------------------------------
head_ "Result"
# --------------------------------------------------------------------------
printf "  %slive logs      %s/logs%s\n"      "$D" "$BASE_URL" "$N"
printf "  %slive dashboard %s/dashboard%s\n" "$D" "$BASE_URL" "$N"
printf "  %smetrics        %s/metrics%s\n"   "$D" "$BASE_URL" "$N"
printf "  %sfilter the log viewer by this run: %s%s\n" "$D" "$RUN_ID" "$N"

if [ "$FAILURES" = "0" ]; then
  printf "\n%s  ALL INVARIANTS HELD  %s\n\n" "$G" "$N"
  exit 0
fi
printf "\n%s  %d CHECK(S) FAILED  %s\n\n" "$R" "$FAILURES" "$N"
exit 1
