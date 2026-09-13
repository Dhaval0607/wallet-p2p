package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Dhaval0607/wallet-p2p/internal/obs"
	"github.com/Dhaval0607/wallet-p2p/internal/store"
)

// maxBodyBytes caps request bodies. These are tiny JSON documents; anything
// larger is a mistake or an attack.
const maxBodyBytes = 8 << 10

type errorBody struct {
	Error struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		CorrelationID string `json:"correlation_id"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = msg
	b.Error.CorrelationID = obs.CorrelationID(r.Context())
	writeJSON(w, status, b)
}

// decodeJSON enforces strict decoding: unknown fields are rejected so that a
// typo'd `amount` never silently becomes a zero-paise transfer.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(struct{})); err != io.EOF {
		return errors.New("body must contain exactly one JSON object")
	}
	return nil
}

// ---------------------------------------------------------------------------
// POST /wallets  -- get-or-create the caller's wallet
// ---------------------------------------------------------------------------

func (s *Server) handleCreateWallet(w http.ResponseWriter, r *http.Request, user store.User) {
	wallet, created, err := s.store.GetOrCreateWallet(r.Context(), user.ID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "get-or-create wallet failed",
			slog.String("event", "wallet_error"), slog.String("error", err.Error()))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "could not provision wallet")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.metrics.WalletsCreated.Inc()
	} else {
		s.metrics.WalletsReused.Inc()
	}

	s.log.InfoContext(r.Context(), "wallet get-or-create",
		slog.String("event", "wallet_get_or_create"),
		slog.String("wallet_id", wallet.ID),
		slog.String("user_id", wallet.UserID),
		slog.Bool("created", created),
		slog.Int64("balance_paise", wallet.BalancePaise),
	)

	writeJSON(w, status, wallet)
}

// ---------------------------------------------------------------------------
// GET /wallets/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleGetWallet(w http.ResponseWriter, r *http.Request, user store.User) {
	id := r.PathValue("id")

	wallet, err := s.store.GetWallet(r.Context(), id)
	if errors.Is(err, store.ErrWalletNotFound) {
		writeError(w, r, http.StatusNotFound, "wallet_not_found", "no wallet with that id")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "get wallet failed", slog.String("error", err.Error()))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "could not read wallet")
		return
	}
	writeJSON(w, http.StatusOK, wallet)
}

// ---------------------------------------------------------------------------
// POST /transfers
// ---------------------------------------------------------------------------

type transferRequestBody struct {
	From string `json:"from"`
	To   string `json:"to"`
	// AmountPaise is a *pointer so that a missing field is distinguishable from
	// an explicit 0, and it is an int64 so money never touches a float. A JSON
	// number with a decimal point fails to decode -- by design.
	AmountPaise    *int64 `json:"amount_paise"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Server) handleCreateTransfer(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()

	var body transferRequestBody
	if err := decodeJSON(r, &body); err != nil {
		s.metrics.Transfers.WithLabelValues(obs.OutcomeInvalid).Inc()
		writeError(w, r, http.StatusBadRequest, "invalid_body", "could not parse request: "+err.Error())
		return
	}

	// --- Validation happens BEFORE the idempotency key is claimed. A request
	// that was never valid should not burn the caller's key: they must be able
	// to fix the typo and retry with the same key. ---
	switch {
	case strings.TrimSpace(body.IdempotencyKey) == "":
		s.rejectTransfer(w, r, "missing_idempotency_key", "idempotency_key is required")
		return
	case len(body.IdempotencyKey) > 255:
		s.rejectTransfer(w, r, "invalid_idempotency_key", "idempotency_key must be at most 255 characters")
		return
	case body.From == "" || body.To == "":
		s.rejectTransfer(w, r, "missing_wallet", "both from and to are required")
		return
	case body.From == body.To:
		s.rejectTransfer(w, r, "same_wallet", "from and to must be different wallets")
		return
	case body.AmountPaise == nil:
		s.rejectTransfer(w, r, "missing_amount", "amount_paise is required (integer paise)")
		return
	case *body.AmountPaise <= 0:
		s.rejectTransfer(w, r, "invalid_amount", "amount_paise must be a positive integer")
		return
	}

	// Ownership: you may only move money out of a wallet you own. Checked before
	// the transaction so an unauthorized caller cannot consume an idempotency key.
	fromWallet, err := s.store.GetWallet(ctx, body.From)
	if errors.Is(err, store.ErrWalletNotFound) {
		s.rejectTransferStatus(w, r, http.StatusNotFound, "wallet_not_found", "source wallet does not exist")
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "transfer precheck failed", slog.String("error", err.Error()))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "could not read source wallet")
		return
	}
	if fromWallet.UserID != user.ID {
		s.rejectTransferStatus(w, r, http.StatusForbidden, "not_wallet_owner",
			"bearer token does not own the source wallet")
		return
	}
	if _, err := s.store.GetWallet(ctx, body.To); err != nil {
		if errors.Is(err, store.ErrWalletNotFound) {
			s.rejectTransferStatus(w, r, http.StatusNotFound, "wallet_not_found", "destination wallet does not exist")
			return
		}
		s.log.ErrorContext(ctx, "transfer precheck failed", slog.String("error", err.Error()))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "could not read destination wallet")
		return
	}

	s.log.InfoContext(ctx, "transfer requested",
		slog.String("event", "transfer_created"),
		slog.String("from", body.From),
		slog.String("to", body.To),
		slog.Int64("amount_paise", *body.AmountPaise),
		slog.String("idempotency_key", body.IdempotencyKey),
	)

	res, err := s.store.Transfer(ctx, store.TransferRequest{
		RequesterUserID: user.ID,
		FromWalletID:    body.From,
		ToWalletID:      body.To,
		AmountPaise:     *body.AmountPaise,
		IdempotencyKey:  body.IdempotencyKey,
	})

	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		s.metrics.Transfers.WithLabelValues(obs.OutcomeConflict).Inc()
		s.log.WarnContext(ctx, "idempotency key reused with a different body",
			slog.String("event", "idempotency_conflict"),
			slog.String("idempotency_key", body.IdempotencyKey),
		)
		writeError(w, r, http.StatusConflict, "idempotency_key_conflict",
			"this idempotency_key was already used with different transfer parameters")
		return

	case errors.Is(err, store.ErrWalletNotFound):
		s.rejectTransferStatus(w, r, http.StatusNotFound, "wallet_not_found", "wallet does not exist")
		return

	case errors.Is(err, store.ErrSameWallet):
		s.rejectTransfer(w, r, "same_wallet", "from and to must be different wallets")
		return

	case err != nil:
		s.log.ErrorContext(ctx, "transfer failed",
			slog.String("event", "transfer_error"),
			slog.String("idempotency_key", body.IdempotencyKey),
			slog.String("error", err.Error()),
		)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "transfer could not be processed")
		return
	}

	t := res.Transfer

	// One log line per meaningful domain event, all sharing the correlation id.
	switch {
	case res.Replay:
		s.metrics.Transfers.WithLabelValues(obs.OutcomeReplay).Inc()
		s.log.InfoContext(ctx, "idempotent replay hit",
			slog.String("event", "idempotent_replay"),
			slog.String("transfer_id", t.ID),
			slog.String("idempotency_key", t.IdempotencyKey),
			slog.String("replayed_status", t.Status),
		)
	case t.Status == store.StatusSucceeded:
		s.metrics.Transfers.WithLabelValues(obs.OutcomeSucceeded).Inc()
		s.metrics.TransferAmount.Add(float64(t.AmountPaise))
		s.log.InfoContext(ctx, "transfer debited and credited",
			slog.String("event", "transfer_succeeded"),
			slog.String("transfer_id", t.ID),
			slog.String("debited_wallet", t.FromWalletID),
			slog.String("credited_wallet", t.ToWalletID),
			slog.Int64("amount_paise", t.AmountPaise),
		)
	default:
		s.metrics.Transfers.WithLabelValues(obs.OutcomeInsufficient).Inc()
		reason := ""
		if t.DeclineReason != nil {
			reason = *t.DeclineReason
		}
		s.log.WarnContext(ctx, "transfer declined",
			slog.String("event", "transfer_declined"),
			slog.String("transfer_id", t.ID),
			slog.String("from", t.FromWalletID),
			slog.Int64("amount_paise", t.AmountPaise),
			slog.String("reason", reason),
		)
	}

	if res.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}

	// A declined transfer is a successfully-recorded business outcome, not an
	// HTTP failure: 201 with status="declined". The retry of a decline must
	// return the identical document, which it cannot do if we 4xx here.
	writeJSON(w, http.StatusCreated, t)
}

