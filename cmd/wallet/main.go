// Command wallet runs the wallet & P2P transfer service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dhaval0607/wallet-p2p/internal/api"
	"github.com/Dhaval0607/wallet-p2p/internal/config"
	"github.com/Dhaval0607/wallet-p2p/internal/obs"
	"github.com/Dhaval0607/wallet-p2p/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	// -healthcheck turns the same binary into its own health probe. This is what
	// lets the Docker HEALTHCHECK work on a distroless base image that contains
	// no shell, no curl and no wget -- the alternative is fattening the runtime
	// image purely so it can curl itself.
	healthcheck := flag.Bool("healthcheck", false, "probe the local server and exit 0 if healthy")
	flag.Parse()

	if *healthcheck {
		if err := probe(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if err := run(); err != nil {
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// probe hits the local /healthz and reports the result via exit code.
func probe() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ring := obs.NewRing(cfg.LogRingSize)
	log := obs.NewLogger(os.Stdout, ring, cfg.LogLevel)
	slog.SetDefault(log)

	metrics := obs.NewMetrics()

	// Signals first, so a SIGTERM arriving during a slow database connect still
	// stops the process instead of being ignored until we reach Serve.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := connectDB(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	st := store.New(pool)
	st.OnRetry(func(sqlstate string) {
		if sqlstate == "" {
			sqlstate = "unknown"
		}
		metrics.DBRetries.WithLabelValues(sqlstate).Inc()
		log.Warn("retrying transaction after transient postgres error",
			slog.String("event", "db_tx_retry"), slog.String("sqlstate", sqlstate))
	})

	metrics.RegisterPoolGauge(
		func() int32 { return pool.Stat().AcquiredConns() },
		func() int32 { return pool.Stat().TotalConns() },
	)

	if cfg.MigrateOnStart {
		mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := store.Migrate(mctx, pool)
		cancel()
		if err != nil {
			return err
		}
		log.Info("schema applied", slog.String("event", "migrate_ok"))
	}

	go sampleInvariants(ctx, st, metrics, log, cfg.InvariantSample)

	handler := api.New(api.Config{
		Store:      st,
		Logger:     log,
		Metrics:    metrics,
		Ring:       ring,
		AdminToken: cfg.AdminToken,
		Version:    cfg.Version,
	})

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: handler,
		// No WriteTimeout: /logs/stream is a long-lived SSE connection and a
		// write deadline would sever it every N seconds. Per-request bounds come
		// from context timeouts in the handlers instead.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening",
			slog.String("event", "server_start"),
			slog.String("addr", srv.Addr),
			slog.String("version", cfg.Version),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting, let in-flight money transactions finish
	// rather than aborting a half-committed request under a rolling deploy.
	log.Info("shutting down", slog.String("event", "server_stop"))
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	return srv.Shutdown(sctx)
}

func connectDB(ctx context.Context, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	poolCfg.MaxConns = cfg.MaxDBConns
	poolCfg.MinConns = cfg.MinDBConns
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}

	// Free-tier Postgres instances cold-start. Retry rather than crash-loop:
	// the host would otherwise mark the deploy failed before the database wakes.
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 1; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pctx)
		cancel()
		if err == nil {
			log.Info("database connected", slog.String("event", "db_connected"))
			return pool, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			pool.Close()
			return nil, err
		}
		log.Warn("database not ready, retrying",
			slog.String("event", "db_connect_retry"),
			slog.Int("attempt", attempt),
			slog.String("error", err.Error()),
		)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// sampleInvariants keeps the conservation gauges fresh even when nobody is
// polling /invariants, so a break shows up on the dashboard and in any scrape
// history rather than only when someone thinks to look.
func sampleInvariants(ctx context.Context, st *store.Store, m *obs.Metrics, log *slog.Logger, every time.Duration) {
	if every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			inv, err := st.CheckInvariants(cctx)
			cancel()
			if err != nil {
				continue
			}
			m.TotalBalance.Set(float64(inv.TotalBalancePaise))
			m.LedgerSum.Set(float64(inv.LedgerSumPaise))

			if !inv.AllHold {
				log.Error("INVARIANT VIOLATION",
					slog.String("event", "invariant_violation"),
					slog.Bool("conservation", inv.Conservation),
					slog.Bool("no_overdraft", inv.NoOverdraft),
					slog.Int64("total_balance_paise", inv.TotalBalancePaise),
					slog.Int64("total_minted_paise", inv.TotalMintedPaise),
					slog.Int64("ledger_sum_paise", inv.LedgerSumPaise),
					slog.Int64("negative_balance_wallets", inv.NegativeBalances),
				)
			}
		}
	}
}
