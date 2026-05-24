package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"storage-gateway/internal/backend"
	"storage-gateway/internal/registry"
)

// resolveBrowseContext verifies the ownership chain tenant→store→mapping and
// returns an initialised Backend and the resolved bucket (with BackendBucket and
// BackendPrefix already parsed from the mapping's backend_bucket value).
// On failure it writes the error response and returns false.
func (h *Handler) resolveBrowseContext(w http.ResponseWriter, r *http.Request) (*registry.ResolvedBucket, backend.Backend, bool) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return nil, nil, false
	}

	mappingID, err := uuidParam(r, "mappingID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid mapping_id")
		return nil, nil, false
	}

	bm, err := h.mgr.GetBucketMapping(r.Context(), mappingID, store.ID)
	if err != nil {
		handleErr(w, err)
		return nil, nil, false
	}

	bucket, prefix := registry.ParseBackendBucket(bm.BackendBucket)
	rb := &registry.ResolvedBucket{
		StoreID:          store.ID,
		TenantID:         store.TenantID,
		BackendType:      store.BackendType,
		BackendConfigEnc: store.BackendConfigEnc,
		GatewayBucket:    bm.GatewayBucket,
		BackendBucket:    bucket,
		BackendPrefix:    prefix,
		PresignedMode:    store.PresignedMode,
		AllowedOrigins:   store.AllowedOrigins,
	}

	be, err := h.pool.Get(rb)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to initialise backend")
		return nil, nil, false
	}

	return rb, be, true
}

// ---------------------------------------------------------------------------
// GET /{storeID}/buckets/{mappingID}/browse
// ---------------------------------------------------------------------------

