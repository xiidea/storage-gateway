package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	DatabaseURL   string
	RedisURL      string
	CacheTTL      time.Duration
	MasterKey     string
	GatewayAddr   string
	AdminAddr     string
	GatewayRegion string
}

func Load() (*Config, error) {
	cfg := &Config{
		RedisURL:      env("REDIS_URL", "redis://localhost:6379"),
		CacheTTL:      5 * time.Minute,
		GatewayAddr:   env("GATEWAY_ADDR", ":8080"),
		AdminAddr:     env("ADMIN_ADDR", ":9001"),
		GatewayRegion: env("GATEWAY_REGION", "us-east-1"),
	}

	var missing []string
	cfg.DatabaseURL = require("DATABASE_URL", &missing)
	cfg.MasterKey = require("MASTER_KEY", &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %v", missing)
	}

	if d := os.Getenv("CACHE_TTL"); d != "" {
		var err error
		if cfg.CacheTTL, err = time.ParseDuration(d); err != nil {
			return nil, fmt.Errorf("invalid CACHE_TTL %q: %w", d, err)
		}
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func require(key string, missing *[]string) string {
	v := os.Getenv(key)
	if v == "" {
		*missing = append(*missing, key)
	}
	return v
}
