package s3

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"storage-gateway/internal/backend"
	"storage-gateway/internal/registry"
)

type ctxKey int

const ctxTenantID ctxKey = iota

// Handler is the S3-protocol HTTP handler. Wire it onto the gateway port.
type Handler struct {
	mgr       registry.Manager
	cryptoKey []byte
	pool      *backend.Pool
	region    string
}

// New wires the S3 handler and returns it as an http.Handler.
func New(mgr registry.Manager, cryptoKey []byte, pool *backend.Pool, region string) http.Handler {
	h := &Handler{
		mgr:       mgr,
		cryptoKey: cryptoKey,
		pool:      pool,
		region:    region,
	}

	r := chi.NewRouter()
	r.Use(h.authMiddleware)

	// Service-level
	r.Get("/", h.listBuckets)

	// Bucket-level
	r.Head("/{bucket}", h.headBucket)
	r.Get("/{bucket}", h.listObjectsV2)
	r.Post("/{bucket}", h.postOnBucket)

	// Object-level (key may contain slashes)
	r.Head("/{bucket}/*", h.headObject)
	r.Get("/{bucket}/*", h.getObject)
	r.Put("/{bucket}/*", h.putObject)
	r.Delete("/{bucket}/*", h.deleteObject)
	r.Post("/{bucket}/*", h.postOnKey)

	return r
}

// bucketAndKey extracts the bucket name and object key from the request URL path.
// For path /bucket/a/b/c: bucket="bucket", key="a/b/c".
func bucketAndKey(r *http.Request) (bucket, key string) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}

// resolveBucket looks up the gateway bucket mapping and initialises the backend client.
// On failure it writes the appropriate S3 error and returns false.
func (h *Handler) resolveBucket(w http.ResponseWriter, r *http.Request, bucketName string) (*registry.ResolvedBucket, backend.Backend, bool) {
	tenantID := r.Context().Value(ctxTenantID).(uuid.UUID)

	rb, err := h.mgr.ResolveBucket(r.Context(), tenantID, bucketName)
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrBucketNotFound):
			writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "the specified bucket does not exist")
		case errors.Is(err, registry.ErrUnauthorized):
			writeS3Error(w, http.StatusForbidden, "AccessDenied", "access denied")
		default:
			writeS3Error(w, http.StatusInternalServerError, "InternalError", "internal error")
		}
		return nil, nil, false
	}

	be, err := h.pool.Get(rb)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "failed to initialise backend")
		return nil, nil, false
	}

	return rb, be, true
}

// backendErr maps backend errors to S3 error responses.
func (h *Handler) backendErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, backend.ErrObjectNotFound):
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "the specified key does not exist")
	case errors.Is(err, backend.ErrBucketNotFound):
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "the specified bucket does not exist")
	case errors.Is(err, backend.ErrAccessDenied):
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "access denied by upstream")
	case errors.Is(err, backend.ErrUnknownSize):
		writeS3Error(w, http.StatusLengthRequired, "MissingContentLength", "content length is required")
	default:
		writeS3Error(w, http.StatusBadGateway, "InternalError", "upstream storage error")
	}
}
