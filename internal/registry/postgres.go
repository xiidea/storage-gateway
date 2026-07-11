package registry

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgManager implements Manager against PostgreSQL with no caching layer.
// Wrap it with NewCached to add Redis-backed caching on the Registry read path.
type pgManager struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Manager backed directly by PostgreSQL.
func NewPostgres(pool *pgxpool.Pool) Manager {
	return &pgManager{pool: pool}
}

// ---------------------------------------------------------------------------
// Registry — read path
// ---------------------------------------------------------------------------

func (m *pgManager) LookupAccessKey(ctx context.Context, accessKey string) (*AccessKeyRow, error) {
	const q = `
		SELECT id, tenant_id, secret_key_enc, readonly, created_at
		FROM   access_keys
		WHERE  access_key = $1 AND revoked_at IS NULL`

	var row AccessKeyRow
	row.AccessKey = accessKey
	err := m.pool.QueryRow(ctx, q, accessKey).Scan(
		&row.ID, &row.TenantID, &row.SecretKeyEnc, &row.Readonly, &row.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccessKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup access key: %w", err)
	}
	return &row, nil
}

func (m *pgManager) ResolveBucket(ctx context.Context, tenantID uuid.UUID, gatewayBucket string) (*ResolvedBucket, error) {
	const q = `
		SELECT
			bm.store_id,
			s.tenant_id,
			s.backend_type,
			s.backend_config_enc,
			bm.backend_bucket,
			s.presigned_mode,
			s.allowed_origins
		FROM  bucket_mappings bm
		JOIN  stores s ON s.id = bm.store_id
		WHERE bm.gateway_bucket = $1`

	var rb ResolvedBucket
	rb.GatewayBucket = gatewayBucket
	err := m.pool.QueryRow(ctx, q, gatewayBucket).Scan(
		&rb.StoreID, &rb.TenantID, &rb.BackendType, &rb.BackendConfigEnc,
		&rb.BackendBucket, &rb.PresignedMode, &rb.AllowedOrigins,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve bucket: %w", err)
	}
	if rb.TenantID != tenantID {
		return nil, ErrUnauthorized
	}
	rb.BackendBucket, rb.BackendPrefix = ParseBackendBucket(rb.BackendBucket)
	return &rb, nil
}

func (m *pgManager) GetBucketAllowedOrigins(ctx context.Context, gatewayBucket string) ([]string, error) {
	const q = `
		SELECT s.allowed_origins
		FROM   bucket_mappings bm
		JOIN   stores s ON s.id = bm.store_id
		WHERE  bm.gateway_bucket = $1`

	var origins []string
	err := m.pool.QueryRow(ctx, q, gatewayBucket).Scan(&origins)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get bucket allowed origins: %w", err)
	}
	return origins, nil
}

// No-ops: pgManager has no cache to invalidate.
func (m *pgManager) InvalidateAccessKey(_ context.Context, _ string) error { return nil }
func (m *pgManager) InvalidateBucket(_ context.Context, _ string) error    { return nil }

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

func (m *pgManager) CreateTenant(ctx context.Context, name string) (*Tenant, error) {
	const q = `
		INSERT INTO tenants (id, name) VALUES ($1, $2)
		RETURNING id, name, created_at`

	t := &Tenant{ID: uuid.New(), Name: name}
	err := m.pool.QueryRow(ctx, q, t.ID, name).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create tenant: %w", err)
	}
	return t, nil
}

func (m *pgManager) ListTenants(ctx context.Context) ([]Tenant, error) {
	const q = `SELECT id, name, created_at FROM tenants ORDER BY name`

	rows, err := m.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()

	var tenants []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

func (m *pgManager) GetTenant(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	const q = `SELECT id, name, created_at FROM tenants WHERE id = $1`

	var t Tenant
	err := m.pool.QueryRow(ctx, q, id).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get tenant: %w", err)
	}
	return &t, nil
}

// ---------------------------------------------------------------------------
// Access keys
// ---------------------------------------------------------------------------

func (m *pgManager) CreateAccessKey(ctx context.Context, tenantID uuid.UUID, accessKey string, secretKeyEnc []byte, readonly bool) (*AccessKeyRow, error) {
	const q = `
		INSERT INTO access_keys (id, tenant_id, access_key, secret_key_enc, readonly)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`

	row := &AccessKeyRow{
		ID:           uuid.New(),
		TenantID:     tenantID,
		AccessKey:    accessKey,
		SecretKeyEnc: secretKeyEnc,
		Readonly:     readonly,
	}
	err := m.pool.QueryRow(ctx, q, row.ID, tenantID, accessKey, secretKeyEnc, readonly).
		Scan(&row.ID, &row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create access key: %w", err)
	}
	return row, nil
}

