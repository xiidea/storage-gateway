package backend

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"storage-gateway/internal/auth"
	"storage-gateway/internal/registry"
)

// ---------------------------------------------------------------------------
// Per-provider config structs (stored encrypted in stores.backend_config_enc)
// ---------------------------------------------------------------------------

// S3Config covers AWS S3, Cloudflare R2, and any S3-compatible endpoint.
// Set Endpoint and ForcePathStyle for non-AWS providers.
type S3Config struct {
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	ForcePathStyle  bool   `json:"force_path_style,omitempty"`
}

// GCSConfig uses a service account JSON key for authentication and signing.
// CredentialsJSON is the raw content of a GCP service account key file.
type GCSConfig struct {
	CredentialsJSON json.RawMessage `json:"credentials_json"`
}

// AzureConfig authenticates with a storage account shared key.
// ServiceURL defaults to https://<AccountName>.blob.core.windows.net if empty.
type AzureConfig struct {
	AccountName string `json:"account_name"`
	AccountKey  string `json:"account_key"`
	ServiceURL  string `json:"service_url,omitempty"`
}

// LocalConfig is for local filesystem storage — development and testing only.
type LocalConfig struct {
	RootPath string `json:"root_path"`
}

// normalizeGCSCredentials extracts the service-account key from a GCS backend
// config, accepting the shapes commonly submitted:
//
//	{"credentials_json": {...key object...}}   — canonical
//	{"credentials_json": "{...stringified...}"} — key pasted as a JSON string
//	{...key object...}                          — bare key as the whole config
//
// Missing or empty credentials are an error: silently falling back to the
// SDK's Application Default Credentials would use ambient machine identity
// for a tenant-scoped store.
func normalizeGCSCredentials(configJSON []byte) (json.RawMessage, error) {
	var cfg GCSConfig
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parsing gcs config: %w", err)
	}
	creds := cfg.CredentialsJSON

	// Stringified key: unwrap one level of JSON string quoting.
	if len(creds) > 0 && creds[0] == '"' {
		var s string
		if err := json.Unmarshal(creds, &s); err != nil {
			return nil, fmt.Errorf("parsing gcs credentials_json string: %w", err)
		}
		creds = json.RawMessage(s)
	}

	// Bare service-account key submitted as the whole config.
	if len(creds) == 0 || string(creds) == "null" {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(configJSON, &probe) == nil && probe.Type == "service_account" {
			creds = configJSON
		}
	}

	if len(creds) == 0 || string(creds) == "null" {
		return nil, fmt.Errorf("gcs config: credentials_json (service account key) is required")
	}
	return creds, nil
}

// ---------------------------------------------------------------------------
// Factory
// ---------------------------------------------------------------------------

// New builds a Backend from the decrypted provider config JSON.
// backendType must match the shape of configJSON.
func New(backendType registry.BackendType, configJSON []byte) (Backend, error) {
	switch backendType {
	case registry.BackendS3, registry.BackendR2:
		var cfg S3Config
		if err := json.Unmarshal(configJSON, &cfg); err != nil {
			return nil, fmt.Errorf("parsing s3 config: %w", err)
		}
		return newS3Backend(cfg)

	case registry.BackendGCS:
		creds, err := normalizeGCSCredentials(configJSON)
		if err != nil {
			return nil, err
		}
		return newGCSBackend(GCSConfig{CredentialsJSON: creds})

	case registry.BackendAzure:
		var cfg AzureConfig
		if err := json.Unmarshal(configJSON, &cfg); err != nil {
			return nil, fmt.Errorf("parsing azure config: %w", err)
		}
		return newAzureBackend(cfg)

	case registry.BackendLocal:
		var cfg LocalConfig
		if err := json.Unmarshal(configJSON, &cfg); err != nil {
			return nil, fmt.Errorf("parsing local config: %w", err)
		}
		return newLocalBackend(cfg)

	default:
		return nil, fmt.Errorf("unknown backend type: %q", backendType)
	}
}

// ---------------------------------------------------------------------------
// Pool — caches Backend instances per store to avoid recreating SDK clients
// ---------------------------------------------------------------------------

// Pool holds one Backend per store ID. It is safe for concurrent use.
// Invalidate must be called whenever a store's backend config is updated.
type Pool struct {
	mu        sync.RWMutex
	backends  map[uuid.UUID]Backend
	cryptoKey []byte
}

// NewPool creates a Pool that uses cryptoKey to decrypt backend configs.
func NewPool(cryptoKey []byte) *Pool {
	return &Pool{
		backends:  make(map[uuid.UUID]Backend),
		cryptoKey: cryptoKey,
	}
}

// Get returns a cached Backend for the resolved bucket, building one on the
// first call. The Backend is keyed by StoreID; credentials are decrypted once
// and reused for the lifetime of the cached instance.
func (p *Pool) Get(rb *registry.ResolvedBucket) (Backend, error) {
	p.mu.RLock()
	b, ok := p.backends[rb.StoreID]
	p.mu.RUnlock()
	if ok {
		return b, nil
	}

	configJSON, err := auth.Decrypt(p.cryptoKey, rb.BackendConfigEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypting backend config for store %s: %w", rb.StoreID, err)
	}

	b, err = New(rb.BackendType, configJSON)
	if err != nil {
		return nil, fmt.Errorf("building backend for store %s: %w", rb.StoreID, err)
	}

	p.mu.Lock()
	// Double-check: another goroutine may have populated the slot while we built.
	if existing, ok := p.backends[rb.StoreID]; ok {
		p.mu.Unlock()
		return existing, nil
	}
	p.backends[rb.StoreID] = b
	p.mu.Unlock()
	return b, nil
}

// Invalidate evicts the cached Backend for the given store, forcing a rebuild
// on the next Get. Call this after admin API updates to a store's config.
func (p *Pool) Invalidate(storeID uuid.UUID) {
	p.mu.Lock()
	delete(p.backends, storeID)
	p.mu.Unlock()
}
