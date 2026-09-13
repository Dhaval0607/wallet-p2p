// Package api wires the HTTP surface: routing, middleware, and the public
// observability endpoints.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Dhaval0607/wallet-p2p/internal/obs"
	"github.com/Dhaval0607/wallet-p2p/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server holds the handler dependencies.
type Server struct {
	store      *store.Store
	log        *slog.Logger
	metrics    *obs.Metrics
	ring       *obs.Ring
	adminToken string
	version    string
	startedAt  time.Time
}

// Config configures the HTTP server.
type Config struct {
	Store      *store.Store
	Logger     *slog.Logger
	Metrics    *obs.Metrics
	Ring       *obs.Ring
	AdminToken string
	Version    string
}

// New builds the fully-routed handler.
func New(cfg Config) http.Handler {
	s := &Server{
		store:      cfg.Store,
		log:        cfg.Logger,
		metrics:    cfg.Metrics,
		ring:       cfg.Ring,
		adminToken: cfg.AdminToken,
		version:    cfg.Version,
		startedAt:  time.Now(),
	}

	mux := http.NewServeMux()

	// --- money API ---
	mux.HandleFunc("POST /wallets", s.authenticate(s.handleCreateWallet))
	mux.HandleFunc("GET /wallets/{id}", s.authenticate(s.handleGetWallet))
	mux.HandleFunc("POST /transfers", s.authenticate(s.handleCreateTransfer))
	mux.HandleFunc("GET /transfers/{id}", s.authenticate(s.handleGetTransfer))

	// --- test funding (admin) ---
	mux.HandleFunc("POST /admin/mint", s.requireAdmin(s.handleMint))

	// --- observability, all public on purpose ---
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /invariants", s.handleInvariants)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{
		Registry:          s.metrics.Registry,
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("GET /logs/stream", s.handleLogStream)
	mux.HandleFunc("GET /logs/recent", s.handleLogsRecent)
	mux.HandleFunc("GET /logs", s.handleLogViewer)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("GET /{$}", s.handleIndex)

	return s.observe(mux)
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// handleHealth is liveness: the process is up. Deliberately does NOT touch the
// database -- a liveness probe that fails on a database blip would have the host
// restart a perfectly healthy container and turn a brief outage into a long one.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        s.version,
		"uptime_seconds": int(time.Since(s.startedAt).Seconds()),
	})
}

// handleReady is readiness: can we actually serve money operations? This one
// does check the database, because without it every write would fail.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.Pool().Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded",
			"db":     "unreachable",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "db": "ok"})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "wallet-p2p",
		"version": s.version,
		"money":   "all amounts are integer paise; there is no float in this service",
		"endpoints": map[string]string{
			"POST /wallets":       "get-or-create the caller's wallet (bearer token = identity)",
			"GET /wallets/{id}":   "current balance",
			"POST /transfers":     "move money; body: from, to, amount_paise, idempotency_key",
			"GET /transfers/{id}": "transfer status",
			"POST /admin/mint":    "test funding (admin bearer token); NOT a transfer",
			"GET /invariants":     "live invariant audit, recomputed from base tables",
			"GET /metrics":        "prometheus metrics",
			"GET /dashboard":      "live metrics dashboard",
			"GET /logs":           "live structured log stream (public)",
			"GET /healthz":        "liveness",
			"GET /readyz":         "readiness (checks database)",
		},
	})
}

// ---------------------------------------------------------------------------
// Public log access
//
// The exercise asks for publicly-viewable logs. Handing out host-dashboard
// credentials is not that, so the process tees its own JSON log stream into a
// bounded ring buffer and serves it over SSE. Anyone with the URL can watch the
// domain events scroll past during a burst, with no login.
// ---------------------------------------------------------------------------

func (s *Server) handleLogsRecent(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 5000 {
			n = parsed
		}
	}

	lines := s.ring.Snapshot(n)
	out := make([]json.RawMessage, 0, len(lines))
	for _, l := range lines {
		if obs.Valid(l) {
			out = append(out, json.RawMessage(l))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":       len(out),
		"subscribers": s.ring.Subscribers(),
		"lines":       out,
	})
}

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "streaming_unsupported", "server cannot stream")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat proxy buffering on PaaS hosts
	w.WriteHeader(http.StatusOK)

	// Replay recent history so a viewer who connects mid-burst sees context.
	for _, line := range s.ring.Snapshot(100) {
		if obs.Valid(line) {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
	}
	flusher.Flush()

	ch, cancel := s.ring.Subscribe()
	defer cancel()

	// Heartbeat keeps intermediary proxies from dropping an idle connection.
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
