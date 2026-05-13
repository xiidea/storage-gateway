package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	prefixAccessKey  = "sgw:ak:"
	prefixBucket     = "sgw:bm:"
	negativeTTL      = 30 * time.Second
	negativeSentinel = "NOT_FOUND"
)

// cachedRegistry wraps a Registry and caches LookupAccessKey and ResolveBucket
// results in Redis. All Manager write methods pass through to the inner Manager
// and also invalidate affected cache entries.
type cachedRegistry struct {
	inner Manager
	rdb   *redis.Client
	ttl   time.Duration
}

// NewCached wraps inner with a Redis caching layer on the Registry read path.
// Mutations on Manager methods automatically invalidate related cache entries.
func NewCached(inner Manager, rdb *redis.Client, ttl time.Duration) Manager {
	return &cachedRegistry{inner: inner, rdb: rdb, ttl: ttl}
}

// ---------------------------------------------------------------------------
// Registry — cached read path
// ---------------------------------------------------------------------------

type cachedAccessKey struct {
	TenantID     string `json:"tid"`
	SecretKeyEnc []byte `json:"sk"`
	CreatedAt    int64  `json:"ca"`
}

type cachedBucket struct {
	StoreID          string   `json:"sid"`
	TenantID         string   `json:"tid"`
	BackendType      string   `json:"bt"`
	BackendConfigEnc []byte   `json:"bc"`
	BackendBucket    string   `json:"bb"`
	PresignedMode    string   `json:"pm"`
	AllowedOrigins   []string `json:"ao"`
}

