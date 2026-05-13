package registry

import (
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
type ResolvedBucket struct {
	StoreID          uuid.UUID
	TenantID         uuid.UUID
	BackendType      BackendType
	BackendConfigEnc []byte
	GatewayBucket    string
	BackendBucket    string
	PresignedMode    PresignedMode
	AllowedOrigins   []string
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
