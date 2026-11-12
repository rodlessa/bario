// Package db opens the PostgreSQL pool shared by the Bario services, creates
// the service's own schema and applies idempotent migrations.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/lib/pq"
)

var schemaPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Connect opens the pool, waits for PostgreSQL to accept connections (useful
// while the StatefulSet is starting) and creates the service schema.
func Connect(ctx context.Context, dsn, schema string) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	if !schemaPattern.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}

	pool, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	pool.SetMaxOpenConns(20)
	pool.SetMaxIdleConns(10)
	pool.SetConnMaxLifetime(30 * time.Minute)

	var lastErr error
	for attempt := 1; attempt <= 30; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = pool.PingContext(pingCtx)
		cancel()
		if lastErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if lastErr != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", lastErr)
	}

	if _, err := pool.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create schema %q: %w", schema, err)
	}
	return pool, nil
}

// Migrate executes the given idempotent DDL statements in order.
func Migrate(ctx context.Context, pool *sql.DB, statements ...string) error {
	for _, statement := range statements {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	return nil
}

// IsUniqueViolation reports whether err is a PostgreSQL unique constraint
// violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}