// rejectTransfer reports a client-side validation failure. These never reach the
// database and never consume an idempotency key.
func (s *Server) rejectTransfer(w http.ResponseWriter, r *http.Request, code, msg string) {
	s.rejectTransferStatus(w, r, http.StatusUnprocessableEntity, code, msg)
}

func (s *Server) rejectTransferStatus(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	s.metrics.Transfers.WithLabelValues(obs.OutcomeInvalid).Inc()
	s.log.WarnContext(r.Context(), "transfer rejected",
		slog.String("event", "transfer_rejected"),
		slog.String("reason", code),
		slog.Int("status", status),
	)
	writeError(w, r, status, code, msg)
}

// ---------------------------------------------------------------------------
// GET /transfers/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleGetTransfer(w http.ResponseWriter, r *http.Request, user store.User) {
	t, err := s.store.GetTransfer(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrTransferNotFound) {
		writeError(w, r, http.StatusNotFound, "transfer_not_found", "no transfer with that id")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "get transfer failed", slog.String("error", err.Error()))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "could not read transfer")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// ---------------------------------------------------------------------------
// POST /admin/mint -- test funding. Explicitly NOT a transfer.
// ---------------------------------------------------------------------------

type mintRequestBody struct {
	WalletID       string `json:"wallet_id"`
	AmountPaise    *int64 `json:"amount_paise"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Server) handleMint(w http.ResponseWriter, r *http.Request) {
	var body mintRequestBody
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.WalletID == "" || body.AmountPaise == nil || *body.AmountPaise <= 0 {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_mint",
			"wallet_id and a positive integer amount_paise are required")
		return
	}
	if strings.TrimSpace(body.IdempotencyKey) == "" {
		writeError(w, r, http.StatusUnprocessableEntity, "missing_idempotency_key",
			"idempotency_key is required so a retried funding step cannot double the float")
		return
	}

	wallet, applied, err := s.store.Mint(r.Context(), body.WalletID, body.IdempotencyKey, *body.AmountPaise)
	if errors.Is(err, store.ErrWalletNotFound) {
		writeError(w, r, http.StatusNotFound, "wallet_not_found", "no wallet with that id")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "mint failed", slog.String("error", err.Error()))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "could not mint")
		return
	}
	if applied {
		s.metrics.MintedPaise.Add(float64(*body.AmountPaise))
	}

	s.log.InfoContext(r.Context(), "mint",
		slog.String("event", "mint"),
		slog.String("wallet_id", wallet.ID),
		slog.Int64("amount_paise", *body.AmountPaise),
		slog.Bool("applied", applied),
		slog.Int64("balance_paise", wallet.BalancePaise),
	)

	if !applied {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, wallet)
}

// ---------------------------------------------------------------------------
// GET /invariants -- live, public, recomputed from base tables
// ---------------------------------------------------------------------------

func (s *Server) handleInvariants(w http.ResponseWriter, r *http.Request) {
	inv, err := s.store.CheckInvariants(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "could not compute invariants")
		return
	}
	s.metrics.TotalBalance.Set(float64(inv.TotalBalancePaise))
	s.metrics.LedgerSum.Set(float64(inv.LedgerSumPaise))

	// Fail loudly: if an invariant is broken, the endpoint that reports on it
	// should not answer 200 OK.
	status := http.StatusOK
	if !inv.AllHold {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, inv)
}
