-- Wallet & P2P transfer schema.
--
-- Invariant enforcement lives here first, in the database, because the database
-- is the only thing every application replica agrees on. The Go code is a client
-- of these rules, not the owner of them.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- users: a bearer token IS the identity. We store only the SHA-256 of it.
-- First use of a token provisions the user (race-free via the unique index).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash  bytea       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_token_hash_key UNIQUE (token_hash)
);

-- ---------------------------------------------------------------------------
-- wallets: one per user. UNIQUE(user_id) is what makes get-or-create race-free.
-- The CHECK is the backstop for "no overdraft": even if application logic were
-- wrong, Postgres refuses to write a negative balance.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wallets (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    balance_paise bigint      NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT wallets_user_id_key       UNIQUE (user_id),
    CONSTRAINT wallets_balance_nonneg    CHECK (balance_paise >= 0)
);

-- ---------------------------------------------------------------------------
-- transfers: also the idempotency table. There is no separate
-- "idempotency_keys" table on purpose -- one row, one unique index, written in
-- the same transaction as the money movement, so the key and the money commit
-- or roll back together. See UNIQUE (requester_user_id, idempotency_key).
--
-- request_fingerprint is a SHA-256 over the canonical request body. Same key +
-- different fingerprint => 409 Conflict, never a second debit.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS transfers (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key     text        NOT NULL,
    requester_user_id   uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    from_wallet_id      uuid        NOT NULL REFERENCES wallets (id) ON DELETE RESTRICT,
    to_wallet_id        uuid        NOT NULL REFERENCES wallets (id) ON DELETE RESTRICT,
    amount_paise        bigint      NOT NULL,
    status              text        NOT NULL,
    decline_reason      text,
    request_fingerprint bytea       NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    completed_at        timestamptz,
    CONSTRAINT transfers_idem_key       UNIQUE (requester_user_id, idempotency_key),
    CONSTRAINT transfers_amount_positive CHECK (amount_paise > 0),
    -- 'pending' exists only *inside* an uncommitted transaction. Because we
    -- never COMMIT while pending, no other session can ever observe it under
    -- READ COMMITTED -- a reader that loses the idempotency race and then reads
    -- the row always sees a terminal status.
    CONSTRAINT transfers_status_valid    CHECK (status IN ('pending', 'succeeded', 'declined')),
    CONSTRAINT transfers_no_self         CHECK (from_wallet_id <> to_wallet_id),
    CONSTRAINT transfers_decline_reason  CHECK (
        (status = 'declined' AND decline_reason IS NOT NULL) OR
        (status <> 'declined' AND decline_reason IS NULL)
    )
);

CREATE INDEX IF NOT EXISTS transfers_from_wallet_idx ON transfers (from_wallet_id, created_at DESC);
CREATE INDEX IF NOT EXISTS transfers_to_wallet_idx   ON transfers (to_wallet_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- ledger_entries: double-entry audit trail. Every succeeded transfer writes
-- exactly two rows summing to zero. This is what makes "conservation" auditable
-- after the fact rather than merely asserted: SUM(delta_paise) over the whole
-- table must always be 0.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ledger_entries (
    id          bigserial PRIMARY KEY,
    transfer_id uuid        NOT NULL REFERENCES transfers (id) ON DELETE RESTRICT,
    wallet_id   uuid        NOT NULL REFERENCES wallets (id) ON DELETE RESTRICT,
    delta_paise bigint      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_delta_nonzero CHECK (delta_paise <> 0)
);

CREATE INDEX IF NOT EXISTS ledger_wallet_idx   ON ledger_entries (wallet_id, id DESC);
CREATE INDEX IF NOT EXISTS ledger_transfer_idx ON ledger_entries (transfer_id);

-- ---------------------------------------------------------------------------
-- mints: money entering the system from outside (test funding). Kept in its own
-- table precisely so that "conservation across transfers" stays a meaningful,
-- checkable statement: total_balance == SUM(mints) at all times.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS mints (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key text        NOT NULL,
    wallet_id       uuid        NOT NULL REFERENCES wallets (id) ON DELETE RESTRICT,
    amount_paise    bigint      NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT mints_idem_key        UNIQUE (idempotency_key),
    CONSTRAINT mints_amount_positive CHECK (amount_paise > 0)
);
