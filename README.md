# Storage Gateway

A multi-tenant S3-compatible storage proxy. Consumer services talk S3 (AWS Sig V4); the gateway routes each request to the actual upstream provider — AWS S3, Google Cloud Storage, Cloudflare R2, Azure Blob Storage, or a local filesystem — without ever buffering file content in memory.

```
Consumer (AWS SDK)  ──→  Gateway :8080  ──→  S3 / GCS / R2 / Azure / Local
                               │
          Admin API :9001 ──────┤
                               │
                          Postgres (registry)
                          Redis   (hot-path cache)
```

---

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Admin API](#admin-api)
  - [Tenants](#tenants)
  - [Access keys](#access-keys)
  - [Stores](#stores)
  - [Bucket mappings](#bucket-mappings)
- [CORS](#cors)
  - [Admin API CORS](#admin-api-cors)
  - [Per-store file serving CORS](#per-store-file-serving-cors)
- [Consumer setup](#consumer-setup)
- [Backend configuration](#backend-configuration)
- [Presigned URL modes](#presigned-url-modes)
- [Docker](#docker)
- [Security](#security)
- [Development](#development)

---

## How it works

The gateway introduces three concepts that sit between a consumer and its upstream storage:

| Concept | Description |
|---|---|
| **Tenant** | An isolated unit (a team, a service, a customer). Each tenant has its own access credentials and stores. |
| **Store** | One upstream provider account — S3 bucket group, a GCS project, an Azure storage account, etc. Each store has exactly one backend type and one set of credentials, stored encrypted. |
| **Bucket mapping** | Maps a **gateway bucket name** (globally unique, what the consumer sees) to a **backend bucket name** on the upstream. Multiple mappings can point at buckets inside the same store. |

On every request:

1. The gateway verifies the consumer's AWS Sig V4 signature against the stored (encrypted) secret key.
2. It resolves the gateway bucket name to a store and backend bucket.
3. It forwards the operation to the upstream provider, streaming bytes directly — no local buffering.

---

## Quick start

**Prerequisites:** Docker, Go 1.26+

```bash
# 1. Start Postgres and Redis
make up          # or: docker compose up -d

# 2. Copy and edit environment variables
cp .env.example .env
# Set MASTER_KEY to a 32+ character random string
# Set ADMIN_TOKEN to a secret bearer token for the admin API
# Edit DATABASE_URL / REDIS_URL if needed

# 3. Run the gateway (migrations apply automatically on startup)
source .env && go run ./cmd/gateway
```

The gateway listens on two ports:

| Port | Purpose |
|---|---|
| **:8080** | S3-compatible data plane (consumer traffic) |
| **:9001** | Admin API (internal — do not expose publicly) |

---

## Configuration

All configuration is via environment variables.

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | ✓ | — | PostgreSQL connection string |
| `MASTER_KEY` | ✓ | — | AES-256 encryption key for secrets at rest. Use `openssl rand -hex 32` to generate. |
| `ADMIN_TOKEN` | ✓ | — | Bearer token required on every Admin API request |
| `REDIS_URL` | | `redis://localhost:6379` | Redis connection URL |
| `GATEWAY_ADDR` | | `:8080` | Listen address for the S3 gateway |
| `ADMIN_ADDR` | | `:9001` | Listen address for the Admin API |
| `GATEWAY_REGION` | | `us-east-1` | AWS region consumers must configure in their SDK |
| `CACHE_TTL` | | `5m` | How long registry entries are cached in Redis |
| `ADMIN_BASE_PATH` | | _(none)_ | Optional URL prefix for all Admin API routes (e.g. `/api`). `GET /healthz` is always at the root regardless of this setting. |
| `ADMIN_ALLOWED_ORIGINS` | | _(none)_ | Comma-separated list of origins permitted to make cross-origin requests to the Admin API. Use `*` to allow all origins. See [Admin API CORS](#admin-api-cors). |

---

## Admin API

All requests require `Authorization: Bearer <ADMIN_TOKEN>`.  
All request and response bodies are JSON.

### Health check

Available on **both** the admin port and the gateway port — no authentication required.

```
GET /healthz
```

```json
// 200 OK — all checks pass
{ "status": "ok", "checks": { "database": {"status": "ok"}, "redis": {"status": "ok"} } }

// 503 Service Unavailable — one or more checks fail
{ "status": "degraded", "checks": { "database": {"status": "ok"}, "redis": {"status": "error", "error": "..."} } }
```

---

### Tenants

#### List all tenants

```
GET /tenants
```

```json
// 200 OK
[
  { "id": "...", "name": "acme", "created_at": "..." }
]
```

#### Create a tenant

```
POST /tenants
```

```json
{ "name": "acme" }
```

```json
// 201 Created
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "acme",
  "created_at": "2025-01-01T00:00:00Z"
}
```

#### Get a tenant

```
GET /tenants/{tenantID}
```

---

### Access keys

Access keys authenticate consumer requests (S3 Sig V4). **The secret key is only returned at creation** — it cannot be retrieved again.

#### Create an access key

```
POST /tenants/{tenantID}/keys
```

```json
// 201 Created
{
  "id": "...",
  "tenant_id": "...",
  "access_key": "SGWABCDEF1234567890",
  "secret_key": "abcdefghijklmnopqrstuvwxyz0123456789ABCD",
  "created_at": "2025-01-01T00:00:00Z"
}
```

Access keys have the format `SGW` + 17 uppercase alphanumeric characters.  
Secret keys are 40-character URL-safe base64 strings.

#### List access keys

```
GET /tenants/{tenantID}/keys
```

Returns all keys (active and revoked). The `secret_key` field is never included in list responses.

#### Revoke an access key

```
DELETE /tenants/{tenantID}/keys/{keyID}
```

```
204 No Content
```

---

### Stores

A store binds a backend provider + credentials to a tenant. The `backend_config` JSON is encrypted before being written to the database — see [Backend configuration](#backend-configuration) for the shape of each provider.

#### Create a store

```
POST /tenants/{tenantID}/stores
```

```json
{
  "name": "primary",
  "backend_type": "s3",
  "presigned_mode": "proxy",
  "backend_config": {
    "region": "us-east-1",
    "access_key_id": "AKIA...",
    "secret_access_key": "..."
  }
}
```

`backend_type` must be one of: `s3`, `gcs`, `r2`, `azure`, `local`.  
`presigned_mode` must be `proxy` (default) or `redirect` — see [Presigned URL modes](#presigned-url-modes).

```json
// 201 Created
{
  "id": "...",
  "tenant_id": "...",
  "name": "primary",
  "backend_type": "s3",
  "presigned_mode": "proxy",
  "allowed_origins": [],
  "created_at": "...",
  "updated_at": "..."
}
```

#### List / get stores

```
GET /tenants/{tenantID}/stores
GET /tenants/{tenantID}/stores/{storeID}
```

#### Update backend credentials

Replaces the backend config and/or presigned mode. Automatically evicts the Redis cache and the in-process backend client pool so the next request picks up the new credentials.

```
PUT /tenants/{tenantID}/stores/{storeID}/backend
```

```json
{
  "backend_config": { "region": "eu-west-1", "access_key_id": "...", "secret_access_key": "..." },
  "presigned_mode": "redirect"
}
```

```
204 No Content
```

#### Delete a store

Cascades to bucket mappings. Evicts cache entries for all affected buckets.

```
DELETE /tenants/{tenantID}/stores/{storeID}
```

```
204 No Content
```

---

### Bucket mappings

A bucket mapping connects a **gateway bucket name** (globally unique across all tenants) to a **backend bucket name** inside a store.

#### Create a bucket mapping

```
POST /tenants/{tenantID}/stores/{storeID}/buckets
```

```json
{
  "gateway_bucket": "acme-uploads",
  "backend_bucket": "prod-uploads-eu"
}
```

```json
// 201 Created
{
  "id": "...",
  "store_id": "...",
  "gateway_bucket": "acme-uploads",
  "backend_bucket": "prod-uploads-eu",
  "created_at": "..."
}
```

#### List bucket mappings

```
GET /tenants/{tenantID}/stores/{storeID}/buckets
```

#### Delete a bucket mapping

```
DELETE /tenants/{tenantID}/stores/{storeID}/buckets/{mappingID}
```

```
204 No Content
```

---

## CORS

### Admin API CORS

By default the Admin API does not send any CORS headers. To allow browser-based dashboards or tooling to call it, set `ADMIN_ALLOWED_ORIGINS` to a comma-separated list of permitted origins:

```bash
# Allow a specific origin
ADMIN_ALLOWED_ORIGINS=https://dashboard.example.com

# Allow multiple origins
ADMIN_ALLOWED_ORIGINS=https://dashboard.example.com,https://staging.example.com

# Allow all origins (development only)
ADMIN_ALLOWED_ORIGINS=*
```

When a request arrives with a matching `Origin` header, the gateway responds with:

```
Access-Control-Allow-Origin: https://dashboard.example.com
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Headers: Authorization, Content-Type
```

`OPTIONS` preflight requests are handled automatically and return `204 No Content`.

---

### Per-store file serving CORS

Each store has an `allowed_origins` list that controls which browser origins may fetch objects through the S3 gateway. This enables browser apps to `fetch()` files directly from the gateway without a proxy.

#### Get allowed origins

```
GET /tenants/{tenantID}/stores/{storeID}/cors
```

```json
// 200 OK
{ "allowed_origins": ["https://app.example.com"] }
```

#### Set allowed origins

Replaces the entire list. Pass an empty array to disable CORS for the store. Automatically evicts the Redis bucket cache so the change takes effect immediately.

```
PUT /tenants/{tenantID}/stores/{storeID}/cors
```

```json
{ "allowed_origins": ["https://app.example.com", "https://staging.example.com"] }
```

```
204 No Content
```

Use `"*"` to allow any origin:

```json
{ "allowed_origins": ["*"] }
```

#### How it works

When a browser sends a `GET` or `HEAD` request with an `Origin` header that matches the store's `allowed_origins`, the gateway adds:

```
Access-Control-Allow-Origin: https://app.example.com
Vary: Origin
```

For `OPTIONS` preflight requests, the gateway responds without requiring AWS Sig V4 authentication:

```
Access-Control-Allow-Origin: https://app.example.com
Access-Control-Allow-Methods: GET, HEAD, PUT, DELETE, POST
Access-Control-Allow-Headers: Content-Type, Authorization, x-amz-date, x-amz-content-sha256, ...
Access-Control-Max-Age: 3600
```

> Presigned-redirect mode (`presigned_mode: redirect`) issues a 307 to the upstream provider. The upstream must have its own CORS policy configured for the browser to follow the redirect successfully.

---

## Consumer setup

Consumers configure their AWS SDK to point at the gateway with path-style addressing. The credentials are the `access_key` / `secret_key` pair created via the Admin API.

### AWS CLI

```bash
aws s3 ls s3://acme-uploads/ \
  --endpoint-url http://gateway:8080 \
  --region us-east-1

aws s3 cp file.txt s3://acme-uploads/path/to/file.txt \
  --endpoint-url http://gateway:8080 \
  --region us-east-1
```

Configure credentials once with `aws configure` or via environment variables:

```bash
export AWS_ACCESS_KEY_ID=SGWABCDEF1234567890
export AWS_SECRET_ACCESS_KEY=abcdefghijklmnopqrstuvwxyz0123456789ABCD
export AWS_DEFAULT_REGION=us-east-1
```

### Go (aws-sdk-go-v2)

```go
import (
    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/s3"
)

cfg, _ := config.LoadDefaultConfig(ctx,
    config.WithRegion("us-east-1"),
    config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
        "SGWABCDEF1234567890",
        "abcdefghijklmnopqrstuvwxyz0123456789ABCD",
        "",
    )),
)

client := s3.NewFromConfig(cfg, func(o *s3.Options) {
    o.BaseEndpoint = aws.String("http://gateway:8080")
    o.UsePathStyle = true
})
```

### Python (boto3)

```python
import boto3

s3 = boto3.client(
    "s3",
    endpoint_url="http://gateway:8080",
    region_name="us-east-1",
    aws_access_key_id="SGWABCDEF1234567890",
    aws_secret_access_key="abcdefghijklmnopqrstuvwxyz0123456789ABCD",
)
```

### Supported S3 operations

| Operation | Notes |
|---|---|
| `GetObject` | Range requests supported; redirect mode returns 307 to upstream |
| `HeadObject` | |
| `PutObject` | AWS chunked encoding decoded transparently |
| `DeleteObject` | |
| `DeleteObjects` | Multi-object delete |
| `ListObjectsV2` | V1 (`?marker`) also accepted |
| `HeadBucket` | |
| `ListBuckets` | Returns only buckets belonging to the authenticated tenant |
| `CreateMultipartUpload` | |
| `UploadPart` | |
| `CompleteMultipartUpload` | |
| `AbortMultipartUpload` | |

> Server-side copy (`x-amz-copy-source`) is not implemented.

---

## Backend configuration

The `backend_config` object passed to the Admin API is encrypted before storage. Each provider has its own shape.

### AWS S3

```json
{
  "region": "us-east-1",
  "access_key_id": "AKIA...",
  "secret_access_key": "..."
}
```

### Cloudflare R2

Use `backend_type: "r2"` with the S3-compatible endpoint:

```json
{
  "region": "auto",
  "endpoint": "https://<account-id>.r2.cloudflarestorage.com",
  "access_key_id": "...",
  "secret_access_key": "...",
  "force_path_style": true
}
```

### Google Cloud Storage

`credentials_json` is the raw content of a GCP service account key file (the JSON you download from the Cloud Console). The service account needs `Storage Object Admin` on the target buckets.

```json
{
  "credentials_json": {
    "type": "service_account",
    "project_id": "my-project",
    "private_key_id": "...",
    "private_key": "-----BEGIN RSA PRIVATE KEY-----\n...",
    "client_email": "storage-gw@my-project.iam.gserviceaccount.com",
    ...
  }
}
```

### Azure Blob Storage

```json
{
  "account_name": "mystorageaccount",
  "account_key": "base64-encoded-key==",
  "service_url": "https://mystorageaccount.blob.core.windows.net"
}
```

`service_url` defaults to `https://<account_name>.blob.core.windows.net` if omitted.

### Local filesystem (development only)

```json
{
  "root_path": "/var/data/storage-gateway"
}
```

Objects are stored as `<root_path>/<bucket>/<key>`. Presigned URLs are not supported for this backend.

---

## Presigned URL modes

Each store has a `presigned_mode` that controls how `GetObject` requests are served:

| Mode | Behaviour |
|---|---|
| `proxy` (default) | Object bytes stream through the gateway. The consumer always talks to the gateway. |
| `redirect` | The gateway authenticates the request, then returns `307 Temporary Redirect` to a short-lived (5 min) presigned URL on the upstream provider. Bytes flow directly from upstream to the consumer, reducing gateway load for read-heavy workloads. |

`PUT`, `DELETE`, and all multipart operations always flow through the gateway regardless of mode.

---

## Docker

### Run with Docker

```bash
docker run -d \
  -p 8080:8080 \
  -p 9001:9001 \
  -e DATABASE_URL=postgres://sgw:sgw@db:5432/storage_gateway?sslmode=disable \
  -e REDIS_URL=redis://redis:6379 \
  -e MASTER_KEY=$(openssl rand -hex 32) \
  -e ADMIN_TOKEN=$(openssl rand -hex 24) \
  ghcr.io/<owner>/storage-gateway:latest
```

### Published image tags

Images are built and pushed to GitHub Container Registry by the CI workflow on every push:

| Git ref | Image tag |
|---|---|
| `v1.2.3` tag | `:1.2.3`, `:1.2`, `:latest` |
| Push to `main` | `:main`, `:sha-<short>` |
| Pull request | Build only, not pushed |

Images are built for `linux/amd64` and `linux/arm64`.

---

## Security

**Credential encryption.** Backend credentials and consumer secret keys are encrypted at rest with AES-256-GCM. The encryption key is derived from `MASTER_KEY` using HKDF-SHA256. Losing `MASTER_KEY` makes all stored credentials unrecoverable.

**Secret key handling.** Consumer secret keys are never stored in plaintext. They must be captured at creation time — the API returns the secret key exactly once and it cannot be retrieved again. AWS Sig V4 requires the plaintext secret for HMAC verification, which is why encryption (not hashing) is used.

**Admin API isolation.** The Admin API runs on a separate port (`ADMIN_ADDR`) and must not be exposed publicly. Use a firewall, a private network, or a sidecar proxy to restrict access.

**Signature verification.** Every S3 request is verified against the stored secret key using AWS Sig V4 before any backend call is made. Presigned URLs are validated for expiry and signature correctness.

**`MASTER_KEY` rotation.** There is no built-in key rotation. To rotate: decrypt all stored credentials with the old key, re-encrypt with the new key, then update `MASTER_KEY`. Perform this as a maintenance operation with no in-flight requests.

---

## Development

```bash
# Start Postgres + Redis
make up

# Build
make build

# Test
make test

# Lint (go vet)
make lint

# Tidy dependencies
make tidy

# Stop infrastructure
make down
```

### Project layout

```
cmd/gateway/          # main entrypoint
config/               # environment-variable config loader
internal/
  auth/               # AES-256-GCM encrypt/decrypt, HKDF key derivation
  backend/            # Backend interface + S3, GCS, Azure, R2, local adapters
  db/                 # pgxpool setup, embedded migrations
  registry/           # Tenant/store/key/bucket registry (Postgres + Redis cache)
  api/
    admin/            # Admin REST API handlers
    s3/               # S3-protocol handlers + AWS Sig V4 middleware
```

### Adding a new backend provider

1. Add a config struct and a `new<Provider>Backend(cfg)` constructor in `internal/backend/`.
2. Implement the `Backend` interface (13 methods).
3. Add the new `BackendType` constant to `internal/registry/models.go`.
4. Register the type in the factory switch in `internal/backend/factory.go`.
5. Add the type to the `validBackendType` check in `internal/api/admin/stores.go`.
