// Package jwt issues and verifies the HS256 tokens shared by the Bario
// services. The signing secret comes from the JWT_SECRET environment variable.
package jwt

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer is stamped into every token issued by the auth service.
const Issuer = "bario-auth"

var (
	ErrMissingToken = errors.New("missing bearer token")
	ErrInvalidToken = errors.New("invalid token")
)

// Claims is the token payload used across the platform.
type Claims struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// Manager signs and verifies tokens with a single shared secret.
type Manager struct {
	secret []byte
	ttl    time.Duration
}

// NewManager validates the secret and returns a token manager.
func NewManager(secret string, ttl time.Duration) (*Manager, error) {
	if len(secret) < 16 {
		return nil, errors.New("JWT_SECRET must be at least 16 characters long")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Manager{secret: []byte(secret), ttl: ttl}, nil
}

// Issue creates a signed token for the given user.
func (m *Manager) Issue(userID int64, username string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   strconv.FormatInt(userID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

// Parse verifies a raw token and returns its claims.
func (m *Manager) Parse(raw string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return m.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(Issuer))
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.UserID <= 0 {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// FromRequest extracts and verifies the bearer token of an HTTP request.
func (m *Manager) FromRequest(r *http.Request) (*Claims, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return nil, ErrMissingToken
	}
	scheme, raw, ok := strings.Cut(header, " ")
	raw = strings.TrimSpace(raw)
	if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
		return nil, ErrMissingToken
	}
	return m.Parse(raw)
}
