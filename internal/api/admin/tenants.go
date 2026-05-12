package admin

import (
	"encoding/json"
	"net/http"
)

// GET /tenants
func (h *Handler) listTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := h.mgr.ListTenants(r.Context())
	if err != nil {
		handleErr(w, err)
		return
	}

	dtos := make([]tenantDTO, len(tenants))
	for i := range tenants {
		dtos[i] = toTenantDTO(&tenants[i])
	}
	writeJSON(w, http.StatusOK, dtos)
}

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