func (m *pgManager) ListAccessKeys(ctx context.Context, tenantID uuid.UUID) ([]AccessKeyRow, error) {
	const q = `
		SELECT id, tenant_id, access_key, readonly, created_at, revoked_at
		FROM   access_keys
		WHERE  tenant_id = $1
		ORDER  BY created_at DESC`

	rows, err := m.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list access keys: %w", err)
	}
	defer rows.Close()

	var keys []AccessKeyRow
	for rows.Next() {
		var k AccessKeyRow
		if err := rows.Scan(&k.ID, &k.TenantID, &k.AccessKey, &k.Readonly, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan access key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (m *pgManager) UpdateAccessKeyReadonly(ctx context.Context, id, tenantID uuid.UUID, readonly bool) (*AccessKeyRow, error) {
	const q = `
		UPDATE access_keys SET readonly = $1
		WHERE  id = $2 AND tenant_id = $3 AND revoked_at IS NULL
		RETURNING id, tenant_id, access_key, readonly, created_at, revoked_at`

	var row AccessKeyRow
	err := m.pool.QueryRow(ctx, q, readonly, id, tenantID).Scan(
		&row.ID, &row.TenantID, &row.AccessKey, &row.Readonly, &row.CreatedAt, &row.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccessKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update access key readonly: %w", err)
	}
	return &row, nil
}

func (m *pgManager) RevokeAccessKey(ctx context.Context, id, tenantID uuid.UUID) error {
	const q = `
		UPDATE access_keys SET revoked_at = NOW()
		WHERE  id = $1 AND tenant_id = $2 AND revoked_at IS NULL`

	tag, err := m.pool.Exec(ctx, q, id, tenantID)
	if err != nil {
		return fmt.Errorf("revoke access key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAccessKeyNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Stores
// ---------------------------------------------------------------------------

func (m *pgManager) CreateStore(ctx context.Context, p CreateStoreParams) (*Store, error) {
	const q = `
		INSERT INTO stores (id, tenant_id, name, backend_type, backend_config_enc, presigned_mode, allowed_origins)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at, updated_at`

	if p.AllowedOrigins == nil {
		p.AllowedOrigins = []string{}
	}
	s := &Store{
		ID:               uuid.New(),
		TenantID:         p.TenantID,
		Name:             p.Name,
		BackendType:      p.BackendType,
		BackendConfigEnc: p.BackendConfigEnc,
		PresignedMode:    p.PresignedMode,
		AllowedOrigins:   p.AllowedOrigins,
	}
	err := m.pool.QueryRow(ctx, q,
		s.ID, p.TenantID, p.Name, p.BackendType, p.BackendConfigEnc, p.PresignedMode, p.AllowedOrigins,
	).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create store: %w", err)
	}
	return s, nil
}

func (m *pgManager) GetStore(ctx context.Context, id uuid.UUID) (*Store, error) {
	const q = `
		SELECT id, tenant_id, name, backend_type, backend_config_enc, presigned_mode, allowed_origins, created_at, updated_at
		FROM   stores
		WHERE  id = $1`

	var s Store
	err := m.pool.QueryRow(ctx, q, id).Scan(
		&s.ID, &s.TenantID, &s.Name, &s.BackendType, &s.BackendConfigEnc,
		&s.PresignedMode, &s.AllowedOrigins, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStoreNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get store: %w", err)
	}
	return &s, nil
}

func (m *pgManager) ListStores(ctx context.Context, tenantID uuid.UUID) ([]Store, error) {
	const q = `
		SELECT id, tenant_id, name, backend_type, backend_config_enc, presigned_mode, allowed_origins, created_at, updated_at
		FROM   stores
		WHERE  tenant_id = $1
		ORDER  BY name`

	rows, err := m.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list stores: %w", err)
	}
	defer rows.Close()

	var stores []Store
	for rows.Next() {
		var s Store
		if err := rows.Scan(
			&s.ID, &s.TenantID, &s.Name, &s.BackendType, &s.BackendConfigEnc,
			&s.PresignedMode, &s.AllowedOrigins, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan store: %w", err)
		}
		stores = append(stores, s)
	}
	return stores, rows.Err()
}

func (m *pgManager) UpdateStoreBackend(ctx context.Context, id, tenantID uuid.UUID, backendConfigEnc []byte, presignedMode PresignedMode) error {
	const q = `
		UPDATE stores
		SET    backend_config_enc = $1, presigned_mode = $2, updated_at = NOW()
		WHERE  id = $3 AND tenant_id = $4`

	tag, err := m.pool.Exec(ctx, q, backendConfigEnc, presignedMode, id, tenantID)
	if err != nil {
		return fmt.Errorf("update store: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStoreNotFound
	}
	return nil
}

func (m *pgManager) UpdateStoreAllowedOrigins(ctx context.Context, id, tenantID uuid.UUID, origins []string) error {
	if origins == nil {
		origins = []string{}
	}
	const q = `
		UPDATE stores SET allowed_origins = $1, updated_at = NOW()
		WHERE  id = $2 AND tenant_id = $3`

	tag, err := m.pool.Exec(ctx, q, origins, id, tenantID)
	if err != nil {
		return fmt.Errorf("update store allowed origins: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStoreNotFound
	}
	return nil
}

func (m *pgManager) DeleteStore(ctx context.Context, id, tenantID uuid.UUID) error {
	const q = `DELETE FROM stores WHERE id = $1 AND tenant_id = $2`

	tag, err := m.pool.Exec(ctx, q, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete store: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStoreNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bucket mappings
// ---------------------------------------------------------------------------

func (m *pgManager) CreateBucketMapping(ctx context.Context, storeID uuid.UUID, gatewayBucket, backendBucket string) (*BucketMapping, error) {
	const q = `
		INSERT INTO bucket_mappings (id, store_id, gateway_bucket, backend_bucket)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`

	bm := &BucketMapping{
		ID:            uuid.New(),
		StoreID:       storeID,
		GatewayBucket: gatewayBucket,
		BackendBucket: backendBucket,
	}
	err := m.pool.QueryRow(ctx, q, bm.ID, storeID, gatewayBucket, backendBucket).
		Scan(&bm.ID, &bm.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create bucket mapping: %w", err)
	}
	return bm, nil
}

func (m *pgManager) GetBucketMapping(ctx context.Context, id, storeID uuid.UUID) (*BucketMapping, error) {
	const q = `
		SELECT id, store_id, gateway_bucket, backend_bucket, created_at
		FROM   bucket_mappings
		WHERE  id = $1 AND store_id = $2`

	var bm BucketMapping
	err := m.pool.QueryRow(ctx, q, id, storeID).Scan(
		&bm.ID, &bm.StoreID, &bm.GatewayBucket, &bm.BackendBucket, &bm.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get bucket mapping: %w", err)
	}
	return &bm, nil
}

func (m *pgManager) ListBucketMappings(ctx context.Context, storeID uuid.UUID) ([]BucketMapping, error) {
	const q = `
		SELECT id, store_id, gateway_bucket, backend_bucket, created_at
		FROM   bucket_mappings
		WHERE  store_id = $1
		ORDER  BY gateway_bucket`

	rows, err := m.pool.Query(ctx, q, storeID)
	if err != nil {
		return nil, fmt.Errorf("list bucket mappings: %w", err)
	}
	defer rows.Close()

	var mappings []BucketMapping
	for rows.Next() {
		var bm BucketMapping
		if err := rows.Scan(&bm.ID, &bm.StoreID, &bm.GatewayBucket, &bm.BackendBucket, &bm.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan bucket mapping: %w", err)
		}
		mappings = append(mappings, bm)
	}
	return mappings, rows.Err()
}

func (m *pgManager) DeleteBucketMapping(ctx context.Context, id, storeID uuid.UUID) error {
	const q = `DELETE FROM bucket_mappings WHERE id = $1 AND store_id = $2`

	tag, err := m.pool.Exec(ctx, q, id, storeID)
	if err != nil {
		return fmt.Errorf("delete bucket mapping: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrBucketNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Multipart uploads
// ---------------------------------------------------------------------------

func (m *pgManager) CreateMultipartUpload(ctx context.Context, u MultipartUpload) error {
	const q = `
		INSERT INTO multipart_uploads
			(id, store_id, gateway_upload_id, backend_upload_id, gateway_bucket, backend_bucket, object_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	_, err := m.pool.Exec(ctx, q,
		u.ID, u.StoreID, u.GatewayUploadID, u.BackendUploadID,
		u.GatewayBucket, u.BackendBucket, u.ObjectKey,
	)
	if err != nil {
		return fmt.Errorf("create multipart upload: %w", err)
	}
	return nil
}

func (m *pgManager) GetMultipartUpload(ctx context.Context, gatewayUploadID string) (*MultipartUpload, error) {
	const q = `
		SELECT id, store_id, gateway_upload_id, backend_upload_id,
		       gateway_bucket, backend_bucket, object_key, created_at
		FROM   multipart_uploads
		WHERE  gateway_upload_id = $1`

	var u MultipartUpload
	err := m.pool.QueryRow(ctx, q, gatewayUploadID).Scan(
		&u.ID, &u.StoreID, &u.GatewayUploadID, &u.BackendUploadID,
		&u.GatewayBucket, &u.BackendBucket, &u.ObjectKey, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMultipartNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get multipart upload: %w", err)
	}
	return &u, nil
}

func (m *pgManager) DeleteMultipartUpload(ctx context.Context, gatewayUploadID string) error {
	const q = `DELETE FROM multipart_uploads WHERE gateway_upload_id = $1`

	_, err := m.pool.Exec(ctx, q, gatewayUploadID)
	if err != nil {
		return fmt.Errorf("delete multipart upload: %w", err)
	}
	return nil
}

func (m *pgManager) ListGatewayBuckets(ctx context.Context, tenantID uuid.UUID) ([]GatewayBucket, error) {
	const q = `
		SELECT bm.gateway_bucket, bm.created_at
		FROM   bucket_mappings bm
		JOIN   stores s ON s.id = bm.store_id
		WHERE  s.tenant_id = $1
		ORDER  BY bm.gateway_bucket`

	rows, err := m.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list gateway buckets: %w", err)
	}
	defer rows.Close()

	var buckets []GatewayBucket
	for rows.Next() {
		var b GatewayBucket
		if err := rows.Scan(&b.Name, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan gateway bucket: %w", err)
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}
