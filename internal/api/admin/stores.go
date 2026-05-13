package admin

import (
	"encoding/json"
	"net/http"

	"storage-gateway/internal/auth"
	"storage-gateway/internal/registry"
)

// POST /tenants/{tenantID}/stores
func (h *Handler) createStore(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuidParam(r, "tenantID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}

	var req struct {
		Name          string                 `json:"name"`
		BackendType   registry.BackendType   `json:"backend_type"`
		BackendConfig json.RawMessage        `json:"backend_config"`
		PresignedMode registry.PresignedMode `json:"presigned_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !validBackendType(req.BackendType) {
		writeError(w, http.StatusBadRequest, "backend_type must be one of: s3, gcs, r2, azure, local")
		return
	}
	if len(req.BackendConfig) == 0 || string(req.BackendConfig) == "null" {
		writeError(w, http.StatusBadRequest, "backend_config is required")
		return
	}
	if req.PresignedMode == "" {
		req.PresignedMode = registry.PresignedProxy
	}
	if !validPresignedMode(req.PresignedMode) {
		writeError(w, http.StatusBadRequest, "presigned_mode must be proxy or redirect")
		return
	}

	configEnc, err := auth.Encrypt(h.cryptoKey, req.BackendConfig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt backend config")
		return
	}

	store, err := h.mgr.CreateStore(r.Context(), registry.CreateStoreParams{
		TenantID:         tenantID,
		Name:             req.Name,
		BackendType:      req.BackendType,
		BackendConfigEnc: configEnc,
		PresignedMode:    req.PresignedMode,
	})
	if err != nil {
		handleErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toStoreDTO(store))
}

// GET /tenants/{tenantID}/stores
func (h *Handler) listStores(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuidParam(r, "tenantID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}

	stores, err := h.mgr.ListStores(r.Context(), tenantID)
	if err != nil {
		handleErr(w, err)
		return
	}

	dtos := make([]storeDTO, len(stores))
	for i := range stores {
		dtos[i] = toStoreDTO(&stores[i])
	}
	writeJSON(w, http.StatusOK, dtos)
}

// GET /tenants/{tenantID}/stores/{storeID}
func (h *Handler) getStore(w http.ResponseWriter, r *http.Request) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toStoreDTO(store))
}

// PUT /tenants/{tenantID}/stores/{storeID}/backend
//
// Replaces the backend config and/or presigned mode. Automatically invalidates
// the Redis cache and the backend SDK client pool for this store.
func (h *Handler) updateStoreBackend(w http.ResponseWriter, r *http.Request) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return
	}

	var req struct {
		BackendConfig json.RawMessage        `json:"backend_config"`
		PresignedMode registry.PresignedMode `json:"presigned_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.BackendConfig) == 0 || string(req.BackendConfig) == "null" {
		writeError(w, http.StatusBadRequest, "backend_config is required")
		return
	}
	if req.PresignedMode == "" {
		req.PresignedMode = store.PresignedMode // keep existing if not supplied
	}
	if !validPresignedMode(req.PresignedMode) {
		writeError(w, http.StatusBadRequest, "presigned_mode must be proxy or redirect")
		return
	}

	configEnc, err := auth.Encrypt(h.cryptoKey, req.BackendConfig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt backend config")
		return
	}

	// UpdateStoreBackend in cachedRegistry also invalidates bucket cache entries.
	if err := h.mgr.UpdateStoreBackend(r.Context(), store.ID, store.TenantID, configEnc, req.PresignedMode); err != nil {
		handleErr(w, err)
		return
	}
	// Evict the cached SDK client so the next request rebuilds with new credentials.
	h.pool.Invalidate(store.ID)

	w.WriteHeader(http.StatusNoContent)
}

// GET /tenants/{tenantID}/stores/{storeID}/cors
func (h *Handler) getStoreCORS(w http.ResponseWriter, r *http.Request) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return
	}
	origins := store.AllowedOrigins
	if origins == nil {
		origins = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowed_origins": origins})
}

// PUT /tenants/{tenantID}/stores/{storeID}/cors
func (h *Handler) updateStoreCORS(w http.ResponseWriter, r *http.Request) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return
	}

	var req struct {
		AllowedOrigins []string `json:"allowed_origins"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.mgr.UpdateStoreAllowedOrigins(r.Context(), store.ID, store.TenantID, req.AllowedOrigins); err != nil {
		handleErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /tenants/{tenantID}/stores/{storeID}
func (h *Handler) deleteStore(w http.ResponseWriter, r *http.Request) {
	store, ok := h.verifyStoreOwnership(w, r)
	if !ok {
		return
	}

	// DeleteStore in cachedRegistry invalidates bucket cache entries before deleting.
	if err := h.mgr.DeleteStore(r.Context(), store.ID, store.TenantID); err != nil {
		handleErr(w, err)
		return
	}
	h.pool.Invalidate(store.ID)

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

func validBackendType(t registry.BackendType) bool {
	switch t {
	case registry.BackendS3, registry.BackendGCS, registry.BackendR2,
		registry.BackendAzure, registry.BackendLocal:
		return true
	}
	return false
}

func validPresignedMode(m registry.PresignedMode) bool {
	return m == registry.PresignedProxy || m == registry.PresignedRedirect
}
