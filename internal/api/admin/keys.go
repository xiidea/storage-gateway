package admin

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"

	"storage-gateway/internal/auth"
)

// POST /tenants/{tenantID}/keys
//
// Generates a new access key pair. The plaintext secret is returned once in
// this response and is never retrievable again — callers must store it immediately.
func (h *Handler) createAccessKey(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuidParam(r, "tenantID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}

	// Verify tenant exists before generating credentials.
	if _, err := h.mgr.GetTenant(r.Context(), tenantID); err != nil {
		handleErr(w, err)
		return
	}

	accessKeyID, err := generateAccessKeyID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate key")
		return
	}
	secretKey, err := generateSecretKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate secret")
		return
	}

	secretKeyEnc, err := auth.Encrypt(h.cryptoKey, []byte(secretKey))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt secret")
		return
	}

	row, err := h.mgr.CreateAccessKey(r.Context(), tenantID, accessKeyID, secretKeyEnc)
	if err != nil {
		handleErr(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, createKeyResponse{
		ID:        row.ID,
		TenantID:  tenantID,
		AccessKey: row.AccessKey,
		SecretKey: secretKey, // plaintext — only shown at creation
		CreatedAt: row.CreatedAt,
	})
}

// GET /tenants/{tenantID}/keys
func (h *Handler) listAccessKeys(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuidParam(r, "tenantID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}

	keys, err := h.mgr.ListAccessKeys(r.Context(), tenantID)
	if err != nil {
		handleErr(w, err)
		return
	}

	dtos := make([]keyDTO, len(keys))
	for i, k := range keys {
		dtos[i] = toKeyDTO(k)
	}
	writeJSON(w, http.StatusOK, dtos)
}

// DELETE /tenants/{tenantID}/keys/{keyID}
func (h *Handler) revokeAccessKey(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuidParam(r, "tenantID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}
	keyID, err := uuidParam(r, "keyID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid key_id")
		return
	}

	// cachedRegistry.RevokeAccessKey fetches the key string and invalidates the
	// Redis cache entry before revoking, so the next request sees the revocation.
	if err := h.mgr.RevokeAccessKey(r.Context(), keyID, tenantID); err != nil {
		handleErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Key generation
// ---------------------------------------------------------------------------

// generateAccessKeyID returns "SGW" + 17 uppercase alphanumeric characters.
func generateAccessKeyID() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 17)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "SGW" + string(b), nil
}

// generateSecretKey returns a 40-character URL-safe base64 string derived from
// 30 cryptographically random bytes (240 bits of entropy).
func generateSecretKey() (string, error) {
	b := make([]byte, 30)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil // exactly 40 chars
}
