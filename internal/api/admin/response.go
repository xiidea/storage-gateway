package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"storage-gateway/internal/registry"
)

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// handleErr maps known domain errors to HTTP status codes and writes the response.
func handleErr(w http.ResponseWriter, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "resource already exists")
		return
	}
	switch {
	case errors.Is(err, registry.ErrTenantNotFound),
		errors.Is(err, registry.ErrStoreNotFound),
		errors.Is(err, registry.ErrBucketNotFound),
		errors.Is(err, registry.ErrAccessKeyNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// uuidParam extracts and parses a named UUID path parameter.
func uuidParam(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, key))
}

// ---------------------------------------------------------------------------
// Response DTOs — never expose encrypted fields
// ---------------------------------------------------------------------------

type tenantDTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func toTenantDTO(t *registry.Tenant) tenantDTO {
	return tenantDTO{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt}
}

// keyDTO is used in list responses — secret is never returned after creation.
type keyDTO struct {
	ID        uuid.UUID  `json:"id"`
	TenantID  uuid.UUID  `json:"tenant_id"`
	AccessKey string     `json:"access_key"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func toKeyDTO(k registry.AccessKeyRow) keyDTO {
	return keyDTO{
		ID:        k.ID,
		TenantID:  k.TenantID,
		AccessKey: k.AccessKey,
		CreatedAt: k.CreatedAt,
		RevokedAt: k.RevokedAt,
	}
}

// createKeyResponse is returned only at creation — the only time the plaintext
// secret is ever visible.
type createKeyResponse struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	AccessKey string    `json:"access_key"`
	SecretKey string    `json:"secret_key"`
	CreatedAt time.Time `json:"created_at"`
}

// storeDTO omits the encrypted backend config.
type storeDTO struct {
	ID            uuid.UUID              `json:"id"`
	TenantID      uuid.UUID              `json:"tenant_id"`
	Name          string                 `json:"name"`
	BackendType   registry.BackendType   `json:"backend_type"`
	PresignedMode registry.PresignedMode `json:"presigned_mode"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

func toStoreDTO(s *registry.Store) storeDTO {
	return storeDTO{
		ID:            s.ID,
		TenantID:      s.TenantID,
		Name:          s.Name,
		BackendType:   s.BackendType,
		PresignedMode: s.PresignedMode,
		CreatedAt:     s.CreatedAt,
		UpdatedAt:     s.UpdatedAt,
	}
}

type bucketMappingDTO struct {
	ID            uuid.UUID `json:"id"`
	StoreID       uuid.UUID `json:"store_id"`
	GatewayBucket string    `json:"gateway_bucket"`
	BackendBucket string    `json:"backend_bucket"`
	CreatedAt     time.Time `json:"created_at"`
}

func toBucketMappingDTO(bm registry.BucketMapping) bucketMappingDTO {
	return bucketMappingDTO{
		ID:            bm.ID,
		StoreID:       bm.StoreID,
		GatewayBucket: bm.GatewayBucket,
		BackendBucket: bm.BackendBucket,
		CreatedAt:     bm.CreatedAt,
	}
}