type browseObject struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"last_modified"`
	ETag         string `json:"etag"`
	StorageClass string `json:"storage_class"`
}

type browseListResponse struct {
	Prefix                string        `json:"prefix"`
	Delimiter             string        `json:"delimiter"`
	MaxKeys               int32         `json:"max_keys"`
	KeyCount              int           `json:"key_count"`
	IsTruncated           bool          `json:"is_truncated"`
	NextContinuationToken *string       `json:"next_continuation_token"`
	Objects               []browseObject `json:"objects"`
	CommonPrefixes        []string      `json:"common_prefixes"`
}

func (h *Handler) browseObjects(w http.ResponseWriter, r *http.Request) {
	rb, be, ok := h.resolveBrowseContext(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()

	clientPrefix := q.Get("prefix")

	// Default delimiter to "/" unless the caller explicitly provides the param
	// (even as an empty string, which means flat/recursive listing).
	delimiter := "/"
	if _, provided := q["delimiter"]; provided {
		delimiter = q.Get("delimiter")
	}

	maxKeys := int32(100)
	if v := q.Get("max_keys"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "max_keys must be an integer between 1 and 1000")
			return
		}
		maxKeys = int32(n)
	}

	out, err := be.ListObjects(r.Context(), backend.ListObjectsInput{
		Bucket:            rb.BackendBucket,
		Prefix:            rb.PrefixKey(clientPrefix),
		Delimiter:         delimiter,
		MaxKeys:           maxKeys,
		ContinuationToken: q.Get("continuation_token"),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream storage error: "+err.Error())
		return
	}

	objects := make([]browseObject, 0, len(out.Contents))
	for _, obj := range out.Contents {
		sc := obj.StorageClass
		if sc == "" {
			sc = "STANDARD"
		}
		objects = append(objects, browseObject{
			Key:          rb.StripPrefix(obj.Key),
			Size:         obj.Size,
			LastModified: obj.LastModified.UTC().Format(time.RFC3339),
			ETag:         quoteETag(obj.ETag),
			StorageClass: sc,
		})
	}

	strippedPrefixes := make([]string, len(out.CommonPrefixes))
	for i, cp := range out.CommonPrefixes {
		strippedPrefixes[i] = rb.StripPrefix(cp)
	}

	var nextToken *string
	if out.IsTruncated && out.NextContinuationToken != "" {
		t := out.NextContinuationToken
		nextToken = &t
	}

	writeJSON(w, http.StatusOK, browseListResponse{
		Prefix:                clientPrefix,
		Delimiter:             delimiter,
		MaxKeys:               maxKeys,
		KeyCount:              len(objects) + len(strippedPrefixes),
		IsTruncated:           out.IsTruncated,
		NextContinuationToken: nextToken,
		Objects:               objects,
		CommonPrefixes:        strippedPrefixes,
	})
}

// ---------------------------------------------------------------------------
// GET /{storeID}/buckets/{mappingID}/browse/metadata
// ---------------------------------------------------------------------------

func (h *Handler) browseMetadata(w http.ResponseWriter, r *http.Request) {
	rb, be, ok := h.resolveBrowseContext(w, r)
	if !ok {
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	out, err := be.HeadObject(r.Context(), backend.HeadObjectInput{
		Bucket: rb.BackendBucket,
		Key:    rb.PrefixKey(key),
	})
	if err != nil {
		if backend.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "object not found")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream storage error: "+err.Error())
		return
	}

	userMeta := out.Metadata
	if userMeta == nil {
		userMeta = map[string]string{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"key":           key,
		"size":          out.ContentLength,
		"last_modified": out.LastModified.UTC().Format(time.RFC3339),
		"etag":          quoteETag(out.ETag),
		"content_type":  out.ContentType,
		"storage_class": "",
		"user_metadata": userMeta,
	})
}

// ---------------------------------------------------------------------------
// POST /{storeID}/buckets/{mappingID}/browse/presign
// ---------------------------------------------------------------------------

const (
	presignDefaultTTL = 3600       // 1 hour
	presignMaxTTL     = 7 * 86400  // 7 days
)

func (h *Handler) browsePresign(w http.ResponseWriter, r *http.Request) {
	rb, be, ok := h.resolveBrowseContext(w, r)
	if !ok {
		return
	}

	var req struct {
		Key       string `json:"key"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	if req.ExpiresIn == 0 {
		req.ExpiresIn = presignDefaultTTL
	}
	if req.ExpiresIn < 1 || req.ExpiresIn > presignMaxTTL {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("expires_in must be between 1 and %d seconds", presignMaxTTL))
		return
	}

	expiry := time.Duration(req.ExpiresIn) * time.Second
	out, err := be.PresignURL(r.Context(), backend.PresignInput{
		Bucket:  rb.BackendBucket,
		Key:     rb.PrefixKey(req.Key),
		Method:  "GET",
		Expires: expiry,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "presigned URLs are not supported by this backend: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"url":        out.URL,
		"expires_at": time.Now().Add(expiry).UTC().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// GET /{storeID}/buckets/{mappingID}/browse/size-stream
// ---------------------------------------------------------------------------

func (h *Handler) browseSizeStream(w http.ResponseWriter, r *http.Request) {
	rb, be, ok := h.resolveBrowseContext(w, r)
	if !ok {
		return
	}

	clientPrefix := r.URL.Query().Get("prefix")

	flusher, canFlush := w.(http.Flusher)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)
	if canFlush {
		flusher.Flush()
	}

	var (
		scanned    int64
		totalBytes int64
		token      string
	)

	for {
		out, err := be.ListObjects(r.Context(), backend.ListObjectsInput{
			Bucket:            rb.BackendBucket,
			Prefix:            rb.PrefixKey(clientPrefix),
			MaxKeys:           1000,
			ContinuationToken: token,
		})
		if err != nil {
			sseWrite(w, "error", map[string]string{"error": "upstream storage error: " + err.Error()})
			if canFlush {
				flusher.Flush()
			}
			return
		}

		for _, obj := range out.Contents {
			scanned++
			totalBytes += obj.Size
		}

		sseWrite(w, "progress", map[string]int64{"scanned": scanned, "total_bytes": totalBytes})
		if canFlush {
			flusher.Flush()
		}

		if !out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}

	sseWrite(w, "done", map[string]int64{"scanned": scanned, "total_bytes": totalBytes})
	if canFlush {
		flusher.Flush()
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// quoteETag wraps an ETag in double quotes if it is not already quoted.
func quoteETag(etag string) string {
	if len(etag) == 0 {
		return etag
	}
	if etag[0] == '"' {
		return etag
	}
	return `"` + etag + `"`
}

// sseWrite writes a single SSE event frame to w.
func sseWrite(w http.ResponseWriter, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}
