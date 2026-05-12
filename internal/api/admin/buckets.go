package admin

import (
	"encoding/json"
	"net/http"
)

// POST /tenants/{tenantID}/stores/{storeID}/buckets
func (h *Handler) createBucketMapping(w http.ResponseWriter, r *http.Request) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return
	}

	var req struct {
		GatewayBucket string `json:"gateway_bucket"`
		BackendBucket string `json:"backend_bucket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.GatewayBucket == "" {
		writeError(w, http.StatusBadRequest, "gateway_bucket is required")
		return
	}
	if req.BackendBucket == "" {
		writeError(w, http.StatusBadRequest, "backend_bucket is required")
		return
	}

	bm, err := h.mgr.CreateBucketMapping(r.Context(), store.ID, req.GatewayBucket, req.BackendBucket)
	if err != nil {
		handleErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toBucketMappingDTO(*bm))
}

// GET /tenants/{tenantID}/stores/{storeID}/buckets
func (h *Handler) listBucketMappings(w http.ResponseWriter, r *http.Request) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return
	}

	mappings, err := h.mgr.ListBucketMappings(r.Context(), store.ID)
	if err != nil {
		handleErr(w, err)
		return
	}

	dtos := make([]bucketMappingDTO, len(mappings))
	for i, bm := range mappings {
		dtos[i] = toBucketMappingDTO(bm)
	}
	writeJSON(w, http.StatusOK, dtos)
}

// DELETE /tenants/{tenantID}/stores/{storeID}/buckets/{mappingID}
func (h *Handler) deleteBucketMapping(w http.ResponseWriter, r *http.Request) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return
	}
	mappingID, err := uuidParam(r, "mappingID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid mapping_id")
		return
	}

	// cachedRegistry.DeleteBucketMapping fetches the gateway_bucket name first
	// so it can evict the Redis cache entry before deleting.
	if err := h.mgr.DeleteBucketMapping(r.Context(), mappingID, store.ID); err != nil {
		handleErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
