package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"storage-gateway/internal/backend"
	"storage-gateway/internal/registry"
)

// Handler is the admin HTTP API. It runs on a separate port from the S3 gateway
// and must not be exposed publicly.
type Handler struct {
	mux       http.Handler
	mgr       registry.Manager
	cryptoKey []byte
	pool      *backend.Pool
}

// New wires all admin routes and returns the ready Handler.
// adminToken is the bearer token required on every request.
func New(mgr registry.Manager, cryptoKey []byte, pool *backend.Pool, adminToken string) *Handler {
	h := &Handler{mgr: mgr, cryptoKey: cryptoKey, pool: pool}

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(bearerAuth(adminToken))

	r.Route("/tenants", func(r chi.Router) {
		r.Post("/", h.createTenant)
		r.Get("/{tenantID}", h.getTenant)

		r.Route("/{tenantID}/keys", func(r chi.Router) {
			r.Post("/", h.createAccessKey)
			r.Get("/", h.listAccessKeys)
			r.Delete("/{keyID}", h.revokeAccessKey)
		})

		r.Route("/{tenantID}/stores", func(r chi.Router) {
			r.Post("/", h.createStore)
			r.Get("/", h.listStores)
			r.Get("/{storeID}", h.getStore)
			r.Put("/{storeID}/backend", h.updateStoreBackend)
			r.Delete("/{storeID}", h.deleteStore)

			r.Route("/{storeID}/buckets", func(r chi.Router) {
				r.Post("/", h.createBucketMapping)
				r.Get("/", h.listBucketMappings)
				r.Delete("/{mappingID}", h.deleteBucketMapping)
			})
		})
	})

	h.mux = r
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func bearerAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				writeError(w, http.StatusUnauthorized, "invalid or missing admin token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// Shared helper
// ---------------------------------------------------------------------------

// verifyStoreOwnership loads a store and confirms it belongs to tenantID.
// Returns (store, true) on success or writes the error response and returns (nil, false).
func (h *Handler) verifyStoreOwnership(w http.ResponseWriter, r *http.Request) (*registry.Store, bool) {
	tenantID, err := uuidParam(r, "tenantID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tenant_id")
		return nil, false
	}
	storeID, err := uuidParam(r, "storeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid store_id")
		return nil, false
	}

	store, err := h.mgr.GetStore(r.Context(), storeID)
	if err != nil {
		handleErr(w, err)
		return nil, false
	}
	if store.TenantID != tenantID {
		writeError(w, http.StatusNotFound, "store not found")
		return nil, false
	}
	return store, true
}
