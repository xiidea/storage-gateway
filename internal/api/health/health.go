package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type checkResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type response struct {
	Status string                 `json:"status"`
	Checks map[string]checkResult `json:"checks"`
}

// Handler performs liveness/readiness checks against Postgres and Redis.
// It is intentionally unauthenticated and should be mounted outside any
// auth middleware so load-balancer and k8s probes can reach it freely.
type Handler struct {
	pool *pgxpool.Pool
	rdb  *redis.Client
}

// New returns a Handler that probes the given pool and Redis client.
func New(pool *pgxpool.Pool, rdb *redis.Client) *Handler {
	return &Handler{pool: pool, rdb: rdb}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	resp := response{
		Status: "ok",
		Checks: make(map[string]checkResult),
	}

	if err := h.pool.Ping(ctx); err != nil {
		resp.Checks["database"] = checkResult{Status: "error", Error: err.Error()}
		resp.Status = "degraded"
	} else {
		resp.Checks["database"] = checkResult{Status: "ok"}
	}

	if err := h.rdb.Ping(ctx).Err(); err != nil {
		resp.Checks["redis"] = checkResult{Status: "error", Error: err.Error()}
		resp.Status = "degraded"
	} else {
		resp.Checks["redis"] = checkResult{Status: "ok"}
	}

	code := http.StatusOK
	if resp.Status != "ok" {
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}