func (c *cachedRegistry) LookupAccessKey(ctx context.Context, accessKey string) (*AccessKeyRow, error) {
	key := prefixAccessKey + accessKey

	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err == nil {
		if string(raw) == negativeSentinel {
			return nil, ErrAccessKeyNotFound
		}
		var cached cachedAccessKey
		if err := json.Unmarshal(raw, &cached); err == nil {
			tid, err := uuid.Parse(cached.TenantID)
			if err == nil {
				return &AccessKeyRow{
					TenantID:     tid,
					AccessKey:    accessKey,
					SecretKeyEnc: cached.SecretKeyEnc,
					CreatedAt:    time.Unix(cached.CreatedAt, 0),
				}, nil
			}
		}
	}

	row, err := c.inner.LookupAccessKey(ctx, accessKey)
	if errors.Is(err, ErrAccessKeyNotFound) {
		_ = c.rdb.Set(ctx, key, negativeSentinel, negativeTTL).Err()
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	if b, jerr := json.Marshal(cachedAccessKey{
		TenantID:     row.TenantID.String(),
		SecretKeyEnc: row.SecretKeyEnc,
		CreatedAt:    row.CreatedAt.Unix(),
	}); jerr == nil {
		_ = c.rdb.Set(ctx, key, b, c.ttl).Err()
	}
	return row, nil
}

func (c *cachedRegistry) ResolveBucket(ctx context.Context, tenantID uuid.UUID, gatewayBucket string) (*ResolvedBucket, error) {
	key := prefixBucket + gatewayBucket

	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err == nil {
		if string(raw) == negativeSentinel {
			return nil, ErrBucketNotFound
		}
		var cached cachedBucket
		if err := json.Unmarshal(raw, &cached); err == nil {
			storeID, serr := uuid.Parse(cached.StoreID)
			tid, terr := uuid.Parse(cached.TenantID)
			if serr == nil && terr == nil {
				if tid != tenantID {
					return nil, ErrUnauthorized
				}
				return &ResolvedBucket{
					StoreID:          storeID,
					TenantID:         tid,
					BackendType:      BackendType(cached.BackendType),
					BackendConfigEnc: cached.BackendConfigEnc,
					GatewayBucket:    gatewayBucket,
					BackendBucket:    cached.BackendBucket,
					PresignedMode:    PresignedMode(cached.PresignedMode),
					AllowedOrigins:   cached.AllowedOrigins,
				}, nil
			}
		}
	}

	rb, err := c.inner.ResolveBucket(ctx, tenantID, gatewayBucket)
	if errors.Is(err, ErrBucketNotFound) {
		_ = c.rdb.Set(ctx, key, negativeSentinel, negativeTTL).Err()
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	if b, jerr := json.Marshal(cachedBucket{
		StoreID:          rb.StoreID.String(),
		TenantID:         rb.TenantID.String(),
		BackendType:      string(rb.BackendType),
		BackendConfigEnc: rb.BackendConfigEnc,
		BackendBucket:    rb.BackendBucket,
		PresignedMode:    string(rb.PresignedMode),
		AllowedOrigins:   rb.AllowedOrigins,
	}); jerr == nil {
		_ = c.rdb.Set(ctx, key, b, c.ttl).Err()
	}
	return rb, nil
}

func (c *cachedRegistry) InvalidateAccessKey(ctx context.Context, accessKey string) error {
	return c.rdb.Del(ctx, prefixAccessKey+accessKey).Err()
}

func (c *cachedRegistry) InvalidateBucket(ctx context.Context, gatewayBucket string) error {
	return c.rdb.Del(ctx, prefixBucket+gatewayBucket).Err()
}

// ---------------------------------------------------------------------------
// Manager write methods — pass through, then invalidate
// ---------------------------------------------------------------------------

func (c *cachedRegistry) CreateTenant(ctx context.Context, name string) (*Tenant, error) {
	return c.inner.CreateTenant(ctx, name)
}

func (c *cachedRegistry) GetTenant(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	return c.inner.GetTenant(ctx, id)
}

func (c *cachedRegistry) ListTenants(ctx context.Context) ([]Tenant, error) {
	return c.inner.ListTenants(ctx)
}

func (c *cachedRegistry) CreateAccessKey(ctx context.Context, tenantID uuid.UUID, accessKey string, secretKeyEnc []byte) (*AccessKeyRow, error) {
	return c.inner.CreateAccessKey(ctx, tenantID, accessKey, secretKeyEnc)
}

func (c *cachedRegistry) ListAccessKeys(ctx context.Context, tenantID uuid.UUID) ([]AccessKeyRow, error) {
	return c.inner.ListAccessKeys(ctx, tenantID)
}

func (c *cachedRegistry) RevokeAccessKey(ctx context.Context, id, tenantID uuid.UUID) error {
	// Fetch the access key string for cache invalidation before revoking.
	keys, err := c.inner.ListAccessKeys(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := c.inner.RevokeAccessKey(ctx, id, tenantID); err != nil {
		return err
	}
	for _, k := range keys {
		if k.ID == id {
			_ = c.InvalidateAccessKey(ctx, k.AccessKey)
			break
		}
	}
	return nil
}

func (c *cachedRegistry) CreateStore(ctx context.Context, p CreateStoreParams) (*Store, error) {
	return c.inner.CreateStore(ctx, p)
}

func (c *cachedRegistry) GetStore(ctx context.Context, id uuid.UUID) (*Store, error) {
	return c.inner.GetStore(ctx, id)
}

func (c *cachedRegistry) ListStores(ctx context.Context, tenantID uuid.UUID) ([]Store, error) {
	return c.inner.ListStores(ctx, tenantID)
}

func (c *cachedRegistry) GetBucketAllowedOrigins(ctx context.Context, gatewayBucket string) ([]string, error) {
	key := prefixBucket + gatewayBucket
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err == nil && string(raw) != negativeSentinel {
		var cached cachedBucket
		if json.Unmarshal(raw, &cached) == nil {
			return cached.AllowedOrigins, nil
		}
	}
	return c.inner.GetBucketAllowedOrigins(ctx, gatewayBucket)
}

func (c *cachedRegistry) UpdateStoreBackend(ctx context.Context, id, tenantID uuid.UUID, backendConfigEnc []byte, presignedMode PresignedMode) error {
	if err := c.inner.UpdateStoreBackend(ctx, id, tenantID, backendConfigEnc, presignedMode); err != nil {
		return err
	}
	return c.invalidateBucketsForStore(ctx, id)
}

func (c *cachedRegistry) UpdateStoreAllowedOrigins(ctx context.Context, id, tenantID uuid.UUID, origins []string) error {
	if err := c.inner.UpdateStoreAllowedOrigins(ctx, id, tenantID, origins); err != nil {
		return err
	}
	return c.invalidateBucketsForStore(ctx, id)
}

func (c *cachedRegistry) DeleteStore(ctx context.Context, id, tenantID uuid.UUID) error {
	if err := c.invalidateBucketsForStore(ctx, id); err != nil {
		return fmt.Errorf("pre-delete cache invalidation: %w", err)
	}
	return c.inner.DeleteStore(ctx, id, tenantID)
}

func (c *cachedRegistry) CreateBucketMapping(ctx context.Context, storeID uuid.UUID, gatewayBucket, backendBucket string) (*BucketMapping, error) {
	return c.inner.CreateBucketMapping(ctx, storeID, gatewayBucket, backendBucket)
}

func (c *cachedRegistry) ListBucketMappings(ctx context.Context, storeID uuid.UUID) ([]BucketMapping, error) {
	return c.inner.ListBucketMappings(ctx, storeID)
}

func (c *cachedRegistry) DeleteBucketMapping(ctx context.Context, id, storeID uuid.UUID) error {
	mappings, err := c.inner.ListBucketMappings(ctx, storeID)
	if err != nil {
		return err
	}
	if err := c.inner.DeleteBucketMapping(ctx, id, storeID); err != nil {
		return err
	}
	for _, bm := range mappings {
		if bm.ID == id {
			_ = c.InvalidateBucket(ctx, bm.GatewayBucket)
			break
		}
	}
	return nil
}

func (c *cachedRegistry) CreateMultipartUpload(ctx context.Context, u MultipartUpload) error {
	return c.inner.CreateMultipartUpload(ctx, u)
}

func (c *cachedRegistry) GetMultipartUpload(ctx context.Context, gatewayUploadID string) (*MultipartUpload, error) {
	return c.inner.GetMultipartUpload(ctx, gatewayUploadID)
}

func (c *cachedRegistry) DeleteMultipartUpload(ctx context.Context, gatewayUploadID string) error {
	return c.inner.DeleteMultipartUpload(ctx, gatewayUploadID)
}

func (c *cachedRegistry) ListGatewayBuckets(ctx context.Context, tenantID uuid.UUID) ([]GatewayBucket, error) {
	return c.inner.ListGatewayBuckets(ctx, tenantID)
}

// invalidateBucketsForStore evicts all cached bucket entries belonging to the given store.
func (c *cachedRegistry) invalidateBucketsForStore(ctx context.Context, storeID uuid.UUID) error {
	mappings, err := c.inner.ListBucketMappings(ctx, storeID)
	if err != nil {
		return err
	}
	for _, bm := range mappings {
		_ = c.InvalidateBucket(ctx, bm.GatewayBucket)
	}
	return nil
}
