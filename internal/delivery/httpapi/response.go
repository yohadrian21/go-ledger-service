package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

const maxBodyBytes = 1 << 20

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorMappings translates use case errors into HTTP responses. Anything not
// listed is an internal error.
var errorMappings = []struct {
	err    error
	status int
	code   string
}{
	{domain.ErrAccountNotFound, http.StatusNotFound, "account_not_found"},
	{domain.ErrInsufficientFunds, http.StatusUnprocessableEntity, "insufficient_funds"},
	{domain.ErrCurrencyMismatch, http.StatusUnprocessableEntity, "currency_mismatch"},
	{domain.ErrBalanceOverflow, http.StatusUnprocessableEntity, "balance_overflow"},
	{domain.ErrSameAccount, http.StatusBadRequest, "same_account"},
	{domain.ErrInvalidAmount, http.StatusBadRequest, "invalid_amount"},
	{domain.ErrInvalidCurrency, http.StatusBadRequest, "invalid_currency"},
	{domain.ErrInvalidAccountName, http.StatusBadRequest, "invalid_name"},
	{usecase.ErrIdempotencyKeyMissing, http.StatusBadRequest, "idempotency_key_missing"},
	{usecase.ErrIdempotencyKeyInvalid, http.StatusBadRequest, "idempotency_key_invalid"},
	{usecase.ErrIdempotencyKeyReused, http.StatusUnprocessableEntity, "idempotency_key_reused"},
}

func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	for _, m := range errorMappings {
		if errors.Is(err, m.err) {
			writeError(w, m.status, m.code, m.err.Error())
			return
		}
	}
	h.log.ErrorContext(r.Context(), "request failed", "error", err, "request_id", requestIDFrom(r.Context()))
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

// decode reads a single JSON object into dst and rejects unknown fields. It
// writes an error response and returns false if the body is invalid.
func (h *handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil && !errors.Is(dec.Decode(&struct{}{}), io.EOF) {
		err = errors.New("body must contain a single JSON object")
	}
	if err == nil {
		return true
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
	return false
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
