package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/delivery/httpapi"
	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/repository/memory"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

type account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Balance  int64  `json:"balance"`
}

type transaction struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Amount  int64  `json:"amount"`
	Entries []struct {
		AccountID    string `json:"account_id"`
		Amount       int64  `json:"amount"`
		BalanceAfter int64  `json:"balance_after"`
	} `json:"entries"`
}

type statement struct {
	Data []struct {
		TransactionID string `json:"transaction_id"`
		Type          string `json:"type"`
		Amount        int64  `json:"amount"`
		BalanceAfter  int64  `json:"balance_after"`
	} `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newClient(t *testing.T, ready httpapi.ReadinessCheck) *client {
	t.Helper()
	if ready == nil {
		ready = func(context.Context) error { return nil }
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpapi.NewHandler(usecase.NewLedger(memory.New()), ready, log))
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

type response struct {
	method, path string
	StatusCode   int
	Header       http.Header
}

// do sends a request and decodes the JSON response into out (if not nil).
func (c *client) do(method, path, idempotencyKey, body string, out any) response {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return response{method: method, path: path, StatusCode: resp.StatusCode, Header: resp.Header}
}

func (c *client) createAccount(currency string) account {
	c.t.Helper()
	var acc account
	resp := c.do(http.MethodPost, "/v1/accounts", uuid.NewString(), `{"name":"Alice","currency":"`+currency+`"}`, &acc)
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("create account: status %d", resp.StatusCode)
	}
	return acc
}

func (c *client) deposit(id string, amount string) {
	c.t.Helper()
	resp := c.do(http.MethodPost, "/v1/accounts/"+id+"/deposits", uuid.NewString(), `{"amount":`+amount+`}`, nil)
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("deposit: status %d", resp.StatusCode)
	}
}

func expectStatus(t *testing.T, resp response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d", resp.method, resp.path, resp.StatusCode, want)
	}
}

func TestAccountLifecycle(t *testing.T) {
	c := newClient(t, nil)

	var created account
	resp := c.do(http.MethodPost, "/v1/accounts", "acc-1", `{"name":"Alice","currency":"usd"}`, &created)
	expectStatus(t, resp, http.StatusCreated)
	if created.Currency != "USD" || created.Balance != 0 || created.Name != "Alice" {
		t.Fatalf("unexpected account: %+v", created)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/accounts/"+created.ID {
		t.Fatalf("Location = %q", loc)
	}
	bob := c.createAccount("USD")

	var dep transaction
	resp = c.do(http.MethodPost, "/v1/accounts/"+created.ID+"/deposits", "dep-1", `{"amount":10000}`, &dep)
	expectStatus(t, resp, http.StatusCreated)
	if dep.Type != "deposit" || dep.Amount != 10000 || len(dep.Entries) != 2 {
		t.Fatalf("unexpected deposit: %+v", dep)
	}

	var tr transaction
	resp = c.do(http.MethodPost, "/v1/transfers", "tr-1",
		`{"from_account_id":"`+created.ID+`","to_account_id":"`+bob.ID+`","amount":2500}`, &tr)
	expectStatus(t, resp, http.StatusCreated)
	if tr.Entries[0].AccountID != created.ID || tr.Entries[0].Amount != -2500 || tr.Entries[0].BalanceAfter != 7500 {
		t.Fatalf("unexpected debit entry: %+v", tr.Entries[0])
	}

	resp = c.do(http.MethodPost, "/v1/accounts/"+bob.ID+"/withdrawals", "wd-1", `{"amount":500}`, nil)
	expectStatus(t, resp, http.StatusCreated)

	var got account
	expectStatus(t, c.do(http.MethodGet, "/v1/accounts/"+created.ID, "", "", &got), http.StatusOK)
	if got.Balance != 7500 {
		t.Fatalf("alice balance = %d, want 7500", got.Balance)
	}
	expectStatus(t, c.do(http.MethodGet, "/v1/accounts/"+bob.ID, "", "", &got), http.StatusOK)
	if got.Balance != 2000 {
		t.Fatalf("bob balance = %d, want 2000", got.Balance)
	}
}

func TestListTransactionsPagination(t *testing.T) {
	c := newClient(t, nil)
	acc := c.createAccount("EUR")
	for _, amount := range []string{"100", "200", "300"} {
		c.deposit(acc.ID, amount)
	}

	var page statement
	expectStatus(t, c.do(http.MethodGet, "/v1/accounts/"+acc.ID+"/transactions?limit=2", "", "", &page), http.StatusOK)
	if len(page.Data) != 2 || page.Data[0].Amount != 300 || page.Data[0].BalanceAfter != 600 || page.NextCursor == nil {
		t.Fatalf("unexpected first page: %+v", page)
	}

	var last statement
	expectStatus(t, c.do(http.MethodGet, "/v1/accounts/"+acc.ID+"/transactions?limit=2&cursor="+*page.NextCursor, "", "", &last), http.StatusOK)
	if len(last.Data) != 1 || last.Data[0].Amount != 100 || last.NextCursor != nil {
		t.Fatalf("unexpected last page: %+v", last)
	}

	other := c.createAccount("EUR")
	var empty map[string]json.RawMessage
	expectStatus(t, c.do(http.MethodGet, "/v1/accounts/"+other.ID+"/transactions", "", "", &empty), http.StatusOK)
	if string(empty["data"]) != "[]" || string(empty["next_cursor"]) != "null" {
		t.Fatalf("empty statement = %v", empty)
	}
}

func TestIdempotentReplay(t *testing.T) {
	c := newClient(t, nil)
	acc := c.createAccount("USD")
	path := "/v1/accounts/" + acc.ID + "/deposits"

	var first, second transaction
	resp := c.do(http.MethodPost, path, "same-key", `{"amount":500}`, &first)
	expectStatus(t, resp, http.StatusCreated)
	if resp.Header.Get("Idempotent-Replayed") != "" {
		t.Fatal("first request must not be marked as replayed")
	}

	resp = c.do(http.MethodPost, path, "same-key", `{"amount":500}`, &second)
	expectStatus(t, resp, http.StatusCreated)
	if resp.Header.Get("Idempotent-Replayed") != "true" || second.ID != first.ID {
		t.Fatalf("retry: replayed=%q id=%s, want replay of %s", resp.Header.Get("Idempotent-Replayed"), second.ID, first.ID)
	}

	var got account
	c.do(http.MethodGet, "/v1/accounts/"+acc.ID, "", "", &got)
	if got.Balance != 500 {
		t.Fatalf("balance = %d, want 500", got.Balance)
	}

	var apiErr apiError
	resp = c.do(http.MethodPost, path, "same-key", `{"amount":501}`, &apiErr)
	expectStatus(t, resp, http.StatusUnprocessableEntity)
	if apiErr.Error.Code != "idempotency_key_reused" {
		t.Fatalf("code = %q", apiErr.Error.Code)
	}
}

func TestErrorResponses(t *testing.T) {
	c := newClient(t, nil)
	usd := c.createAccount("USD")
	eur := c.createAccount("EUR")
	c.deposit(usd.ID, "100")
	unknown := uuid.NewString()

	tests := []struct {
		name, method, path, key, body string
		status                        int
		code                          string
	}{
		{"missing idempotency key", "POST", "/v1/accounts", "", `{"name":"A","currency":"USD"}`, 400, "idempotency_key_missing"},
		{"idempotency key too long", "POST", "/v1/accounts", strings.Repeat("k", 256), `{"name":"A","currency":"USD"}`, 400, "idempotency_key_invalid"},
		{"invalid currency", "POST", "/v1/accounts", "k1", `{"name":"A","currency":"dollar"}`, 400, "invalid_currency"},
		{"blank name", "POST", "/v1/accounts", "k2", `{"name":"","currency":"USD"}`, 400, "invalid_name"},
		{"malformed json", "POST", "/v1/accounts", "k3", `{"name":`, 400, "invalid_request"},
		{"unknown field", "POST", "/v1/accounts", "k4", `{"name":"A","currency":"USD","admin":true}`, 400, "invalid_request"},
		{"trailing data", "POST", "/v1/accounts", "k5", `{"name":"A","currency":"USD"} {}`, 400, "invalid_request"},
		{"fractional amount", "POST", "/v1/accounts/" + usd.ID + "/deposits", "k6", `{"amount":10.5}`, 400, "invalid_request"},
		{"zero amount", "POST", "/v1/accounts/" + usd.ID + "/deposits", "k7", `{"amount":0}`, 400, "invalid_amount"},
		{"invalid account id", "GET", "/v1/accounts/not-a-uuid", "", "", 400, "invalid_request"},
		{"unknown account", "GET", "/v1/accounts/" + unknown, "", "", 404, "account_not_found"},
		{"deposit to unknown account", "POST", "/v1/accounts/" + unknown + "/deposits", "k8", `{"amount":1}`, 404, "account_not_found"},
		{"insufficient funds", "POST", "/v1/accounts/" + usd.ID + "/withdrawals", "k9", `{"amount":101}`, 422, "insufficient_funds"},
		{"currency mismatch", "POST", "/v1/transfers", "k10", `{"from_account_id":"` + usd.ID + `","to_account_id":"` + eur.ID + `","amount":1}`, 422, "currency_mismatch"},
		{"same account", "POST", "/v1/transfers", "k11", `{"from_account_id":"` + usd.ID + `","to_account_id":"` + usd.ID + `","amount":1}`, 400, "same_account"},
		{"invalid transfer account id", "POST", "/v1/transfers", "k12", `{"from_account_id":"x","to_account_id":"` + usd.ID + `","amount":1}`, 400, "invalid_request"},
		{"invalid limit", "GET", "/v1/accounts/" + usd.ID + "/transactions?limit=1000", "", "", 400, "invalid_request"},
		{"invalid cursor", "GET", "/v1/accounts/" + usd.ID + "/transactions?cursor=bm9wZQ", "", "", 400, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiErr apiError
			resp := c.do(tt.method, tt.path, tt.key, tt.body, &apiErr)
			expectStatus(t, resp, tt.status)
			if apiErr.Error.Code != tt.code || apiErr.Error.Message == "" {
				t.Fatalf("error = %+v, want code %q", apiErr.Error, tt.code)
			}
		})
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	c := newClient(t, nil)
	body := `{"name":"` + strings.Repeat("a", 2<<20) + `","currency":"USD"}`
	var apiErr apiError
	expectStatus(t, c.do(http.MethodPost, "/v1/accounts", "big", body, &apiErr), http.StatusRequestEntityTooLarge)
}

func TestHealthAndReadiness(t *testing.T) {
	healthy := newClient(t, nil)
	expectStatus(t, healthy.do(http.MethodGet, "/healthz", "", "", nil), http.StatusOK)
	expectStatus(t, healthy.do(http.MethodGet, "/readyz", "", "", nil), http.StatusOK)

	unready := newClient(t, func(context.Context) error { return errors.New("database unreachable") })
	expectStatus(t, unready.do(http.MethodGet, "/healthz", "", "", nil), http.StatusOK)
	var body map[string]string
	expectStatus(t, unready.do(http.MethodGet, "/readyz", "", "", &body), http.StatusServiceUnavailable)
	if body["status"] != "unavailable" {
		t.Fatalf("readyz body = %v", body)
	}
}

func TestRequestIDHeader(t *testing.T) {
	c := newClient(t, nil)
	resp := c.do(http.MethodGet, "/healthz", "", "", nil)
	if _, err := uuid.Parse(resp.Header.Get("X-Request-ID")); err != nil {
		t.Fatalf("generated request id %q is not a UUID", resp.Header.Get("X-Request-ID"))
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpapi.NewHandler(usecase.NewLedger(memory.New()), nil, log).ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "abc-123" {
		t.Fatalf("X-Request-ID = %q, want the caller's id", got)
	}
}

type panickingLedger struct{ httpapi.Ledger }

func (panickingLedger) GetAccount(context.Context, uuid.UUID) (domain.Account, error) {
	panic("boom")
}

func TestPanicRecovery(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := httpapi.NewHandler(panickingLedger{}, nil, log)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/accounts/"+uuid.NewString(), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "panic while serving request") {
		t.Fatalf("panic was not logged: %s", logs.String())
	}
}
