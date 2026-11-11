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

	"github.com/yohadrian21/go-ledger-service/internal/auth"
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
		BalanceAfter *int64 `json:"balance_after"`
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

// testAPI is a server with an operator, two ordinary clients and a caller
// without credentials.
type testAPI struct {
	operator, alice, bob, anonymous *client
}

type apiOptions struct {
	ledger httpapi.Ledger
	ready  httpapi.ReadinessCheck
	log    *slog.Logger
	limit  httpapi.RateLimit
}

func newAPI(t *testing.T, opts apiOptions) *testAPI {
	t.Helper()
	if opts.ledger == nil {
		opts.ledger = usecase.NewLedger(memory.New())
	}
	if opts.ready == nil {
		opts.ready = func(context.Context) error { return nil }
	}
	if opts.log == nil {
		opts.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	keys := map[string]string{}
	var entries []string
	for clientID, role := range map[string]usecase.Role{"ops": usecase.RoleOperator, "alice": usecase.RoleClient, "bob": usecase.RoleClient} {
		key, err := auth.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		entry, err := auth.Entry(clientID, role, key)
		if err != nil {
			t.Fatal(err)
		}
		keys[clientID] = key
		entries = append(entries, entry)
	}
	authn, err := auth.ParseAPIKeys(strings.Join(entries, ","))
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(httpapi.NewHandler(opts.ledger, authn, opts.limit, opts.ready, opts.log))
	t.Cleanup(srv.Close)
	as := func(authorization string) *client { return &client{t: t, srv: srv, authorization: authorization} }
	return &testAPI{
		operator:  as("Bearer " + keys["ops"]),
		alice:     as("Bearer " + keys["alice"]),
		bob:       as("Bearer " + keys["bob"]),
		anonymous: as(""),
	}
}

type client struct {
	t             *testing.T
	srv           *httptest.Server
	authorization string // Authorization header value; empty sends none
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
	if c.authorization != "" {
		req.Header.Set("Authorization", c.authorization)
	}
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

func (c *client) balance(id string) int64 {
	c.t.Helper()
	var acc account
	expectStatus(c.t, c.do(http.MethodGet, "/v1/accounts/"+id, "", "", &acc), http.StatusOK)
	return acc.Balance
}

func expectStatus(t *testing.T, resp response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d", resp.method, resp.path, resp.StatusCode, want)
	}
}

func expectError(t *testing.T, resp response, got apiError, status int, code string) {
	t.Helper()
	expectStatus(t, resp, status)
	if got.Error.Code != code || got.Error.Message == "" {
		t.Fatalf("%s %s: error = %+v, want code %q", resp.method, resp.path, got.Error, code)
	}
}

func TestAccountLifecycle(t *testing.T) {
	api := newAPI(t, apiOptions{})

	var created account
	resp := api.alice.do(http.MethodPost, "/v1/accounts", "acc-1", `{"name":"Alice","currency":"usd"}`, &created)
	expectStatus(t, resp, http.StatusCreated)
	if created.Currency != "USD" || created.Balance != 0 || created.Name != "Alice" {
		t.Fatalf("unexpected account: %+v", created)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/accounts/"+created.ID {
		t.Fatalf("Location = %q", loc)
	}
	bob := api.bob.createAccount("USD")

	var dep transaction
	resp = api.operator.do(http.MethodPost, "/v1/accounts/"+created.ID+"/deposits", "dep-1", `{"amount":10000}`, &dep)
	expectStatus(t, resp, http.StatusCreated)
	if dep.Type != "deposit" || dep.Amount != 10000 || len(dep.Entries) != 2 || dep.Entries[1].BalanceAfter == nil {
		t.Fatalf("unexpected deposit: %+v", dep)
	}

	var tr transaction
	resp = api.alice.do(http.MethodPost, "/v1/transfers", "tr-1",
		`{"from_account_id":"`+created.ID+`","to_account_id":"`+bob.ID+`","amount":2500}`, &tr)
	expectStatus(t, resp, http.StatusCreated)
	debit, credit := tr.Entries[0], tr.Entries[1]
	if debit.AccountID != created.ID || debit.Amount != -2500 || debit.BalanceAfter == nil || *debit.BalanceAfter != 7500 {
		t.Fatalf("unexpected debit entry: %+v", debit)
	}
	if credit.AccountID != bob.ID || credit.Amount != 2500 || credit.BalanceAfter != nil {
		t.Fatalf("credit entry must not reveal the payee's balance: %+v", credit)
	}

	resp = api.operator.do(http.MethodPost, "/v1/accounts/"+bob.ID+"/withdrawals", "wd-1", `{"amount":500}`, nil)
	expectStatus(t, resp, http.StatusCreated)

	if got := api.alice.balance(created.ID); got != 7500 {
		t.Fatalf("alice balance = %d, want 7500", got)
	}
	if got := api.bob.balance(bob.ID); got != 2000 {
		t.Fatalf("bob balance = %d, want 2000", got)
	}
}

func TestListTransactionsPagination(t *testing.T) {
	api := newAPI(t, apiOptions{})
	acc := api.alice.createAccount("EUR")
	for _, amount := range []string{"100", "200", "300"} {
		api.operator.deposit(acc.ID, amount)
	}

	var page statement
	expectStatus(t, api.alice.do(http.MethodGet, "/v1/accounts/"+acc.ID+"/transactions?limit=2", "", "", &page), http.StatusOK)
	if len(page.Data) != 2 || page.Data[0].Amount != 300 || page.Data[0].BalanceAfter != 600 || page.NextCursor == nil {
		t.Fatalf("unexpected first page: %+v", page)
	}

	var last statement
	expectStatus(t, api.alice.do(http.MethodGet, "/v1/accounts/"+acc.ID+"/transactions?limit=2&cursor="+*page.NextCursor, "", "", &last), http.StatusOK)
	if len(last.Data) != 1 || last.Data[0].Amount != 100 || last.NextCursor != nil {
		t.Fatalf("unexpected last page: %+v", last)
	}

	other := api.alice.createAccount("EUR")
	var empty map[string]json.RawMessage
	expectStatus(t, api.alice.do(http.MethodGet, "/v1/accounts/"+other.ID+"/transactions", "", "", &empty), http.StatusOK)
	if string(empty["data"]) != "[]" || string(empty["next_cursor"]) != "null" {
		t.Fatalf("empty statement = %v", empty)
	}
}

func TestIdempotentReplay(t *testing.T) {
	api := newAPI(t, apiOptions{})
	acc := api.alice.createAccount("USD")
	path := "/v1/accounts/" + acc.ID + "/deposits"

	var first, second transaction
	resp := api.operator.do(http.MethodPost, path, "same-key", `{"amount":500}`, &first)
	expectStatus(t, resp, http.StatusCreated)
	if resp.Header.Get("Idempotent-Replayed") != "" {
		t.Fatal("first request must not be marked as replayed")
	}

	resp = api.operator.do(http.MethodPost, path, "same-key", `{"amount":500}`, &second)
	expectStatus(t, resp, http.StatusCreated)
	if resp.Header.Get("Idempotent-Replayed") != "true" || second.ID != first.ID {
		t.Fatalf("retry: replayed=%q id=%s, want replay of %s", resp.Header.Get("Idempotent-Replayed"), second.ID, first.ID)
	}

	if got := api.alice.balance(acc.ID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}

	var apiErr apiError
	resp = api.operator.do(http.MethodPost, path, "same-key", `{"amount":501}`, &apiErr)
	expectError(t, resp, apiErr, http.StatusUnprocessableEntity, "idempotency_key_reused")
}

func TestIdempotencyKeysAreScopedPerClient(t *testing.T) {
	api := newAPI(t, apiOptions{})
	body := `{"name":"Shared","currency":"USD"}`

	var a, b account
	respA := api.alice.do(http.MethodPost, "/v1/accounts", "shared-key", body, &a)
	respB := api.bob.do(http.MethodPost, "/v1/accounts", "shared-key", body, &b)
	expectStatus(t, respA, http.StatusCreated)
	expectStatus(t, respB, http.StatusCreated)
	if a.ID == b.ID || respB.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("bob must get his own account, not a replay of alice's (alice=%s bob=%s)", a.ID, b.ID)
	}
	var apiErr apiError
	expectError(t, api.bob.do(http.MethodGet, "/v1/accounts/"+a.ID, "", "", &apiErr), apiErr, http.StatusNotFound, "account_not_found")
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	api := newAPI(t, apiOptions{})
	acc := api.alice.createAccount("USD")
	api.operator.deposit(acc.ID, "1000")
	other := api.bob.createAccount("USD")

	routes := []struct {
		method, path, body string
	}{
		{"POST", "/v1/accounts", `{"name":"A","currency":"USD"}`},
		{"GET", "/v1/accounts/" + acc.ID, ""},
		{"POST", "/v1/accounts/" + acc.ID + "/deposits", `{"amount":1}`},
		{"POST", "/v1/accounts/" + acc.ID + "/withdrawals", `{"amount":1}`},
		{"GET", "/v1/accounts/" + acc.ID + "/transactions", ""},
		{"POST", "/v1/transfers", `{"from_account_id":"` + acc.ID + `","to_account_id":"` + other.ID + `","amount":1}`},
		{"GET", "/v1/no-such-route", ""},
	}
	unknownKey, err := auth.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	callers := map[string]*client{
		"no credentials": api.anonymous,
		"unknown key":    {t: t, srv: api.alice.srv, authorization: "Bearer " + unknownKey},
		"wrong scheme":   {t: t, srv: api.alice.srv, authorization: strings.Replace(api.alice.authorization, "Bearer", "Basic", 1)},
		"empty bearer":   {t: t, srv: api.alice.srv, authorization: "Bearer "},
	}
	for name, c := range callers {
		for _, rt := range routes {
			t.Run(name+" "+rt.method+" "+rt.path, func(t *testing.T) {
				var apiErr apiError
				resp := c.do(rt.method, rt.path, uuid.NewString(), rt.body, &apiErr)
				expectError(t, resp, apiErr, http.StatusUnauthorized, "unauthorized")
				if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
					t.Fatalf("WWW-Authenticate = %q", resp.Header.Get("WWW-Authenticate"))
				}
			})
		}
	}
	if got := api.alice.balance(acc.ID); got != 1000 {
		t.Fatalf("balance = %d, want 1000: unauthenticated requests must not move money", got)
	}
	expectStatus(t, api.anonymous.do(http.MethodGet, "/healthz", "", "", nil), http.StatusOK)
	expectStatus(t, api.anonymous.do(http.MethodGet, "/readyz", "", "", nil), http.StatusOK)
}

