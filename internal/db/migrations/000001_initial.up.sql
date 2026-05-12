CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE tenants (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT tenants_name_unique UNIQUE (name)
);

CREATE TABLE access_keys (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    access_key      TEXT        NOT NULL,
    secret_key_enc  BYTEA       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at      TIMESTAMPTZ,
    CONSTRAINT access_keys_key_unique UNIQUE (access_key)
);

-- Partial index: only active (non-revoked) keys are looked up on the hot path.
CREATE INDEX idx_access_keys_lookup ON access_keys(access_key)
    WHERE revoked_at IS NULL;

CREATE TABLE stores (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name                TEXT        NOT NULL,
    backend_type        TEXT        NOT NULL,
    backend_config_enc  BYTEA       NOT NULL,
    presigned_mode      TEXT        NOT NULL DEFAULT 'proxy',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT stores_tenant_name_unique UNIQUE (tenant_id, name),
    CONSTRAINT stores_backend_type_check  CHECK (backend_type   IN ('s3','gcs','r2','azure','local')),
    CONSTRAINT stores_presigned_mode_check CHECK (presigned_mode IN ('proxy','redirect'))
);

-- gateway_bucket is globally unique: two tenants cannot claim the same bucket name
-- because the S3 protocol identifies buckets by name alone, with no tenant prefix.
CREATE TABLE bucket_mappings (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id        UUID        NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    gateway_bucket  TEXT        NOT NULL,
    backend_bucket  TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT bucket_mappings_gateway_unique UNIQUE (gateway_bucket)
);

CREATE INDEX idx_bucket_mappings_lookup ON bucket_mappings(gateway_bucket);

-- Tracks in-flight multipart uploads so gateway_upload_id can be mapped to the
-- backend's upload_id across UploadPart and CompleteMultipartUpload calls.
CREATE TABLE multipart_uploads (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id            UUID        NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    gateway_upload_id   TEXT        NOT NULL,
    backend_upload_id   TEXT        NOT NULL,
    gateway_bucket      TEXT        NOT NULL,
    backend_bucket      TEXT        NOT NULL,
    object_key          TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT multipart_gateway_upload_id_unique UNIQUE (gateway_upload_id)
);

CREATE INDEX idx_multipart_upload_lookup ON multipart_uploads(gateway_upload_id);
