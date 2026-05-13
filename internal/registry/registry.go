package registry

import (
	"context"

	"github.com/google/uuid"
)

// Registry is the read path used on every request. Implementations must be safe
// for concurrent use and should keep per-call latency as low as possible.
type Registry interface {
	// LookupAccessKey returns the tenant ID and encrypted secret for Sig v4 verification.
	// Returns ErrAccessKeyNotFound if the key does not exist or has been revoked.
	LookupAccessKey(ctx context.Context, accessKey string) (*AccessKeyRow, error)

	// ResolveBucket returns the full backend context for the given gateway bucket.
	// Returns ErrBucketNotFound if no mapping exists.
	// Returns ErrUnauthorized if the resolved store belongs to a different tenant.
	ResolveBucket(ctx context.Context, tenantID uuid.UUID, gatewayBucket string) (*ResolvedBucket, error)

	// GetBucketAllowedOrigins returns the CORS allowed origins for a gateway bucket.
	// No tenant authentication required — used for OPTIONS preflight responses.
	// Returns an empty slice (no error) if the bucket does not exist.
	GetBucketAllowedOrigins(ctx context.Context, gatewayBucket string) ([]string, error)

	// Invalidate* are called by the admin API after mutations so cached entries
	// are not served stale. Implementations that do not cache are no-ops.
	InvalidateAccessKey(ctx context.Context, accessKey string) error
	InvalidateBucket(ctx context.Context, gatewayBucket string) error
}

// Manager extends Registry with the write operations used by the admin API.
type Manager interface {
	Registry

	// Tenants
	CreateTenant(ctx context.Context, name string) (*Tenant, error)
	GetTenant(ctx context.Context, id uuid.UUID) (*Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)

	// Access keys
	CreateAccessKey(ctx context.Context, tenantID uuid.UUID, accessKey string, secretKeyEnc []byte) (*AccessKeyRow, error)
	ListAccessKeys(ctx context.Context, tenantID uuid.UUID) ([]AccessKeyRow, error)
	RevokeAccessKey(ctx context.Context, id, tenantID uuid.UUID) error

	// Stores
	CreateStore(ctx context.Context, p CreateStoreParams) (*Store, error)
	GetStore(ctx context.Context, id uuid.UUID) (*Store, error)
	ListStores(ctx context.Context, tenantID uuid.UUID) ([]Store, error)
	UpdateStoreBackend(ctx context.Context, id, tenantID uuid.UUID, backendConfigEnc []byte, presignedMode PresignedMode) error
	UpdateStoreAllowedOrigins(ctx context.Context, id, tenantID uuid.UUID, origins []string) error
	DeleteStore(ctx context.Context, id, tenantID uuid.UUID) error

	// Bucket mappings
	CreateBucketMapping(ctx context.Context, storeID uuid.UUID, gatewayBucket, backendBucket string) (*BucketMapping, error)
	ListBucketMappings(ctx context.Context, storeID uuid.UUID) ([]BucketMapping, error)
	DeleteBucketMapping(ctx context.Context, id, storeID uuid.UUID) error

	// Multipart upload state
	CreateMultipartUpload(ctx context.Context, u MultipartUpload) error
	GetMultipartUpload(ctx context.Context, gatewayUploadID string) (*MultipartUpload, error)
	DeleteMultipartUpload(ctx context.Context, gatewayUploadID string) error

	// ListGatewayBuckets returns all gateway bucket names for a tenant (for S3 ListBuckets).
	ListGatewayBuckets(ctx context.Context, tenantID uuid.UUID) ([]GatewayBucket, error)
}
