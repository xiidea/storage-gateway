package s3

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
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
	log       *slog.Logger
}

// New wires the S3 handler and returns it as an http.Handler.
func New(mgr registry.Manager, cryptoKey []byte, pool *backend.Pool, region string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &Handler{
		mgr:       mgr,
		cryptoKey: cryptoKey,
		pool:      pool,
		region:    region,
		log:       log,
	}

	r := chi.NewRouter()

	// OPTIONS preflight — no authentication required; resolves bucket CORS config.
	r.Options("/{bucket}", h.corsOptions)
	r.Options("/{bucket}/*", h.corsOptions)

	// All S3 operations require Sig V4 authentication.
	r.Group(func(r chi.Router) {
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
	})

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
		h.log.Error("failed to initialise backend", "store_id", rb.StoreID, "backend_type", rb.BackendType, "err", err)
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "failed to initialise backend")
		return nil, nil, false
	}

	return rb, be, true
}

// corsOptions handles CORS preflight (OPTIONS) requests without authentication.
// It looks up the bucket's allowed origins and responds with appropriate headers.
func (h *Handler) corsOptions(w http.ResponseWriter, r *http.Request) {
	bucket, _ := bucketAndKey(r)
	origins, err := h.mgr.GetBucketAllowedOrigins(r.Context(), bucket)
	if err != nil || len(origins) == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	origin := r.Header.Get("Origin")
	if !originAllowed(origin, origins) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, DELETE, POST")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-amz-date, x-amz-content-sha256, x-amz-security-token, x-amz-decoded-content-length")
	w.Header().Set("Access-Control-Max-Age", "3600")
	w.Header().Add("Vary", "Origin")
	w.WriteHeader(http.StatusNoContent)
}

// setCORSHeaders sets Access-Control-Allow-Origin on the response when the
// request Origin matches any of the store's allowed origins.
func setCORSHeaders(w http.ResponseWriter, r *http.Request, allowedOrigins []string) {
	if len(allowedOrigins) == 0 {
		return
	}
	origin := r.Header.Get("Origin")
	if origin == "" || !originAllowed(origin, allowedOrigins) {
		return
	}
	for _, ao := range allowedOrigins {
		if ao == "*" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			return
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Add("Vary", "Origin")
}

func originAllowed(origin string, allowedOrigins []string) bool {
	for _, ao := range allowedOrigins {
		if ao == "*" || ao == origin {
			return true
		}
	}
	return false
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
