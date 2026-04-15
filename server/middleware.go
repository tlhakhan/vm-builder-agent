package server

import (
	"log/slog"
	"net/http"
	"time"
)

// cnMiddleware rejects any request whose client certificate CN is not the
// expected value. Only used when mTLS is enabled.
func cnMiddleware(expectedCN string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			slog.Warn("request missing client certificate", "remote", r.RemoteAddr)
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		cn := r.TLS.PeerCertificates[0].Subject.CommonName
		if cn != expectedCN {
			slog.Warn("client CN mismatch",
				"remote", r.RemoteAddr,
				"got_cn", cn,
				"want_cn", expectedCN,
			)
			http.Error(w, "forbidden: unexpected client CN", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs method, path, status code, and duration for every
// request.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"status", lw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// loggingResponseWriter wraps http.ResponseWriter to capture the status code.
type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (lw *loggingResponseWriter) WriteHeader(code int) {
	lw.status = code
	lw.ResponseWriter.WriteHeader(code)
}
