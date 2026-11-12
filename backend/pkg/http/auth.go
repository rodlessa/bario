package bariohttp

import (
	"net/http"

	"bario/pkg/jwt"
)

// AuthedHandler is a handler that receives the verified token claims.
type AuthedHandler func(http.ResponseWriter, *http.Request, *jwt.Claims)

// RequireAuth rejects requests without a valid bearer token and passes the
// verified claims to next.
func RequireAuth(tokens *jwt.Manager, next AuthedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := tokens.FromRequest(r)
		if err != nil {
			WriteError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r, claims)
	}
}
