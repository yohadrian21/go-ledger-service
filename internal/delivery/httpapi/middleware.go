package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

const (
	headerRequestID    = "X-Request-ID"
	maxRequestIDLength = 128
)

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// withRequestID propagates the caller's X-Request-ID or generates one.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if id == "" || len(id) > maxRequestIDLength {
			id = uuid.NewString()
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// accessLogFields carries values that inner handlers learn about a request,
// such as the authenticated client, out to the access log.
type accessLogFields struct {
	clientID string
}

type accessLogKey struct{}

func setAccessLogClientID(ctx context.Context, clientID string) {
	if f, ok := ctx.Value(accessLogKey{}).(*accessLogFields); ok {
		f.clientID = clientID
	}
}

// withAccessLog writes one structured log line per request, including the
// authenticated client if there is one. Probe requests are logged at debug
// level to keep the logs readable.
func withAccessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		fields := &accessLogFields{}
		r = r.WithContext(context.WithValue(r.Context(), accessLogKey{}, fields))
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := slog.LevelInfo
		switch {
		case rec.status >= http.StatusInternalServerError:
			level = slog.LevelError
		case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
			level = slog.LevelDebug
		}
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("request_id", requestIDFrom(r.Context())),
		}
		if fields.clientID != "" {
			attrs = append(attrs, slog.String("client_id", fields.clientID))
		}
		log.LogAttrs(r.Context(), level, "http request", attrs...)
	})
}

// withRecovery turns a panic in a handler into a 500 response.
func withRecovery(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			log.ErrorContext(r.Context(), "panic while serving request",
				"panic", v, "stack", string(debug.Stack()), "request_id", requestIDFrom(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
