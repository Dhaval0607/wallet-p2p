// Package config reads process configuration from the environment.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full set of knobs. Everything has a working default except the
// database URL, so `docker compose up` needs no .env file.
type Config struct {
	Port            string
	DatabaseURL     string
	AdminToken      string
	LogLevel        slog.Level
	LogRingSize     int
	MaxDBConns      int32
	MinDBConns      int32
	ShutdownGrace   time.Duration
	MigrateOnStart  bool
	InvariantSample time.Duration
	Version         string
}

// Load reads the environment, applying defaults.
func Load() (Config, error) {
	c := Config{
		Port:            env("PORT", "8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		AdminToken:      env("ADMIN_TOKEN", "dev-admin-token"),
		LogRingSize:     envInt("LOG_RING_SIZE", 5000),
		MaxDBConns:      int32(envInt("DB_MAX_CONNS", 20)),
		MinDBConns:      int32(envInt("DB_MIN_CONNS", 2)),
		ShutdownGrace:   time.Duration(envInt("SHUTDOWN_GRACE_SECONDS", 15)) * time.Second,
		MigrateOnStart:  env("MIGRATE_ON_START", "true") == "true",
		InvariantSample: time.Duration(envInt("INVARIANT_SAMPLE_SECONDS", 15)) * time.Second,
		Version:         env("APP_VERSION", "dev"),
	}

	switch strings.ToLower(env("LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		c.LogLevel = slog.LevelInfo
	}

	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if c.MinDBConns > c.MaxDBConns {
		return c, fmt.Errorf("DB_MIN_CONNS (%d) exceeds DB_MAX_CONNS (%d)", c.MinDBConns, c.MaxDBConns)
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
