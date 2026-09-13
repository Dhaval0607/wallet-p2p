package obs

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Transfer outcome labels. These are the domain counters the exercise asks for:
// transfers created / declined-insufficient-funds / idempotent-replays.
const (
	OutcomeSucceeded    = "succeeded"
	OutcomeInsufficient = "declined_insufficient_funds"
	OutcomeReplay       = "idempotent_replay"
	OutcomeConflict     = "idempotency_key_conflict"
	OutcomeInvalid      = "rejected_invalid"
)

// Metrics is the process metric set. Held explicitly rather than in package
// globals so tests can build a throwaway registry.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec   // rate + error rate
	HTTPDuration *prometheus.HistogramVec // latency p99
	HTTPInFlight prometheus.Gauge

	Transfers      *prometheus.CounterVec // by outcome
	TransferAmount prometheus.Counter     // total paise successfully moved
	WalletsCreated prometheus.Counter
	WalletsReused  prometheus.Counter // get-or-create that found an existing wallet
	MintedPaise    prometheus.Counter

	DBRetries   *prometheus.CounterVec // serialization failures / deadlocks retried
	DBPoolInUse prometheus.GaugeFunc

	TotalBalance prometheus.Gauge // refreshed by the invariant sampler
	LedgerSum    prometheus.Gauge // must stay pinned at 0
}

// NewMetrics registers the full metric set on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{Registry: reg}

	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests by route and status class.",
	}, []string{"method", "route", "status"})

	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_request_duration_seconds",
		Help: "HTTP request latency in seconds.",
		// Buckets skew low-and-tight: this is a single-digit-millisecond
		// database write path, and p99 is meaningless if every real request
		// lands in the first bucket.
		Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"method", "route"})

	m.HTTPInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "http_requests_in_flight",
		Help: "HTTP requests currently being served.",
	})

	m.Transfers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wallet_transfers_total",
		Help: "Transfer attempts by terminal outcome.",
	}, []string{"outcome"})

	m.TransferAmount = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wallet_transferred_paise_total",
		Help: "Total paise moved by succeeded transfers.",
	})

	m.WalletsCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wallet_wallets_created_total",
		Help: "Wallets actually created by get-or-create.",
	})

	m.WalletsReused = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wallet_wallets_reused_total",
		Help: "Get-or-create calls that returned an existing wallet.",
	})

	m.MintedPaise = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wallet_minted_paise_total",
		Help: "Total paise minted into the system from outside (test funding).",
	})

	m.DBRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wallet_db_retries_total",
		Help: "Transactions retried after a transient Postgres error.",
	}, []string{"sqlstate"})

	m.TotalBalance = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wallet_total_balance_paise",
		Help: "Sum of all wallet balances. Must equal total minted paise.",
	})

	m.LedgerSum = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wallet_ledger_sum_paise",
		Help: "Sum of all double-entry ledger deltas. Must be exactly 0.",
	})

	reg.MustRegister(
		m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight,
		m.Transfers, m.TransferAmount,
		m.WalletsCreated, m.WalletsReused, m.MintedPaise,
		m.DBRetries, m.TotalBalance, m.LedgerSum,
	)
	return m
}

// RegisterPoolGauge exposes pgx pool saturation, which is the first thing that
// pins under a burst and the usual explanation for a latency cliff.
func (m *Metrics) RegisterPoolGauge(acquired func() int32, total func() int32) {
	m.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "wallet_db_pool_acquired_conns",
		Help: "Connections currently checked out of the pgx pool.",
	}, func() float64 { return float64(acquired()) }))

	m.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "wallet_db_pool_total_conns",
		Help: "Total connections held by the pgx pool.",
	}, func() float64 { return float64(total()) }))
}
