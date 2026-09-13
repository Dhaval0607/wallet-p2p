package store_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Dhaval0607/wallet-p2p/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These are integration tests on purpose. The invariants this service claims
// live in Postgres -- in unique indexes, row locks and CHECK constraints -- so a
// test with a mocked database would verify nothing that matters. Set
// TEST_DATABASE_URL to run them; `make test` and CI both do.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		fmt.Println("TEST_DATABASE_URL not set; skipping store integration tests")
		os.Exit(0)
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		fmt.Println("bad TEST_DATABASE_URL:", err)
		os.Exit(1)
	}
	cfg.MaxConns = 25

	testPool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	if err := testPool.Ping(ctx); err != nil {
		fmt.Println("ping:", err)
		os.Exit(1)
	}
	if err := store.Migrate(ctx, testPool); err != nil {
		fmt.Println("migrate:", err)
		os.Exit(1)
	}

	code := m.Run()
	testPool.Close()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return store.New(testPool)
}

// freshWallet provisions a brand-new user and their wallet, funded with `paise`.
func freshWallet(t *testing.T, s *store.Store, paise int64) store.Wallet {
	t.Helper()
	ctx := context.Background()

	token := fmt.Sprintf("tok_%s_%d", t.Name(), time.Now().UnixNano())
	u, err := s.UpsertUser(ctx, token)
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	w, _, err := s.GetOrCreateWallet(ctx, u.ID)
	if err != nil {
		t.Fatalf("create wallet: %v", err)
	}
	if paise > 0 {
		w, _, err = s.Mint(ctx, w.ID, fmt.Sprintf("mint_%s", w.ID), paise)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
	}
	return w
}

func balance(t *testing.T, s *store.Store, id string) int64 {
	t.Helper()
	w, err := s.GetWallet(context.Background(), id)
	if err != nil {
		t.Fatalf("get wallet %s: %v", id, err)
	}
	return w.BalancePaise
}

// ---------------------------------------------------------------------------
// Invariant 4 -- race-free get-or-create
// ---------------------------------------------------------------------------

func TestConcurrentGetOrCreateYieldsOneWallet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	u, err := s.UpsertUser(ctx, fmt.Sprintf("tok_goc_%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}

	const racers = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     = map[string]int{}
		created int
		errs    []error
		start   = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines at the same instant
			w, wasCreated, err := s.GetOrCreateWallet(ctx, u.ID)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids[w.ID]++
			if wasCreated {
				created++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d/%d calls errored, first: %v", len(errs), racers, errs[0])
	}
	if len(ids) != 1 {
		t.Fatalf("expected exactly 1 wallet across %d concurrent creates, got %d: %v", racers, len(ids), ids)
	}
	if created != 1 {
		t.Fatalf("expected exactly 1 caller to report created=true, got %d", created)
	}
}

// ---------------------------------------------------------------------------
// Invariant 3 -- exactly-once transfer
// ---------------------------------------------------------------------------

func TestIdempotentRetryStormAppliesExactlyOneDebit(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	from := freshWallet(t, s, 100_000)
	to := freshWallet(t, s, 0)
	fromUser, toUser := from.UserID, to.UserID
	_ = toUser

	const (
		storm  = 30
		amount = 7_777
	)
	key := fmt.Sprintf("storm_%d", time.Now().UnixNano())

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []store.TransferResult
		errs    []error
		start   = make(chan struct{})
	)

	for i := 0; i < storm; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.Transfer(ctx, store.TransferRequest{
				RequesterUserID: fromUser,
				FromWalletID:    from.ID,
				ToWalletID:      to.ID,
				AmountPaise:     amount,
				IdempotencyKey:  key,
			})

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, res)
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d/%d calls errored, first: %v", len(errs), storm, errs[0])
	}
	if len(results) != storm {
		t.Fatalf("expected %d results, got %d", storm, len(results))
	}

	// Every response must be the same transfer, with the same status.
	first := results[0].Transfer
	realWork := 0
	for i, r := range results {
		if r.Transfer.ID != first.ID {
			t.Fatalf("result %d has transfer id %s, expected %s -- the key was applied more than once",
				i, r.Transfer.ID, first.ID)
		}
		if r.Transfer.Status != first.Status {
			t.Fatalf("result %d status %q != %q", i, r.Transfer.Status, first.Status)
		}
		if !r.Replay {
			realWork++
		}
	}
	if realWork != 1 {
		t.Fatalf("expected exactly 1 non-replay result, got %d", realWork)
	}

	if got, want := balance(t, s, from.ID), int64(100_000-amount); got != want {
		t.Fatalf("source balance %d, want %d -- %d debits applied instead of 1",
			got, want, (100_000-got)/amount)
	}
	if got, want := balance(t, s, to.ID), int64(amount); got != want {
		t.Fatalf("destination balance %d, want %d", got, want)
	}
}

