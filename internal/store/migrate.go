package store

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Migrate applies the schema. It is a single idempotent CREATE-IF-NOT-EXISTS
// script rather than a versioned migration tool: for a service with one table
// group and no production history to preserve, a migration framework is a
// dependency and an operational surface bought for nothing. It runs inside one
// transaction, and concurrent replicas starting at once are serialized by
// Postgres' own catalog locks, so a rolling deploy cannot half-apply it.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
