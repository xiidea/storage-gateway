# ---------- build stage ----------
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Fetch dependencies first so this layer is cached separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w" \
      -trimpath \
      -o /gateway \
      ./cmd/gateway

# ---------- final stage ----------
FROM alpine:3.21

# ca-certificates: needed for TLS connections to Postgres, Redis, and upstream cloud APIs.
# tzdata: needed for time-zone-aware logging.
RUN apk add --no-cache ca-certificates tzdata

# Run as a non-root user.
RUN addgroup -S gateway && adduser -S gateway -G gateway
USER gateway

COPY --from=builder /gateway /gateway

# Gateway (S3 protocol) and Admin API ports.
EXPOSE 8080 9001

ENTRYPOINT ["/gateway"]
