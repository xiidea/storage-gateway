package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"storage-gateway/internal/auth"
	"storage-gateway/internal/db"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "verify all rows decrypt without writing any changes")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	oldMasterKey := os.Getenv("MASTER_KEY_OLD")
	newMasterKey := os.Getenv("MASTER_KEY_NEW")
	databaseURL := os.Getenv("DATABASE_URL")
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}

	if oldMasterKey == "" || newMasterKey == "" || databaseURL == "" {
		fmt.Fprintln(os.Stderr, "usage: MASTER_KEY_OLD=<old> MASTER_KEY_NEW=<new> DATABASE_URL=<url> [REDIS_URL=<url>] rotate-key [--dry-run]")
		os.Exit(1)
	}

	if oldMasterKey == newMasterKey {
		log.Warn("MASTER_KEY_OLD and MASTER_KEY_NEW are identical — rows will be re-encrypted with fresh nonces but the key does not change")
	}

	oldKey, err := auth.DeriveKey(oldMasterKey)
	if err != nil {
		log.Error("deriving old key", "err", err)
		os.Exit(1)
	}
	newKey, err := auth.DeriveKey(newMasterKey)
	if err != nil {
		log.Error("deriving new key", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()

	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		log.Error("connecting to database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Redis is best-effort: rotation succeeds without it, but cache entries will
	// remain stale until their TTL expires (typically 5 minutes).
	rdb := dialRedis(ctx, log, redisURL)
	if rdb != nil {
		defer rdb.Close()
	}

	// --- Sanity probe ---
	// Before opening a transaction, confirm the old key actually decrypts at
	// least one row. This catches wrong-key mistakes before touching any data.
	if err := sanityProbe(ctx, pool, oldKey); err != nil {
		log.Error("sanity probe failed", "err", err)
		os.Exit(1)
	}

	// --- Atomic re-encryption ---
	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Error("beginning transaction", "err", err)
		os.Exit(1)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // intentional: no-op after Commit

	storeCount, err := reencryptStores(ctx, tx, oldKey, newKey, log)
	if err != nil {
		log.Error("re-encrypting stores", "err", err)
		os.Exit(1)
	}

	keyCount, err := reencryptAccessKeys(ctx, tx, oldKey, newKey, log)
	if err != nil {
		log.Error("re-encrypting access keys", "err", err)
		os.Exit(1)
	}

	if *dryRun {
		log.Info("dry-run complete — no changes written", "stores_verified", storeCount, "access_keys_verified", keyCount)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		log.Error("committing transaction", "err", err)
		os.Exit(1)
	}
	log.Info("re-encryption committed", "stores", storeCount, "access_keys", keyCount)

	// --- Redis cache flush ---
	if rdb == nil {
		log.Warn("redis unavailable — stale cache entries will expire on their own TTL; gateway errors may occur until then")
		return
	}
	flushed, err := flushSGWKeys(ctx, rdb)
	if err != nil {
		log.Warn("redis flush incomplete — some stale entries may remain", "err", err, "flushed", flushed)
		return
	}
	log.Info("redis cache flushed", "keys_deleted", flushed)
}

// sanityProbe decrypts the first store row with oldKey. Returns an error if the
// row exists but cannot be decrypted — which means oldKey is wrong or the blob
// is corrupt. A non-existent row (empty DB) is not an error.
func sanityProbe(ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, oldKey []byte) error {
	var enc []byte
	err := pool.QueryRow(ctx, `SELECT backend_config_enc FROM stores LIMIT 1`).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing to probe
	}
	if err != nil {
		return fmt.Errorf("querying stores: %w", err)
	}
	if _, err := auth.Decrypt(oldKey, enc); err != nil {
		return fmt.Errorf("cannot decrypt with MASTER_KEY_OLD — wrong key or corrupt data: %w", err)
	}
	return nil
}

func reencryptStores(ctx context.Context, tx pgx.Tx, oldKey, newKey []byte, log *slog.Logger) (int, error) {
	type row struct {
		id  uuid.UUID
		enc []byte
	}

	rows, err := tx.Query(ctx, `SELECT id, backend_config_enc FROM stores ORDER BY id FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("querying stores: %w", err)
	}
	var stores []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.enc); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning row: %w", err)
		}
		stores = append(stores, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating rows: %w", err)
	}

	for _, s := range stores {
		plain, err := auth.Decrypt(oldKey, s.enc)
		if err != nil {
			return 0, fmt.Errorf("decrypting store %s: %w", s.id, err)
		}
		newEnc, err := auth.Encrypt(newKey, plain)
		if err != nil {
			return 0, fmt.Errorf("re-encrypting store %s: %w", s.id, err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE stores SET backend_config_enc = $1 WHERE id = $2`,
			newEnc, s.id,
		); err != nil {
			return 0, fmt.Errorf("updating store %s: %w", s.id, err)
		}
		log.Debug("store re-encrypted", "store_id", s.id)
	}
	return len(stores), nil
}

func reencryptAccessKeys(ctx context.Context, tx pgx.Tx, oldKey, newKey []byte, log *slog.Logger) (int, error) {
	type row struct {
		id  uuid.UUID
		enc []byte
	}

	rows, err := tx.Query(ctx,
		`SELECT id, secret_key_enc FROM access_keys WHERE revoked_at IS NULL ORDER BY id FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("querying access keys: %w", err)
	}
	var keys []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.enc); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning row: %w", err)
		}
		keys = append(keys, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating rows: %w", err)
	}

	for _, k := range keys {
		plain, err := auth.Decrypt(oldKey, k.enc)
		if err != nil {
			return 0, fmt.Errorf("decrypting access key %s: %w", k.id, err)
		}
		newEnc, err := auth.Encrypt(newKey, plain)
		if err != nil {
			return 0, fmt.Errorf("re-encrypting access key %s: %w", k.id, err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE access_keys SET secret_key_enc = $1 WHERE id = $2`,
			newEnc, k.id,
		); err != nil {
			return 0, fmt.Errorf("updating access key %s: %w", k.id, err)
		}
		log.Debug("access key re-encrypted", "key_id", k.id)
	}
	return len(keys), nil
}

// flushSGWKeys deletes all Redis keys matching the sgw:* namespace using SCAN
// to avoid blocking the server. Returns the number of keys deleted.
func flushSGWKeys(ctx context.Context, rdb *redis.Client) (int, error) {
	var (
		cursor  uint64
		deleted int
	)
	for {
		keys, nextCursor, err := rdb.Scan(ctx, cursor, "sgw:*", 100).Result()
		if err != nil {
			return deleted, fmt.Errorf("scanning redis: %w", err)
		}
		if len(keys) > 0 {
			n, err := rdb.Del(ctx, keys...).Result()
			if err != nil {
				return deleted, fmt.Errorf("deleting keys: %w", err)
			}
			deleted += int(n)
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return deleted, nil
}

// dialRedis attempts to connect to Redis and ping it. Returns nil on failure
// rather than fataling — Redis unavailability is handled gracefully by the caller.
func dialRedis(ctx context.Context, log *slog.Logger, redisURL string) *redis.Client {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Warn("invalid REDIS_URL", "err", err)
		return nil
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Warn("cannot reach redis", "err", err)
		rdb.Close()
		return nil
	}
	return rdb
}
