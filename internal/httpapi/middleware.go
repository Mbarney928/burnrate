package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/Mbarney928/burnrate/internal/auth"
	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/ratelimit"
)

type ctxKey int

const tokenKey ctxKey = iota

// tokenFrom returns the authenticated token stored by authenticate.
func tokenFrom(ctx context.Context) (model.Token, bool) {
	t, ok := ctx.Value(tokenKey).(model.Token)
	return t, ok
}

// clientIP is the peer address. X-Forwarded-For is deliberately ignored: trusting it
// without a known proxy would let any client pick its own rate-limit key.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// requestLogger logs one structured line per request. It never logs headers or bodies,
// so tokens cannot leak into logs.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("client_ip", clientIP(r)),
			)
		})
	}
}

// recoverer turns a panic into a generic 500 so stack traces never reach clients.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic serving request",
					"request_id", middleware.GetReqID(r.Context()), "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func rateLimit(l *ratelimit.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(clientIP(r)) {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authenticate requires a valid bearer token. Failed attempts are rate limited per IP
// separately (and much more strictly) than normal traffic.
func authenticate(tokens TokenLookup, failures *ratelimit.Limiter, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if failures.Blocked(ip) {
				w.Header().Set("Retry-After", "60")
				writeError(w, http.StatusTooManyRequests, "rate_limited", "too many failed authentication attempts", nil)
				return
			}
			deny := func(reason string) {
				failures.Allow(ip)
				log.WarnContext(r.Context(), "authentication failed",
					"request_id", middleware.GetReqID(r.Context()), "client_ip", ip, "reason", reason)
				w.Header().Set("WWW-Authenticate", `Bearer realm="burnrate"`)
				writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required", nil)
			}

			plain, err := auth.ParseBearer(r.Header.Get("Authorization"))
			if err != nil {
				deny("missing or malformed token")
				return
			}
			tok, err := tokens.TokenByHash(r.Context(), auth.Hash(plain))
			if errors.Is(err, model.ErrNotFound) {
				deny("unknown token")
				return
			}
			if err != nil {
				internalError(w, r, log, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey, tok)))
		})
	}
}

// requireWriter rejects tokens that may only read.
func requireWriter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := tokenFrom(r.Context()); !ok || !tok.Role.CanWrite() {
			writeError(w, http.StatusForbidden, "forbidden", "this token's role cannot change data", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
