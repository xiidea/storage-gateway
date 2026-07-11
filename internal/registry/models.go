package registry

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

type BackendType string

const (
	BackendS3    BackendType = "s3"
	BackendGCS   BackendType = "gcs"
	BackendR2    BackendType = "r2"
	BackendAzure BackendType = "azure"
	BackendLocal BackendType = "local"
)

type PresignedMode string

const (
	PresignedProxy    PresignedMode = "proxy"
	PresignedRedirect PresignedMode = "redirect"
)

type Tenant struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type AccessKeyRow struct {
	ID           uuid.UUID  `json:"id"`
	TenantID     uuid.UUID  `json:"tenant_id"`
	AccessKey    string     `json:"access_key"`
	SecretKeyEnc []byte     `json:"-"`
	Readonly     bool       `json:"readonly"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

type Store struct {
	ID               uuid.UUID     `json:"id"`
	TenantID         uuid.UUID     `json:"tenant_id"`
	Name             string        `json:"name"`
	BackendType      BackendType   `json:"backend_type"`
	BackendConfigEnc []byte        `json:"-"`
	PresignedMode    PresignedMode `json:"presigned_mode"`
	AllowedOrigins   []string      `json:"allowed_origins"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

type CreateStoreParams struct {
	TenantID         uuid.UUID
	Name             string
	BackendType      BackendType
	BackendConfigEnc []byte
	PresignedMode    PresignedMode
	AllowedOrigins   []string
}

type BucketMapping struct {
	ID            uuid.UUID `json:"id"`
	StoreID       uuid.UUID `json:"store_id"`
	GatewayBucket string    `json:"gateway_bucket"`
	BackendBucket string    `json:"backend_bucket"`
	CreatedAt     time.Time `json:"created_at"`
}

// ResolvedBucket contains everything a request handler needs to talk to the upstream.
// BackendBucket is always the bare bucket name. BackendPrefix is the optional key
// prefix parsed from the backend_bucket mapping value ("bucket/prefix/path").
type ResolvedBucket struct {
	StoreID          uuid.UUID
	TenantID         uuid.UUID
	BackendType      BackendType
	BackendConfigEnc []byte
	GatewayBucket    string
	BackendBucket    string
	BackendPrefix    string
	PresignedMode    PresignedMode
	AllowedOrigins   []string
}

// PrefixKey prepends BackendPrefix to a gateway object key.
// When key is empty (list prefix use-case), returns "prefix/" so the backend
// lists only objects under that path.
func (rb *ResolvedBucket) PrefixKey(key string) string {
	if rb.BackendPrefix == "" {
		return key
	}
	if key == "" {
		return rb.BackendPrefix + "/"
	}
	return rb.BackendPrefix + "/" + key
}

// StripPrefix removes BackendPrefix from a backend key returned by the upstream,
// translating it back to the gateway namespace.
func (rb *ResolvedBucket) StripPrefix(backendKey string) string {
	if rb.BackendPrefix == "" {
		return backendKey
	}
	return strings.TrimPrefix(backendKey, rb.BackendPrefix+"/")
}

// ParseBackendBucket splits a raw backend_bucket mapping value into the bucket
// name and an optional key prefix.
//
//	"my-bucket"              → ("my-bucket", "")
//	"my-bucket/base/path"   → ("my-bucket", "base/path")
func ParseBackendBucket(raw string) (bucket, prefix string) {
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

// GatewayBucket is a lightweight view used by the S3 ListBuckets response.
type GatewayBucket struct {
	Name      string
	CreatedAt time.Time
}

type MultipartUpload struct {
	ID              uuid.UUID `json:"id"`
	StoreID         uuid.UUID `json:"store_id"`
	GatewayUploadID string    `json:"gateway_upload_id"`
	BackendUploadID string    `json:"backend_upload_id"`
	GatewayBucket   string    `json:"gateway_bucket"`
	BackendBucket   string    `json:"backend_bucket"`
	ObjectKey       string    `json:"object_key"`
	CreatedAt       time.Time `json:"created_at"`
}
