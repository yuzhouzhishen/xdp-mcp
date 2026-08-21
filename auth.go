package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"

	"github.com/golang/glog"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tokenSalt = "Ve0wNDVp"

type Authenticator struct {
	pool *pgxpool.Pool
}

func NewAuthenticator(ctx context.Context, databaseURL string) (*Authenticator, error) {
	glog.Infof("connecting to database")
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create db pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	glog.Infof("database connection established")
	return &Authenticator{pool: pool}, nil
}

func (a *Authenticator) Validate(ctx context.Context, psn string, token string) error {
	expected := DeriveToken(psn)
	if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		glog.V(1).Infof("auth rejected: psn=%s, expected=%s, got=%s (derived token mismatch)", psn, expected, token)
		return fmt.Errorf("invalid token for psn %s", psn)
	}

	var exists bool
	err := a.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM device_information WHERE psn = $1)`,
		psn,
	).Scan(&exists)
	if err != nil {
		glog.Errorf("auth psn lookup failed: psn=%s err=%v", psn, err)
		return fmt.Errorf("query psn: %w", err)
	}
	if !exists {
		glog.V(1).Infof("auth rejected: psn=%s (psn not found)", psn)
		return fmt.Errorf("invalid psn %s", psn)
	}

	glog.V(1).Infof("auth ok: psn=%s", psn)
	return nil
}

// DeriveToken calculates the request token from the device PSN.
// Algorithm:
// 1. Concatenate psn + "Ve0wNDVp"
// 2. Compute the SHA-256 digest
// 3. Hex-encode the digest to a 64-character lowercase string
// 4. Return the middle 8 characters (hex[28:36])
func DeriveToken(psn string) string {
	sum := sha256.Sum256([]byte(psn + tokenSalt))
	digest := hex.EncodeToString(sum[:])
	return digest[28:36]
}

func (a *Authenticator) Pool() *pgxpool.Pool {
	return a.pool
}

func (a *Authenticator) Close() {
	a.pool.Close()
	glog.Info("database connection closed")
}
