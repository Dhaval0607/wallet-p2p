// Package obs holds the observability plumbing: structured logging with a
// publicly-tailable ring buffer, and Prometheus metrics.
package obs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
)

type ctxKey int

const correlationIDKey ctxKey = iota

// WithCorrelationID returns a context carrying the per-request correlation id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

// CorrelationID pulls the correlation id back out, or "" if absent.
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey).(string)
	return id
}

// correlationHandler stamps every record with the correlation id from context,
// so a caller never has to remember to pass it and no log line can lose it.
type correlationHandler struct{ slog.Handler }

func (h correlationHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlation_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h correlationHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return correlationHandler{h.Handler.WithAttrs(as)}
}

func (h correlationHandler) WithGroup(name string) slog.Handler {
	return correlationHandler{h.Handler.WithGroup(name)}
}

// NewLogger builds the process logger. Output is JSON on stdout (for the host's
// log drain) and simultaneously into the ring buffer (for /logs, which is what
// makes the logs publicly viewable without handing out dashboard credentials).
func NewLogger(stdout io.Writer, ring *Ring, level slog.Level) *slog.Logger {
	w := io.MultiWriter(stdout, ring)
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(correlationHandler{h})
}

// ---------------------------------------------------------------------------
// Ring buffer + fan-out to live tailers.
// ---------------------------------------------------------------------------

// Ring is a fixed-size in-memory buffer of the most recent log lines plus a
// fan-out to live subscribers. It is an io.Writer so it can sit behind slog.
//
// Bounded memory is the whole point: a free-tier container has ~512MB and a
// burst script can emit thousands of lines a second. Old lines are dropped,
// slow subscribers are dropped -- logging must never block or OOM the money path.
type Ring struct {
	mu    sync.RWMutex
	buf   [][]byte
	size  int
	next  int
	count int
	subs  map[chan []byte]struct{}
}

// NewRing creates a ring retaining the last size lines.
func NewRing(size int) *Ring {
	if size <= 0 {
		size = 1
	}
	return &Ring{
		buf:  make([][]byte, size),
		size: size,
		subs: make(map[chan []byte]struct{}),
	}
}

// Write implements io.Writer. slog hands us exactly one JSON line per call.
func (r *Ring) Write(p []byte) (int, error) {
	line := make([]byte, len(p))
	copy(line, p)

	r.mu.Lock()
	r.buf[r.next] = line
	r.next = (r.next + 1) % r.size
	if r.count < r.size {
		r.count++
	}
	subs := make([]chan []byte, 0, len(r.subs))
	for c := range r.subs {
		subs = append(subs, c)
	}
	r.mu.Unlock()

	for _, c := range subs {
		select {
		case c <- line:
		default: // subscriber is behind; drop rather than block the request path
		}
	}
	return len(p), nil
}

// Snapshot returns up to n most recent lines, oldest first.
func (r *Ring) Snapshot(n int) [][]byte {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if n <= 0 || n > r.count {
		n = r.count
	}
	out := make([][]byte, 0, n)
	// The oldest of the n most recent lines sits n slots behind the write head.
	start := ((r.next-n)%r.size + r.size) % r.size
	for i := 0; i < n; i++ {
		out = append(out, r.buf[(start+i)%r.size])
	}
	return out
}

// Subscribe registers a live tailer. The returned cancel func must be called.
func (r *Ring) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 256)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()

	return ch, func() {
		r.mu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.mu.Unlock()
	}
}

// Subscribers reports the current live tailer count (surfaced on /logs).
func (r *Ring) Subscribers() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}

// Valid reports whether b is a single well-formed JSON object, used by the
// viewer endpoint to skip anything that did not come from slog.
func Valid(b []byte) bool { return json.Valid(b) }