func TestCannotAccessAnotherClientsAccount(t *testing.T) {
	api := newAPI(t, apiOptions{})
	victim := api.alice.createAccount("USD")
	api.operator.deposit(victim.ID, "1000")
	own := api.bob.createAccount("USD")

	tests := []struct {
		name, method, path, body string
	}{
		{"read balance", "GET", "/v1/accounts/" + victim.ID, ""},
		{"read statement", "GET", "/v1/accounts/" + victim.ID + "/transactions", ""},
		{"transfer out", "POST", "/v1/transfers", `{"from_account_id":"` + victim.ID + `","to_account_id":"` + own.ID + `","amount":100}`},
		{"transfer more than the balance", "POST", "/v1/transfers", `{"from_account_id":"` + victim.ID + `","to_account_id":"` + own.ID + `","amount":999999}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiErr apiError
			resp := api.bob.do(tt.method, tt.path, uuid.NewString(), tt.body, &apiErr)
			// Indistinguishable from an account that does not exist.
			expectError(t, resp, apiErr, http.StatusNotFound, "account_not_found")
		})
	}

	if got := api.alice.balance(victim.ID); got != 1000 {
		t.Fatalf("victim balance = %d, want 1000", got)
	}
	if got := api.bob.balance(own.ID); got != 0 {
		t.Fatalf("bob balance = %d, want 0", got)
	}

	// Paying into someone else's account is allowed.
	api.operator.deposit(own.ID, "50")
	resp := api.bob.do(http.MethodPost, "/v1/transfers", uuid.NewString(),
		`{"from_account_id":"`+own.ID+`","to_account_id":"`+victim.ID+`","amount":50}`, nil)
	expectStatus(t, resp, http.StatusCreated)
	if got := api.alice.balance(victim.ID); got != 1050 {
		t.Fatalf("victim balance = %d, want 1050", got)
	}
}

func TestDepositAndWithdrawRequireOperator(t *testing.T) {
	api := newAPI(t, apiOptions{})
	acc := api.alice.createAccount("USD")
	api.operator.deposit(acc.ID, "1000")

	for _, op := range []string{"deposits", "withdrawals"} {
		for name, c := range map[string]*client{"owner": api.alice, "other client": api.bob} {
			t.Run(op+" by "+name, func(t *testing.T) {
				var apiErr apiError
				resp := c.do(http.MethodPost, "/v1/accounts/"+acc.ID+"/"+op, uuid.NewString(), `{"amount":100}`, &apiErr)
				expectError(t, resp, apiErr, http.StatusForbidden, "forbidden")
			})
		}
	}
	if got := api.alice.balance(acc.ID); got != 1000 {
		t.Fatalf("balance = %d, want 1000", got)
	}

	expectStatus(t, api.operator.do(http.MethodPost, "/v1/accounts/"+acc.ID+"/withdrawals", uuid.NewString(), `{"amount":400}`, nil), http.StatusCreated)
	if got := api.operator.balance(acc.ID); got != 600 {
		t.Fatalf("balance = %d, want 600", got)
	}

	// Operators move money in and out but cannot spend a client's funds.
	opsAcc := api.operator.createAccount("USD")
	var apiErr apiError
	resp := api.operator.do(http.MethodPost, "/v1/transfers", uuid.NewString(),
		`{"from_account_id":"`+acc.ID+`","to_account_id":"`+opsAcc.ID+`","amount":1}`, &apiErr)
	expectError(t, resp, apiErr, http.StatusNotFound, "account_not_found")
}

func TestErrorResponses(t *testing.T) {
	api := newAPI(t, apiOptions{})
	alice, ops := api.alice, api.operator
	usd := alice.createAccount("USD")
	eur := alice.createAccount("EUR")
	ops.deposit(usd.ID, "100")
	unknown := uuid.NewString()

	tests := []struct {
		name                    string
		as                      *client
		method, path, key, body string
		status                  int
		code                    string
	}{
		{"missing idempotency key", alice, "POST", "/v1/accounts", "", `{"name":"A","currency":"USD"}`, 400, "idempotency_key_missing"},
		{"idempotency key too long", alice, "POST", "/v1/accounts", strings.Repeat("k", 256), `{"name":"A","currency":"USD"}`, 400, "idempotency_key_invalid"},
		{"invalid currency", alice, "POST", "/v1/accounts", "k1", `{"name":"A","currency":"dollar"}`, 400, "invalid_currency"},
		{"blank name", alice, "POST", "/v1/accounts", "k2", `{"name":"","currency":"USD"}`, 400, "invalid_name"},
		{"malformed json", alice, "POST", "/v1/accounts", "k3", `{"name":`, 400, "invalid_request"},
		{"unknown field", alice, "POST", "/v1/accounts", "k4", `{"name":"A","currency":"USD","admin":true}`, 400, "invalid_request"},
		{"trailing data", alice, "POST", "/v1/accounts", "k5", `{"name":"A","currency":"USD"} {}`, 400, "invalid_request"},
		{"fractional amount", ops, "POST", "/v1/accounts/" + usd.ID + "/deposits", "k6", `{"amount":10.5}`, 400, "invalid_request"},
		{"zero amount", ops, "POST", "/v1/accounts/" + usd.ID + "/deposits", "k7", `{"amount":0}`, 400, "invalid_amount"},
		{"invalid account id", alice, "GET", "/v1/accounts/not-a-uuid", "", "", 400, "invalid_request"},
		{"unknown account", alice, "GET", "/v1/accounts/" + unknown, "", "", 404, "account_not_found"},
		{"deposit to unknown account", ops, "POST", "/v1/accounts/" + unknown + "/deposits", "k8", `{"amount":1}`, 404, "account_not_found"},
		{"insufficient funds", ops, "POST", "/v1/accounts/" + usd.ID + "/withdrawals", "k9", `{"amount":101}`, 422, "insufficient_funds"},
		{"currency mismatch", alice, "POST", "/v1/transfers", "k10", `{"from_account_id":"` + usd.ID + `","to_account_id":"` + eur.ID + `","amount":1}`, 422, "currency_mismatch"},
		{"same account", alice, "POST", "/v1/transfers", "k11", `{"from_account_id":"` + usd.ID + `","to_account_id":"` + usd.ID + `","amount":1}`, 400, "same_account"},
		{"invalid transfer account id", alice, "POST", "/v1/transfers", "k12", `{"from_account_id":"x","to_account_id":"` + usd.ID + `","amount":1}`, 400, "invalid_request"},
		{"invalid limit", alice, "GET", "/v1/accounts/" + usd.ID + "/transactions?limit=1000", "", "", 400, "invalid_request"},
		{"invalid cursor", alice, "GET", "/v1/accounts/" + usd.ID + "/transactions?cursor=bm9wZQ", "", "", 400, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiErr apiError
			resp := tt.as.do(tt.method, tt.path, tt.key, tt.body, &apiErr)
			expectError(t, resp, apiErr, tt.status, tt.code)
		})
	}
}

func TestInvalidBodyErrorsAreGeneric(t *testing.T) {
	api := newAPI(t, apiOptions{})
	for name, body := range map[string]string{
		"malformed":     `{"name":`,
		"unknown field": `{"name":"A","currency":"USD","is_admin":true}`,
		"wrong type":    `{"name":["x"],"currency":"USD"}`,
		"trailing data": `{"name":"A","currency":"USD"} {"x":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			var apiErr apiError
			resp := api.alice.do(http.MethodPost, "/v1/accounts", uuid.NewString(), body, &apiErr)
			expectError(t, resp, apiErr, http.StatusBadRequest, "invalid_request")
			if apiErr.Error.Message != "request body is not valid JSON for this endpoint" {
				t.Fatalf("message = %q, want the generic message", apiErr.Error.Message)
			}
		})
	}
}

func TestRateLimit(t *testing.T) {
	// One request every 1000s: the burst is all a caller gets during the test.
	api := newAPI(t, apiOptions{limit: httpapi.RateLimit{RequestsPerSecond: 0.001, Burst: 2}})
	acc := api.alice.createAccount("USD")

	expectStatus(t, api.alice.do(http.MethodGet, "/v1/accounts/"+acc.ID, "", "", nil), http.StatusOK)
	var apiErr apiError
	resp := api.alice.do(http.MethodGet, "/v1/accounts/"+acc.ID, "", "", &apiErr)
	expectError(t, resp, apiErr, http.StatusTooManyRequests, "rate_limited")
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 response without Retry-After")
	}

	// Limits are per client.
	expectStatus(t, api.bob.do(http.MethodGet, "/v1/accounts/"+acc.ID, "", "", nil), http.StatusNotFound)

	// Failed authentication is limited per remote address and never reaches
	// the client buckets.
	expectStatus(t, api.anonymous.do(http.MethodGet, "/v1/accounts/"+acc.ID, "", "", nil), http.StatusUnauthorized)
	expectStatus(t, api.anonymous.do(http.MethodGet, "/v1/accounts/"+acc.ID, "", "", nil), http.StatusUnauthorized)
	resp = api.anonymous.do(http.MethodGet, "/v1/accounts/"+acc.ID, "", "", &apiErr)
	expectError(t, resp, apiErr, http.StatusTooManyRequests, "rate_limited")
	expectStatus(t, api.bob.do(http.MethodGet, "/v1/accounts/"+acc.ID, "", "", nil), http.StatusNotFound)

	// Probes are never limited.
	for range 5 {
		expectStatus(t, api.anonymous.do(http.MethodGet, "/healthz", "", "", nil), http.StatusOK)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	api := newAPI(t, apiOptions{})
	body := `{"name":"` + strings.Repeat("a", 2<<20) + `","currency":"USD"}`
	var apiErr apiError
	expectStatus(t, api.alice.do(http.MethodPost, "/v1/accounts", "big", body, &apiErr), http.StatusRequestEntityTooLarge)
}

func TestHealthAndReadiness(t *testing.T) {
	healthy := newAPI(t, apiOptions{}).anonymous
	expectStatus(t, healthy.do(http.MethodGet, "/healthz", "", "", nil), http.StatusOK)
	expectStatus(t, healthy.do(http.MethodGet, "/readyz", "", "", nil), http.StatusOK)

	unready := newAPI(t, apiOptions{ready: func(context.Context) error { return errors.New("database unreachable") }}).anonymous
	expectStatus(t, unready.do(http.MethodGet, "/healthz", "", "", nil), http.StatusOK)
	var body map[string]string
	expectStatus(t, unready.do(http.MethodGet, "/readyz", "", "", &body), http.StatusServiceUnavailable)
	if body["status"] != "unavailable" {
		t.Fatalf("readyz body = %v", body)
	}
}

func TestRequestIDHeader(t *testing.T) {
	c := newAPI(t, apiOptions{}).anonymous
	resp := c.do(http.MethodGet, "/healthz", "", "", nil)
	if _, err := uuid.Parse(resp.Header.Get("X-Request-ID")); err != nil {
		t.Fatalf("generated request id %q is not a UUID", resp.Header.Get("X-Request-ID"))
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpapi.NewHandler(usecase.NewLedger(memory.New()), nil, httpapi.RateLimit{}, nil, log).ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "abc-123" {
		t.Fatalf("X-Request-ID = %q, want the caller's id", got)
	}
}

func TestAccessLogIncludesClientID(t *testing.T) {
	var logs bytes.Buffer
	api := newAPI(t, apiOptions{log: slog.New(slog.NewJSONHandler(&logs, nil))})
	api.alice.createAccount("USD")
	if !strings.Contains(logs.String(), `"client_id":"alice"`) {
		t.Fatalf("access log does not name the client: %s", logs.String())
	}
	if strings.Contains(logs.String(), strings.TrimPrefix(api.alice.authorization, "Bearer ")) {
		t.Fatal("the API key must never be logged")
	}
}

type panickingLedger struct{ httpapi.Ledger }

func (panickingLedger) GetAccount(context.Context, usecase.Principal, uuid.UUID) (domain.Account, error) {
	panic("boom")
}

func TestPanicRecovery(t *testing.T) {
	var logs bytes.Buffer
	api := newAPI(t, apiOptions{ledger: panickingLedger{}, log: slog.New(slog.NewJSONHandler(&logs, nil))})

	expectStatus(t, api.alice.do(http.MethodGet, "/v1/accounts/"+uuid.NewString(), "", "", nil), http.StatusInternalServerError)
	if !strings.Contains(logs.String(), "panic while serving request") {
		t.Fatalf("panic was not logged: %s", logs.String())
	}
}
