package s3

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"storage-gateway/internal/backend"
	"storage-gateway/internal/registry"
)

// GET /{bucket}/{key}
func (h *Handler) getObject(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)
	if key == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "key is required")
		return
	}

	rb, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	// Redirect mode: generate an upstream presigned URL and return 307.
	if rb.PresignedMode == registry.PresignedRedirect {
		out, err := be.PresignURL(r.Context(), backend.PresignInput{
			Bucket:  rb.BackendBucket,
			Key:     rb.PrefixKey(key),
			Method:  "GET",
			Expires: 5 * time.Minute,
		})
		if err != nil {
			writeS3Error(w, http.StatusInternalServerError, "InternalError", "failed to generate redirect URL")
			return
		}
		http.Redirect(w, r, out.URL, http.StatusTemporaryRedirect)
		return
	}

	// Proxy mode: stream bytes through the gateway.
	out, err := be.GetObject(r.Context(), backend.GetObjectInput{
		Bucket: rb.BackendBucket,
		Key:    rb.PrefixKey(key),
		Range:  r.Header.Get("Range"),
	})
	if err != nil {
		h.backendErr(w, err)
		return
	}
	defer out.Body.Close()

	setCORSHeaders(w, r, rb.AllowedOrigins)
	w.Header().Set("Accept-Ranges", "bytes")
	if out.ETag != "" {
		w.Header().Set("ETag", `"`+out.ETag+`"`)
	}
	if out.ContentType != "" {
		w.Header().Set("Content-Type", out.ContentType)
	}
	if out.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(out.ContentLength, 10))
	}
	if out.ContentRange != "" {
		w.Header().Set("Content-Range", out.ContentRange)
	}
	if !out.LastModified.IsZero() {
		w.Header().Set("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}
	for k, v := range out.Metadata {
		w.Header().Set("x-amz-meta-"+k, v)
	}
	w.Header().Set("x-amz-request-id", newRequestID())

	if out.ContentRange != "" {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	buf := copyBufPool.Get().(*[]byte)
	io.CopyBuffer(writerOnly{w}, out.Body, *buf) //nolint:errcheck
	copyBufPool.Put(buf)
}

// copyBufPool provides 1 MiB buffers for streaming object bodies; io.Copy's
// default 32 KiB buffer throttles multi-gigabyte transfers.
var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1<<20)
		return &b
	},
}

// writerOnly hides http.ResponseWriter's ReadFrom so io.CopyBuffer uses the
// pooled buffer instead of net/http's small internal one.
type writerOnly struct{ io.Writer }

// HEAD /{bucket}/{key}
func (h *Handler) headObject(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)
	if key == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tenantID := r.Context().Value(ctxTenantID).(uuid.UUID)
	rb, err := h.mgr.ResolveBucket(r.Context(), tenantID, bucket)
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrBucketNotFound), errors.Is(err, registry.ErrUnauthorized):
			w.WriteHeader(http.StatusNotFound)
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

	out, err := be.HeadObject(r.Context(), backend.HeadObjectInput{
		Bucket: rb.BackendBucket,
		Key:    rb.PrefixKey(key),
	})
	if err != nil {
		if backend.IsNotFound(err) {
			w.WriteHeader(http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusBadGateway)
		}
		return
	}

	setCORSHeaders(w, r, rb.AllowedOrigins)
	w.Header().Set("Accept-Ranges", "bytes")
	if out.ETag != "" {
		w.Header().Set("ETag", `"`+out.ETag+`"`)
	}
	if out.ContentType != "" {
		w.Header().Set("Content-Type", out.ContentType)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(out.ContentLength, 10))
	if !out.LastModified.IsZero() {
		w.Header().Set("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}
	for k, v := range out.Metadata {
		w.Header().Set("x-amz-meta-"+k, v)
	}
	w.Header().Set("x-amz-request-id", newRequestID())
	w.WriteHeader(http.StatusOK)
}

// PUT /{bucket}/{key} — dispatches to UploadPart or PutObject.
func (h *Handler) putObject(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("uploadId") != "" {
		h.uploadPart(w, r)
		return
	}
	h.doPutObject(w, r)
}

func (h *Handler) doPutObject(w http.ResponseWriter, r *http.Request) {
	bucket, key := bucketAndKey(r)
	if key == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "key is required")
		return
	}

	// Server-side copy is not implemented.
	if r.Header.Get("X-Amz-Copy-Source") != "" {
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented", "server-side copy is not supported")
		return
	}

	rb, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	body := r.Body
	size := r.ContentLength

	// AWS chunked encoding: strip the chunk framing and use the decoded length.
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "aws-chunked") {
		body = io.NopCloser(newAWSChunkedReader(r.Body))
		if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				size = n
			}
		}
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	out, err := be.PutObject(r.Context(), backend.PutObjectInput{
		Bucket:      rb.BackendBucket,
		Key:         rb.PrefixKey(key),
		Body:        body,
		Size:        size,
		ContentType: contentType,
		Metadata:    extractMetadata(r),
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

// DELETE /{bucket}/{key} — dispatches to AbortMultipartUpload or DeleteObject.
func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("uploadId") != "" {
		h.abortMultipartUpload(w, r)
		return
	}

	bucket, key := bucketAndKey(r)
	if key == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "key is required")
		return
	}

	rb, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	if err := be.DeleteObject(r.Context(), backend.DeleteObjectInput{
		Bucket: rb.BackendBucket,
		Key:    rb.PrefixKey(key),
	}); err != nil {
		h.backendErr(w, err)
		return
	}

	w.Header().Set("x-amz-request-id", newRequestID())
	w.WriteHeader(http.StatusNoContent)
}