func TestSameKeyDifferentBodyIsConflict(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	from := freshWallet(t, s, 50_000)
	to := freshWallet(t, s, 0)
	key := fmt.Sprintf("conflict_%d", time.Now().UnixNano())

	req := store.TransferRequest{
		RequesterUserID: from.UserID,
		FromWalletID:    from.ID,
		ToWalletID:      to.ID,
		AmountPaise:     1_000,
		IdempotencyKey:  key,
	}
	if _, err := s.Transfer(ctx, req); err != nil {
		t.Fatalf("first transfer: %v", err)
	}

	// Same key, different amount.
	req.AmountPaise = 2_000
	_, err := s.Transfer(ctx, req)
	if err != store.ErrIdempotencyConflict {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}

	if got := balance(t, s, from.ID); got != 49_000 {
		t.Fatalf("the conflicting replay moved money: balance %d, want 49000", got)
	}
}

func TestDeclineIsAlsoIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	from := freshWallet(t, s, 100)
	to := freshWallet(t, s, 0)
	key := fmt.Sprintf("decline_%d", time.Now().UnixNano())

	req := store.TransferRequest{
		RequesterUserID: from.UserID,
		FromWalletID:    from.ID,
		ToWalletID:      to.ID,
		AmountPaise:     999_999,
		IdempotencyKey:  key,
	}

	first, err := s.Transfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Transfer.Status != store.StatusDeclined {
		t.Fatalf("expected declined, got %q", first.Transfer.Status)
	}

	// The retry must return the identical stored decision, not re-evaluate it.
	second, err := s.Transfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replay {
		t.Fatal("retry of a declined transfer was not reported as a replay")
	}
	if second.Transfer.ID != first.Transfer.ID || second.Transfer.Status != store.StatusDeclined {
		t.Fatalf("retry returned a different outcome: %+v vs %+v", second.Transfer, first.Transfer)
	}
	if got := balance(t, s, from.ID); got != 100 {
		t.Fatalf("a declined transfer moved money: balance %d, want 100", got)
	}
}

// ---------------------------------------------------------------------------
// Invariants 1 and 2 -- conservation and no overdraft, under contention
// ---------------------------------------------------------------------------

