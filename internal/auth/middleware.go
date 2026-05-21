package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const bearerPrefix = "Bearer "

// Middleware returns an http.Handler middleware that requires an
// `Authorization: Bearer <key>` header matching expectedKey. Empty expectedKey
// is treated as a misconfiguration and rejects all requests.
func Middleware(expectedKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expectedKey == "" {
				unauthorized(w)
				return
			}
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, bearerPrefix) {
				unauthorized(w)
				return
			}
			provided := strings.TrimPrefix(header, bearerPrefix)
			if subtle.ConstantTimeCompare([]byte(provided), []byte(expectedKey)) != 1 {
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="mindgraph"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
