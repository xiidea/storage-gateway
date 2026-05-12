package admin

import (
	"encoding/json"
	"net/http"
)

// POST /tenants
func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	tenant, err := h.mgr.CreateTenant(r.Context(), req.Name)
	if err != nil {
		handleErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTenantDTO(tenant))
}

// GET /tenants/{tenantID}
func (h *Handler) getTenant(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "tenantID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}

	tenant, err := h.mgr.GetTenant(r.Context(), id)
	if err != nil {
		handleErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTenantDTO(tenant))
}
