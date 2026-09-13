package store

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/jackc/pgx/v5"
)

// User is an authenticated caller, identified solely by their bearer token.
type User struct {
	ID string
}

// TokenHash is the only form of a bearer token we ever persist.
func TokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// UpsertUser resolves a bearer token to a user, provisioning on first sight.
//
// Race-free by construction: ON CONFLICT ... DO UPDATE (rather than DO NOTHING)
// always produces a returned row, whether we inserted or lost the race. With DO
// NOTHING we would have to follow up with a SELECT, and that gap is exactly the
// window that makes concurrent first-use flaky.
func (s *Store) UpsertUser(ctx context.Context, token string) (User, error) {
	const q = `
		INSERT INTO users (token_hash) VALUES ($1)
		ON CONFLICT (token_hash) DO UPDATE SET token_hash = EXCLUDED.token_hash
		RETURNING id`

	var u User
	err := s.pool.QueryRow(ctx, q, TokenHash(token)).Scan(&u.ID)
	return u, err
}

// GetOrCreateWallet returns the caller's single wallet, creating it on first
// call. The `created` return distinguishes the two so we can count them apart.
//
// Correctness rests entirely on the UNIQUE (user_id) constraint plus the same
// ON CONFLICT DO UPDATE ... RETURNING trick: N concurrent callers all execute
// one statement, Postgres serializes them on the index, exactly one INSERT
// wins, and every other caller is handed the winner's row. No application-side
// check-then-insert, so there is no window to lose.
func (s *Store) GetOrCreateWallet(ctx context.Context, userID string) (w Wallet, created bool, err error) {
	const q = `
		INSERT INTO wallets (user_id) VALUES ($1)
		ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
		RETURNING id, user_id, balance_paise, created_at, (xmax = 0) AS inserted`

	err = s.pool.QueryRow(ctx, q, userID).
		Scan(&w.ID, &w.UserID, &w.BalancePaise, &w.CreatedAt, &created)
	return w, created, err
}

// GetWallet reads a wallet by id.
func (s *Store) GetWallet(ctx context.Context, id string) (Wallet, error) {
	const q = `SELECT id, user_id, balance_paise, created_at FROM wallets WHERE id = $1`

	var w Wallet
	err := s.pool.QueryRow(ctx, q, id).Scan(&w.ID, &w.UserID, &w.BalancePaise, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, ErrWalletNotFound
	}
	// An id that is not even a UUID is a lookup miss, not a server error.
	if sqlstate(err) == "22P02" {
		return Wallet{}, ErrWalletNotFound
	}
	return w, err
}

// Mint puts money into the system from outside, so that transfers have
// something to move. It is deliberately NOT a transfer: it lives in its own
// table and is admin-gated, which keeps "conservation across transfers" a
// statement that can actually be checked (total balance == total minted).
//
// Idempotent on its own key so that a retried funding step during a burst run
// cannot silently double the float and invalidate the conservation assertion.
func (s *Store) Mint(ctx context.Context, walletID, idemKey string, amountPaise int64) (Wallet, bool, error) {
	var (
		w       Wallet
		applied bool
	)

	err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		applied = false

		// Nested tx == SAVEPOINT. Needed because *any* error aborts the whole
		// Postgres transaction, so a duplicate-key we intend to swallow has to
		// be caught behind a savepoint or every later statement fails with 25P02.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		_, err = sp.Exec(ctx,
			`INSERT INTO mints (idempotency_key, wallet_id, amount_paise) VALUES ($1, $2, $3)`,
			idemKey, walletID, amountPaise)
		switch {
		case err == nil:
			applied = true
			if err = sp.Commit(ctx); err != nil {
				return err
			}
		case sqlstate(err) == sqlstateUniqueViolation:
			// Already minted under this key: release the savepoint and just
			// report the current balance.
			_ = sp.Rollback(ctx)
		case sqlstate(err) == sqlstateForeignKeyViolation, sqlstate(err) == "22P02":
			_ = sp.Rollback(ctx)
			return ErrWalletNotFound
		default:
			_ = sp.Rollback(ctx)
			return err
		}

		if applied {
			_, err = tx.Exec(ctx,
				`UPDATE wallets SET balance_paise = balance_paise + $1, updated_at = now() WHERE id = $2`,
				amountPaise, walletID)
			if err != nil {
				return err
			}
		}

		return tx.QueryRow(ctx,
			`SELECT id, user_id, balance_paise, created_at FROM wallets WHERE id = $1`, walletID).
			Scan(&w.ID, &w.UserID, &w.BalancePaise, &w.CreatedAt)
	})

	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, false, ErrWalletNotFound
	}
	return w, applied, err
}
