// Package auth handles JWT issuance/verification and password hashing.
//
// Design decisions:
//   - JWTs are stateless. Pros: no session table, horizontal scaling is trivial.
//     Cons: we cannot revoke a token before it expires. For a task manager
//     the blast radius is small, so we accept this. A production system with
//     sensitive operations would use short-lived access tokens + refresh tokens
//     stored in a revocation list.
//   - bcrypt for passwords. Argon2id would be stronger but bcrypt is in the
//     Go std-crypto module and is still the most widely reviewed choice.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// contextKey is a private type so nobody outside this package can collide
// with our context key. This is the idiomatic Go pattern for context values.
type contextKey string

const userIDKey contextKey = "userID"

// HashPassword returns a bcrypt hash. Cost 12 is a reasonable 2026 default;
// higher = slower login + more DoS-resistant but costs CPU on every signup.
func HashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), 12)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword returns nil on match, an error otherwise. bcrypt.Compare is
// constant-time with respect to its inputs, so we do not leak timing info.
func CheckPassword(hash, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}

// Claims is our custom JWT payload. Embedding RegisteredClaims gives us
// standard fields (exp, iat, iss, sub) for free.
type Claims struct {
	UserID string `json:"uid"`
	jwt.RegisteredClaims
}

// secret reads the signing key from env. Panicking at startup if missing
// is intentional: running with a blank secret would silently accept forged tokens.
func secret() []byte {
	s := os.Getenv("JWT_SECRET")
	if s == "" {
		panic("JWT_SECRET must be set")
	}
	return []byte(s)
}

// IssueToken signs a token valid for 7 days.
// 7 days is a tradeoff: longer = less friction, shorter = smaller window if leaked.
func IssueToken(userID string) (string, error) {
	claims := Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(7 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "taskflow",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(secret())
}

// ParseToken verifies signature + expiry and returns the user ID.
// Returning just the ID (not the full claims) forces callers to go through
// this function and prevents misuse of unverified claims.
func ParseToken(tokenStr string) (string, error) {
	claims := &Claims{}
	// jwt.ParseWithClaims takes a keyfunc so it can reject mismatched algorithms.
	// We explicitly check the method to prevent the classic "alg: none" attack.
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret(), nil
	})
	if err != nil {
		return "", err
	}
	if claims.UserID == "" {
		return "", errors.New("token has no user id")
	}
	return claims.UserID, nil
}

// WithUser attaches the user ID to a context. Called by the HTTP middleware
// after it verifies a token; resolvers later pull the ID out to check auth.
func WithUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// UserFromContext returns the user ID or "" if unauthenticated.
// Resolvers call this to enforce auth — returning "" (not panicking) lets
// public queries like `me` return null gracefully.
func UserFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userIDKey).(string)
	return v
}

// ErrUnauthenticated is returned by resolvers that require auth when the
// context has no user. Exposing a named error lets the HTTP layer map it
// to a GraphQL error extension with a proper code.
var ErrUnauthenticated = errors.New("unauthenticated")
