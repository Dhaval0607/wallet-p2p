// Package store owns every database interaction. All invariant-critical logic
// lives here rather than in the HTTP layer, so there is exactly one place to
// audit for correctness under concurrency.
package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors the API layer maps onto status codes.
var (
	ErrWalletNotFound      = errors.New("wallet not found")
	ErrTransferNotFound    = errors.New("transfer not found")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
	ErrSameWallet          = errors.New("from and to must differ")
	ErrNotOwner            = errors.New("caller does not own the source wallet")
)

// Postgres SQLSTATEs we react to by name instead of by string matching.
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateCheckViolation      = "23514"
	sqlstateForeignKeyViolation = "23503"
	sqlstateDeadlockDetected    = "40P01"
	sqlstateSerializationFail   = "40001"
)

// Store is a thin, dependency-free wrapper over a pgx pool.
type Store struct {
	pool *pgxpool.Pool

	// onRetry is called whenever a transaction is retried after a transient
	// Postgres error, so the caller can count it. Never nil after New.
	onRetry func(sqlstate string)
}

// New wraps an existing pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, onRetry: func(string) {}}
}

// OnRetry installs the retry observer.
func (s *Store) OnRetry(f func(sqlstate string)) {
	if f != nil {
		s.onRetry = f
	}
}

// Pool exposes the underlying pool for health checks and pool metrics.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// Wallet is a user's single balance, in integer paise. There is deliberately no
// float anywhere in this program: paise are int64 from the JSON decoder to the
// bigint column and back.
type Wallet struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	BalancePaise int64     `json:"balance_paise"`
	CreatedAt    time.Time `json:"created_at"`
}

// Transfer is one attempted money movement and its terminal outcome.
type Transfer struct {
	ID             string     `json:"id"`
	Status         string     `json:"status"`
	DeclineReason  *string    `json:"decline_reason,omitempty"`
	FromWalletID   string     `json:"from"`
	ToWalletID     string     `json:"to"`
	AmountPaise    int64      `json:"amount_paise"`
	IdempotencyKey string     `json:"idempotency_key"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// Transfer statuses.
const (
	StatusSucceeded = "succeeded"
	StatusDeclined  = "declined"

	ReasonInsufficientFunds = "insufficient_funds"
)

// TransferResult reports both the transfer and how it was arrived at, so the
// API layer can set the right status code and the right domain counter.
type TransferResult struct {
	Transfer Transfer
	// Replay is true when this call hit an existing idempotency key and
	// returned the stored outcome instead of moving money again.
	Replay bool
}

// Fingerprint is the canonical hash of a transfer's *meaning*, used to detect a
// reused idempotency key carrying a different request. It intentionally hashes
// the normalized fields rather than the raw bytes, so that reformatted JSON --
// different whitespace, different key order -- is a retry, not a conflict.
func Fingerprint(from, to string, amountPaise int64) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("v1|%s|%s|%d", from, to, amountPaise)))
	return sum[:]
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sqlstate(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// isTransient reports whether Postgres aborted us for a reason that a plain
// retry can fix. Deadlock and serialization failures are the only two: both
// mean "your transaction did nothing, try again".
func isTransient(err error) bool {
	switch sqlstate(err) {
	case sqlstateDeadlockDetected, sqlstateSerializationFail:
		return true
	}
	return false
}

const maxTxAttempts = 5

// inTx runs fn inside a transaction, retrying transient aborts with a short
// backoff. The sorted-lock-order discipline in Transfer is what *prevents*
// deadlock; this retry is the safety net that keeps a surprise from becoming a
// 500, and the wallet_db_retries_total counter is the evidence of whether the
// prevention is actually holding (it should stay at zero).
func (s *Store) inTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < maxTxAttempts; attempt++ {
		if attempt > 0 {
			s.onRetry(sqlstate(err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Millisecond):
			}
		}

		var tx pgx.Tx
		tx, err = s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			if isTransient(err) {
				continue
			}
			return err
		}

		err = fn(ctx, tx)
		if err != nil {
			_ = tx.Rollback(ctx)
			if isTransient(err) {
				continue
			}
			return err
		}

		if err = tx.Commit(ctx); err != nil {
			if isTransient(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("transaction failed after %d attempts: %w", maxTxAttempts, err)
}
