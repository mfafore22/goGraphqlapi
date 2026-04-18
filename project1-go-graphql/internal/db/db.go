// Package db wraps the pgx connection pool.
//
// Why pgx and not database/sql+lib/pq?
//   - pgx is faster (binary protocol by default, no parameter-type lookups)
//   - Better Postgres-specific type support (UUIDs, arrays, JSONB)
//   - database/sql interface is also available via stdlib adapter if we ever
//     need a library that expects *sql.DB.
package db

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is a thin alias so the rest of the codebase does not import pgx directly.
// If we swap drivers later, we change this type and the connect function only.
type Pool = pgxpool.Pool

// Connect builds a connection pool from the DATABASE_URL env var.
// Returning early on missing config is deliberate — fail fast at startup
// beats discovering bad config on the first request.
func Connect(ctx context.Context) (*Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, fmt.Errorf("DATABASE_URL not set")
	}

	// ParseConfig lets us tune pool parameters before dialing.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}

	// Tradeoff: small pool is fine for a demo, but under load you want
	// MaxConns ~= (number of CPU cores on DB host) * 2 or so. Too many
	// connections causes Postgres context-switching overhead.
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute  // recycle to avoid TLS/NAT staleness
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	// Ping once to surface auth/network errors at boot, not on first query.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