func TestConservationAndNoOverdraftUnderContention(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const (
		nWallets = 6
		seed     = 200_000
		nMoves   = 600
		workers  = 40
	)

	wallets := make([]store.Wallet, nWallets)
	for i := range wallets {
		wallets[i] = freshWallet(t, s, seed)
	}
	totalBefore := int64(0)
	for _, w := range wallets {
		totalBefore += balance(t, s, w.ID)
	}

	type job struct{ from, to, amount int }
	jobs := make(chan job, nMoves)
	for n := 0; n < nMoves; n++ {
		a := n % nWallets
		b := (n + 1) % nWallets
		// Every fourth job runs the pair backwards, so A->B and B->A are in
		// flight simultaneously -- the case that deadlocks an unordered locker.
		if n%4 == 0 {
			a, b = b, a
		}
		amount := (n*13)%900 + 100
		// Every seventh job tries to move more than exists anywhere: must decline.
		if n%7 == 0 {
			amount = seed * nWallets * 2
		}
		jobs <- job{from: a, to: b, amount: amount}
	}
	close(jobs)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		declined  int
		errs      []error
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := range jobs {
				from, to := wallets[j.from], wallets[j.to]
				res, err := s.Transfer(ctx, store.TransferRequest{
					RequesterUserID: from.UserID,
					FromWalletID:    from.ID,
					ToWalletID:      to.ID,
					AmountPaise:     int64(j.amount),
					IdempotencyKey:  fmt.Sprintf("contend_%d_%s_%s_%d", time.Now().UnixNano(), from.ID[:8], to.ID[:8], j.amount),
				})

				mu.Lock()
				switch {
				case err != nil:
					errs = append(errs, err)
				case res.Transfer.Status == store.StatusSucceeded:
					succeeded++
				default:
					declined++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	// A deadlock that escapes the retry budget surfaces here. The point of the
	// sorted FOR NO KEY UPDATE is that this list stays empty.
	if len(errs) > 0 {
		t.Fatalf("%d/%d transfers errored under contention, first: %v", len(errs), nMoves, errs[0])
	}
	if declined == 0 {
		t.Fatal("expected some overdraft declines, got none")
	}

	totalAfter := int64(0)
	for _, w := range wallets {
		b := balance(t, s, w.ID)
		if b < 0 {
			t.Fatalf("wallet %s went negative: %d", w.ID, b)
		}
		totalAfter += b
	}

	if totalAfter != totalBefore {
		t.Fatalf("CONSERVATION BROKEN: total %d -> %d (delta %d) across %d successful transfers",
			totalBefore, totalAfter, totalAfter-totalBefore, succeeded)
	}

	inv, err := s.CheckInvariants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.AllHold {
		t.Fatalf("server-side invariant audit failed: %+v", inv)
	}
}

// TestDrainRaceNeverOverdraws points every worker at the same wallet with a
// balance that can only satisfy some of them. It is the sharpest form of the
// no-overdraft check: the wrong implementation (read balance, subtract, write)
// passes the contention test above and fails this one.
func TestDrainRaceNeverOverdraws(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const (
		balanceStart = 10_000
		amount       = 1_000
		racers       = 40 // 4x more claimants than the wallet can fund
	)
	expectedWinners := balanceStart / amount

	from := freshWallet(t, s, balanceStart)
	to := freshWallet(t, s, 0)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		declined  int
		errs      []error
		start     = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := s.Transfer(ctx, store.TransferRequest{
				RequesterUserID: from.UserID,
				FromWalletID:    from.ID,
				ToWalletID:      to.ID,
				AmountPaise:     amount,
				IdempotencyKey:  fmt.Sprintf("drain_%d_%d", time.Now().UnixNano(), i),
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case res.Transfer.Status == store.StatusSucceeded:
				succeeded++
			default:
				declined++
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d/%d errored, first: %v", len(errs), racers, errs[0])
	}
	if succeeded != expectedWinners {
		t.Fatalf("expected exactly %d transfers to succeed against a %d-paise balance, got %d",
			expectedWinners, balanceStart, succeeded)
	}
	if got := balance(t, s, from.ID); got != 0 {
		t.Fatalf("source balance %d, want 0", got)
	}
	if got := balance(t, s, to.ID); got != int64(expectedWinners*amount) {
		t.Fatalf("destination balance %d, want %d", got, expectedWinners*amount)
	}
}

// TestSelfTransferRejected guards the degenerate case that would otherwise try
// to lock the same row twice and could silently create money.
func TestSelfTransferRejected(t *testing.T) {
	s := newStore(t)
	w := freshWallet(t, s, 1_000)

	_, err := s.Transfer(context.Background(), store.TransferRequest{
		RequesterUserID: w.UserID,
		FromWalletID:    w.ID,
		ToWalletID:      w.ID,
		AmountPaise:     100,
		IdempotencyKey:  fmt.Sprintf("self_%d", time.Now().UnixNano()),
	})
	if err != store.ErrSameWallet {
		t.Fatalf("expected ErrSameWallet, got %v", err)
	}
	if got := balance(t, s, w.ID); got != 1_000 {
		t.Fatalf("self-transfer changed the balance to %d", got)
	}
}
