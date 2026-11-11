// Package httpapi exposes the ledger over a JSON REST API.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

const (
	headerIdempotencyKey     = "Idempotency-Key"
	headerIdempotentReplayed = "Idempotent-Replayed"
	readinessTimeout         = 2 * time.Second
)

// Ledger is the subset of the use cases the HTTP API depends on. Every call
// runs on behalf of the authenticated caller, which the use cases authorize.
type Ledger interface {
	CreateAccount(ctx context.Context, caller usecase.Principal, in usecase.CreateAccountInput) (domain.Account, bool, error)
	GetAccount(ctx context.Context, caller usecase.Principal, id uuid.UUID) (domain.Account, error)
	Deposit(ctx context.Context, caller usecase.Principal, in usecase.DepositInput) (domain.Transaction, bool, error)
	Withdraw(ctx context.Context, caller usecase.Principal, in usecase.WithdrawInput) (domain.Transaction, bool, error)
	Transfer(ctx context.Context, caller usecase.Principal, in usecase.TransferInput) (domain.Transaction, bool, error)
	ListTransactions(ctx context.Context, caller usecase.Principal, in usecase.ListTransactionsInput) (usecase.TransactionPage, error)
}

// ReadinessCheck reports whether the service can serve traffic.
type ReadinessCheck func(ctx context.Context) error

type handler struct {
	ledger Ledger
	authn  Authenticator
	ready  ReadinessCheck
	log    *slog.Logger
}

// NewHandler returns the HTTP handler with all routes and middleware.
func NewHandler(ledger Ledger, authn Authenticator, ready ReadinessCheck, log *slog.Logger) http.Handler {
	h := &handler{ledger: ledger, authn: authn, ready: ready, log: log}

	api := http.NewServeMux()
	api.HandleFunc("POST /v1/accounts", withCaller(h.createAccount))
	api.HandleFunc("GET /v1/accounts/{id}", withCaller(h.getAccount))
	api.HandleFunc("POST /v1/accounts/{id}/deposits", withCaller(h.deposit))
	api.HandleFunc("POST /v1/accounts/{id}/withdrawals", withCaller(h.withdraw))
	api.HandleFunc("GET /v1/accounts/{id}/transactions", withCaller(h.listTransactions))
	api.HandleFunc("POST /v1/transfers", withCaller(h.transfer))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	// Everything under /v1/, including routes added later, requires a valid
	// API key.
	mux.Handle("/v1/", h.authenticate(api))

	return withRequestID(withAccessLog(log, withRecovery(log, mux)))
}

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	if err := h.ready(ctx); err != nil {
		h.log.WarnContext(ctx, "readiness check failed", "error", err, "request_id", requestIDFrom(ctx))
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}

func (h *handler) createAccount(w http.ResponseWriter, r *http.Request, caller usecase.Principal) {
	var req createAccountRequest
	if !h.decode(w, r, &req) {
		return
	}
	acc, replayed, err := h.ledger.CreateAccount(r.Context(), caller, usecase.CreateAccountInput{
		IdempotencyKey: r.Header.Get(headerIdempotencyKey),
		Name:           req.Name,
		Currency:       req.Currency,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/accounts/"+acc.ID.String())
	writeCreated(w, replayed, toAccountResponse(acc))
}

func (h *handler) getAccount(w http.ResponseWriter, r *http.Request, caller usecase.Principal) {
	id, ok := h.accountID(w, r)
	if !ok {
		return
	}
	acc, err := h.ledger.GetAccount(r.Context(), caller, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountResponse(acc))
}

// deposit and withdraw are operator-only; the use cases enforce that.
func (h *handler) deposit(w http.ResponseWriter, r *http.Request, caller usecase.Principal) {
	id, ok := h.accountID(w, r)
	if !ok {
		return
	}
	var req amountRequest
	if !h.decode(w, r, &req) {
		return
	}
	tx, replayed, err := h.ledger.Deposit(r.Context(), caller, usecase.DepositInput{
		IdempotencyKey: r.Header.Get(headerIdempotencyKey),
		AccountID:      id,
		Amount:         req.Amount,
	})
	h.writePosting(w, r, tx, replayed, err, showAllBalances)
}

func (h *handler) withdraw(w http.ResponseWriter, r *http.Request, caller usecase.Principal) {
	id, ok := h.accountID(w, r)
	if !ok {
		return
	}
	var req amountRequest
	if !h.decode(w, r, &req) {
		return
	}
	tx, replayed, err := h.ledger.Withdraw(r.Context(), caller, usecase.WithdrawInput{
		IdempotencyKey: r.Header.Get(headerIdempotencyKey),
		AccountID:      id,
		Amount:         req.Amount,
	})
	h.writePosting(w, r, tx, replayed, err, showAllBalances)
}

func (h *handler) transfer(w http.ResponseWriter, r *http.Request, caller usecase.Principal) {
	var req transferRequest
	if !h.decode(w, r, &req) {
		return
	}
	tx, replayed, err := h.ledger.Transfer(r.Context(), caller, usecase.TransferInput{
		IdempotencyKey: r.Header.Get(headerIdempotencyKey),
		FromAccountID:  req.FromAccountID,
		ToAccountID:    req.ToAccountID,
		Amount:         req.Amount,
	})
	// The payer owns the source account but not necessarily the destination,
	// so the destination's balance is left out.
	h.writePosting(w, r, tx, replayed, err, func(id uuid.UUID) bool { return id == req.FromAccountID })
}

func (h *handler) listTransactions(w http.ResponseWriter, r *http.Request, caller usecase.Principal) {
	id, ok := h.accountID(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()

	limit := usecase.DefaultPageSize
	if s := query.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > usecase.MaxPageSize {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be an integer between 1 and "+strconv.Itoa(usecase.MaxPageSize))
			return
		}
		limit = n
	}
	cursor, err := decodeCursor(query.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid cursor")
		return
	}

	page, err := h.ledger.ListTransactions(r.Context(), caller, usecase.ListTransactionsInput{
		AccountID: id,
		Cursor:    cursor,
		Limit:     limit,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toListTransactionsResponse(page))
}

func (h *handler) writePosting(w http.ResponseWriter, r *http.Request, tx domain.Transaction, replayed bool, err error, showBalance func(accountID uuid.UUID) bool) {
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeCreated(w, replayed, toTransactionResponse(tx, showBalance))
}

func showAllBalances(uuid.UUID) bool { return true }

func (h *handler) accountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "account id must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func writeCreated(w http.ResponseWriter, replayed bool, body any) {
	if replayed {
		w.Header().Set(headerIdempotentReplayed, "true")
	}
	writeJSON(w, http.StatusCreated, body)
}
