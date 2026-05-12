package s3

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"storage-gateway/internal/backend"
	"storage-gateway/internal/registry"
)

// POST /{bucket}/{key} — dispatches to CreateMultipartUpload or CompleteMultipartUpload.
func (h *Handler) postOnKey(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, ok := q["uploads"]; ok {
		h.createMultipartUpload(w, r)
		return
	}
	if q.Get("uploadId") != "" {
		h.completeMultipartUpload(w, r)
		return
	}
	writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed")
}

// POST /{bucket}/{key}?uploads
func (h *Handler) createMultipartUpload(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)
	if key == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "key is required")
		return
	}

	rb, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	out, err := be.CreateMultipartUpload(r.Context(), backend.CreateMultipartUploadInput{
		Bucket:      rb.BackendBucket,
		Key:         key,
		ContentType: contentType,
		Metadata:    extractMetadata(r),
	})
	if err != nil {
		h.backendErr(w, err)
		return
	}

	gatewayUploadID := uuid.New().String()
	if err := h.mgr.CreateMultipartUpload(r.Context(), registry.MultipartUpload{
		ID:              uuid.New(),
		StoreID:         rb.StoreID,
		GatewayUploadID: gatewayUploadID,
		BackendUploadID: out.UploadID,
		GatewayBucket:   bucket,
		BackendBucket:   rb.BackendBucket,
		ObjectKey:       key,
	}); err != nil {
		// Best-effort: try to abort the backend upload to avoid orphaned parts.
		be.AbortMultipartUpload(r.Context(), backend.AbortMultipartUploadInput{ //nolint:errcheck
			Bucket:   rb.BackendBucket,
			Key:      key,
			UploadID: out.UploadID,
		})
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "failed to record multipart upload")
		return
	}

	writeS3XML(w, http.StatusOK, initiateMultipartUploadResult{
		Bucket:   bucket,
		Key:      key,
		UploadId: gatewayUploadID,
	})
}

// PUT /{bucket}/{key}?uploadId=X&partNumber=N
func (h *Handler) uploadPart(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)
	q := r.URL.Query()

	gatewayUploadID := q.Get("uploadId")
	partNumberStr := q.Get("partNumber")
	partNumber, err := strconv.ParseInt(partNumberStr, 10, 32)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "invalid partNumber")
		return
	}

	mu, err := h.mgr.GetMultipartUpload(r.Context(), gatewayUploadID)
	if err != nil {
		if errors.Is(err, registry.ErrMultipartNotFound) {
			writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}

	// Verify bucket and key match the recorded upload.
	if mu.GatewayBucket != bucket || mu.ObjectKey != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
		return
	}

	_, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	var body io.Reader = r.Body
	size := r.ContentLength
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "aws-chunked") {
		body = newAWSChunkedReader(r.Body)
		if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				size = n
			}
		}
	}

	out, err := be.UploadPart(r.Context(), backend.UploadPartInput{
		Bucket:     mu.BackendBucket,
		Key:        key,
		UploadID:   mu.BackendUploadID,
		PartNumber: int32(partNumber),
		Body:       body,
		Size:       size,
	})
	if err != nil {
		h.backendErr(w, err)
		return
	}

	if out.ETag != "" {
		w.Header().Set("ETag", `"`+out.ETag+`"`)
	}
	w.Header().Set("x-amz-request-id", newRequestID())
	w.WriteHeader(http.StatusOK)
}

// POST /{bucket}/{key}?uploadId=X
func (h *Handler) completeMultipartUpload(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)
	gatewayUploadID := r.URL.Query().Get("uploadId")

	mu, err := h.mgr.GetMultipartUpload(r.Context(), gatewayUploadID)
	if err != nil {
		if errors.Is(err, registry.ErrMultipartNotFound) {
			writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}

	if mu.GatewayBucket != bucket || mu.ObjectKey != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
		return
	}

	_, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	var req completeMultipartUploadRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "invalid request body")
		return
	}

	parts := make([]backend.CompletedPart, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = backend.CompletedPart{
			PartNumber: p.PartNumber,
			ETag:       strings.Trim(p.ETag, `"`),
		}
	}

	out, err := be.CompleteMultipartUpload(r.Context(), backend.CompleteMultipartUploadInput{
		Bucket:   mu.BackendBucket,
		Key:      key,
		UploadID: mu.BackendUploadID,
		Parts:    parts,
	})
	if err != nil {
		h.backendErr(w, err)
		return
	}

	// Clean up the tracking record regardless of outcome.
	h.mgr.DeleteMultipartUpload(r.Context(), gatewayUploadID) //nolint:errcheck

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	location := scheme + "://" + r.Host + r.URL.Path

	writeS3XML(w, http.StatusOK, completeMultipartUploadResult{
		Location: location,
		Bucket:   bucket,
		Key:      key,
		ETag:     `"` + out.ETag + `"`,
	})
}

// DELETE /{bucket}/{key}?uploadId=X
func (h *Handler) abortMultipartUpload(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)
	gatewayUploadID := r.URL.Query().Get("uploadId")

	mu, err := h.mgr.GetMultipartUpload(r.Context(), gatewayUploadID)
	if err != nil {
		if errors.Is(err, registry.ErrMultipartNotFound) {
			writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}

	if mu.GatewayBucket != bucket || mu.ObjectKey != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
		return
	}

	_, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	// Best-effort abort on the backend; some providers (Azure) treat this as a no-op.
	be.AbortMultipartUpload(r.Context(), backend.AbortMultipartUploadInput{ //nolint:errcheck
		Bucket:   mu.BackendBucket,
		Key:      key,
		UploadID: mu.BackendUploadID,
	})

	h.mgr.DeleteMultipartUpload(r.Context(), gatewayUploadID) //nolint:errcheck

	w.Header().Set("x-amz-request-id", newRequestID())
	w.WriteHeader(http.StatusNoContent)
}
