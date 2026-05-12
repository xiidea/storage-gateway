package s3

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"storage-gateway/internal/backend"
	"storage-gateway/internal/registry"
)

// GET /
func (h *Handler) listBuckets(w http.ResponseWriter, r *http.Request) {
	tenantID := r.Context().Value(ctxTenantID).(uuid.UUID)

	buckets, err := h.mgr.ListGatewayBuckets(r.Context(), tenantID)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}

	result := listAllMyBucketsResult{}
	result.Owner.ID = tenantID.String()
	result.Owner.DisplayName = tenantID.String()

	for _, b := range buckets {
		result.Buckets.Bucket = append(result.Buckets.Bucket, bucketInfo{
			Name:         b.Name,
			CreationDate: b.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	writeS3XML(w, http.StatusOK, result)
}

// HEAD /{bucket}
func (h *Handler) headBucket(w http.ResponseWriter, r *http.Request) {
	bucket, _ := bucketAndKey(r)
	tenantID := r.Context().Value(ctxTenantID).(uuid.UUID)

	rb, err := h.mgr.ResolveBucket(r.Context(), tenantID, bucket)
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrBucketNotFound):
			w.WriteHeader(http.StatusNotFound)
		case errors.Is(err, registry.ErrUnauthorized):
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}

	be, err := h.pool.Get(rb)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := be.HeadBucket(r.Context(), backend.HeadBucketInput{Bucket: rb.BackendBucket}); err != nil {
		if errors.Is(err, backend.ErrBucketNotFound) {
			w.WriteHeader(http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusBadGateway)
		}
		return
	}

	w.Header().Set("x-amz-request-id", newRequestID())
	w.WriteHeader(http.StatusOK)
}

// GET /{bucket} — ListObjectsV2 (also handles V1 via ?marker)
func (h *Handler) listObjectsV2(w http.ResponseWriter, r *http.Request) {
	bucket, _ := bucketAndKey(r)
	rb, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	q := r.URL.Query()
	maxKeys := int32(1000)
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 && n <= 1000 {
			maxKeys = int32(n)
		}
	}

	in := backend.ListObjectsInput{
		Bucket:    rb.BackendBucket,
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		MaxKeys:   maxKeys,
	}
	// Support both ListObjectsV1 (?marker) and V2 (?continuation-token / ?start-after).
	if ct := q.Get("continuation-token"); ct != "" {
		in.ContinuationToken = ct
	}
	if sa := q.Get("start-after"); sa != "" {
		in.StartAfter = sa
	} else if m := q.Get("marker"); m != "" {
		in.StartAfter = m
	}

	out, err := be.ListObjects(r.Context(), in)
	if err != nil {
		h.backendErr(w, err)
		return
	}

	result := listBucketResult{
		Name:              bucket,
		Prefix:            in.Prefix,
		Delimiter:         in.Delimiter,
		MaxKeys:           maxKeys,
		KeyCount:          out.KeyCount,
		IsTruncated:       out.IsTruncated,
		ContinuationToken: q.Get("continuation-token"),
		StartAfter:        in.StartAfter,
	}
	if out.IsTruncated {
		result.NextContinuationToken = out.NextContinuationToken
	}

	for _, obj := range out.Contents {
		sc := obj.StorageClass
		if sc == "" {
			sc = "STANDARD"
		}
		result.Contents = append(result.Contents, s3Object{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(time.RFC3339Nano),
			ETag:         `"` + obj.ETag + `"`,
			Size:         obj.Size,
			StorageClass: sc,
		})
	}
	for _, cp := range out.CommonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, s3Prefix{Prefix: cp})
	}

	writeS3XML(w, http.StatusOK, result)
}

// POST /{bucket} — only ?delete is supported.
func (h *Handler) postOnBucket(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.URL.Query()["delete"]; ok {
		h.deleteObjects(w, r)
		return
	}
	writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed")
}