// POST /{bucket}?delete — multi-object delete.
func (h *Handler) deleteObjects(w http.ResponseWriter, r *http.Request) {
	bucket, _ := bucketAndKey(r)
	rb, be, ok := h.resolveBucket(w, r, bucket)
	if !ok {
		return
	}

	var req deleteRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "invalid request body")
		return
	}

	objects := make([]backend.ObjectIdentifier, len(req.Objects))
	for i, o := range req.Objects {
		objects[i] = backend.ObjectIdentifier{Key: rb.PrefixKey(o.Key)}
	}

	out, err := be.DeleteObjects(r.Context(), backend.DeleteObjectsInput{
		Bucket:  rb.BackendBucket,
		Objects: objects,
		Quiet:   req.Quiet,
	})
	if err != nil {
		h.backendErr(w, err)
		return
	}

	result := deleteResult{}
	for _, d := range out.Deleted {
		result.Deleted = append(result.Deleted, deletedXML{Key: rb.StripPrefix(d.Key)})
	}
	for _, e := range out.Errors {
		result.Error = append(result.Error, deleteErrorXML{Key: rb.StripPrefix(e.Key), Code: e.Code, Message: e.Message})
	}

	writeS3XML(w, http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// extractMetadata returns user-defined metadata from x-amz-meta-* headers.
func extractMetadata(r *http.Request) map[string]string {
	meta := make(map[string]string)
	for k, v := range r.Header {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "x-amz-meta-") {
			meta[lower[len("x-amz-meta-"):]] = v[0]
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// awsChunkedReader strips the AWS chunked encoding framing from a body.
// Each chunk is: hex-size[;chunk-signature=hex]\r\ndata\r\n
// Terminated by: 0[;chunk-signature=hex]\r\n\r\n
type awsChunkedReader struct {
	rd        *bufio.Reader
	remaining int
	done      bool
}

func newAWSChunkedReader(r io.Reader) io.Reader {
	return &awsChunkedReader{rd: bufio.NewReader(r)}
}

func (a *awsChunkedReader) Read(p []byte) (int, error) {
	if a.done {
		return 0, io.EOF
	}
	for a.remaining == 0 {
		line, err := a.rd.ReadString('\n')
		if err != nil {
			return 0, fmt.Errorf("aws-chunked: read header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		// Strip optional ";chunk-signature=..."
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil {
			return 0, fmt.Errorf("aws-chunked: bad chunk size %q", line)
		}
		if size == 0 {
			a.done = true
			return 0, io.EOF
		}
		a.remaining = int(size)
	}

	toRead := len(p)
	if toRead > a.remaining {
		toRead = a.remaining
	}
	n, err := a.rd.Read(p[:toRead])
	a.remaining -= n
	if a.remaining == 0 {
		// Consume trailing \r\n after chunk data.
		a.rd.ReadString('\n') //nolint:errcheck
	}
	if err == io.EOF {
		return n, nil // real EOF comes from the 0-size terminator
	}
	return n, err
}
