package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

// Authenticator verifies the bearer credential of a request and returns the
// client it belongs to. Any error rejects the request.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (usecase.Principal, error)
}

type principalKey struct{}

// authenticate rejects requests without valid credentials, applies the rate
// limit and passes the authenticated client on in the request context.
// Authenticated requests are limited per client. Failed attempts are limited
// per remote address, so guessing keys is slow, while valid clients behind
// the same proxy are not affected.
func (h *handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.authn.Authenticate(r.Context(), bearerToken(r))
		if err != nil {
			if h.limiter.limit(w, "addr:"+remoteHost(r)) {
				writeUnauthorized(w)
			}
			return
		}
		setAccessLogClientID(r.Context(), p.ClientID)
		if !h.limiter.limit(w, "client:"+p.ClientID) {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// callerHandler handles a request on behalf of an authenticated client.
type callerHandler func(w http.ResponseWriter, r *http.Request, caller usecase.Principal)

// withCaller hands the authenticated client to fn. It fails closed if the
// route is not behind authenticate.
func withCaller(fn callerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := r.Context().Value(principalKey{}).(usecase.Principal)
		if !ok {
			writeUnauthorized(w)
			return
		}
		fn(w, r, p)
	}
}

func bearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="go-ledger-service"`)
	writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
}
