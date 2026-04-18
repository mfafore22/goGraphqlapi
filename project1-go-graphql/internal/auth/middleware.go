package auth

import (
	"net/http"
	"strings"
)

// Middleware reads the Authorization header, verifies the token, and
// stashes the user ID in the request context. On missing/invalid tokens
// it does NOT reject — it just leaves the context unauthenticated.
// This lets public operations (signup, login, some queries) still work.
//
// The actual "must be logged in" check happens inside resolvers, closer
// to the business logic. That's more flexible than a blanket HTTP gate.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		// Expected form: "Bearer <token>". Anything else -> leave context clean.
		if strings.HasPrefix(header, "Bearer ") {
			tok := strings.TrimPrefix(header, "Bearer ")
			if uid, err := ParseToken(tok); err == nil {
				r = r.WithContext(WithUser(r.Context(), uid))
			}
			// We deliberately swallow the error. A malformed token is
			// treated the same as no token — the resolver will return
			// "unauthenticated" if it requires auth.
		}
		next.ServeHTTP(w, r)
	})
}
