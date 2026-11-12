// Command auth is the Bario identity service. It registers users, issues JWTs
// and exposes public profiles.
package main

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	netmail "net/mail"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"

	"bario/pkg/db"
	bariohttp "bario/pkg/http"
	"bario/pkg/jwt"
	"bario/pkg/metrics"
)

const (
	schema = "auth"

	maxInterests      = 32
	maxInterestLength = 64
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)

// authMigrations is shared with main_test.go so tests apply exactly the schema
// run() uses.
var authMigrations = []string{
	`CREATE TABLE IF NOT EXISTS auth.users (
		id BIGSERIAL PRIMARY KEY,
		username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		email TEXT NOT NULL,
		is_admin BOOLEAN NOT NULL DEFAULT false,
		interests TEXT[] NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`ALTER TABLE auth.users ADD COLUMN IF NOT EXISTS is_admin BOOLEAN NOT NULL DEFAULT false`,
	`ALTER TABLE auth.users ADD COLUMN IF NOT EXISTS interests TEXT[] NOT NULL DEFAULT '{}'`,
}

type server struct {
	db            *sql.DB
	tokens        *jwt.Manager
	adminUsername string
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("auth service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := bariohttp.SignalContext()
	defer stop()

	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"), schema)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, authMigrations...); err != nil {
		return err
	}

	tokens, err := jwt.NewManager(os.Getenv("JWT_SECRET"), 24*time.Hour)
	if err != nil {
		return err
	}

	srv := &server{db: pool, tokens: tokens, adminUsername: env("ADMIN_USERNAME", "admin")}
	m := metrics.New("auth")
	router := bariohttp.NewRouter(m)
	srv.registerRoutes(router)

	return bariohttp.Serve(ctx, ":"+env("HTTP_PORT", "8080"), router.Handler(), ":"+env("METRICS_PORT", "9090"), m.Handler())
}

func (s *server) registerRoutes(router *bariohttp.Router) {
	for _, prefix := range []string{"", "/api/auth"} {
		router.Get(prefix+"/healthz", "health", bariohttp.Healthy)
		router.Post(prefix+"/register", "register", s.handleRegister)
		router.Post(prefix+"/login", "login", s.handleLogin)
		router.Get(prefix+"/users/{id}", "get_user", s.handleGetUser)
	}
}

type registerRequest struct {
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	Email     string   `json:"email"`
	Interests []string `json:"interests"`
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !bariohttp.DecodeJSON(w, r, &req) {
		return
	}

	username := strings.TrimSpace(req.Username)
	email := strings.ToLower(strings.TrimSpace(req.Email))

	if !usernamePattern.MatchString(username) {
		bariohttp.WriteError(w, http.StatusBadRequest, "username must be 3-32 characters and contain only letters, digits or underscores")
		return
	}
	if len(req.Password) < 8 || len(req.Password) > 72 {
		bariohttp.WriteError(w, http.StatusBadRequest, "password must be between 8 and 72 characters")
		return
	}
	if addr, err := netmail.ParseAddress(email); err != nil || addr.Address != email || len(email) > 254 {
		bariohttp.WriteError(w, http.StatusBadRequest, "a valid email address is required")
		return
	}
	if len(req.Interests) > maxInterests {
		bariohttp.WriteError(w, http.StatusBadRequest, "interests must contain at most 32 tags")
		return
	}
	interests := make([]string, 0, len(req.Interests))
	for _, raw := range req.Interests {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > maxInterestLength {
			bariohttp.WriteError(w, http.StatusBadRequest, "each interest must be at most 64 characters")
			return
		}
		interests = append(interests, tag)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("hash password", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not create account")
		return
	}

	isAdmin := username == s.adminUsername

	var id int64
	err = s.db.QueryRowContext(r.Context(),
		`INSERT INTO auth.users (username, password_hash, email, is_admin, interests)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		username, string(hash), email, isAdmin, pq.Array(interests),
	).Scan(&id)
	if err != nil {
		if db.IsUniqueViolation(err) {
			bariohttp.WriteError(w, http.StatusConflict, "username is already taken")
			return
		}
		slog.Error("insert user", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not create account")
		return
	}

	bariohttp.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":        id,
		"username":  username,
		"is_admin":  isAdmin,
		"interests": interests,
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !bariohttp.DecodeJSON(w, r, &req) {
		return
	}

	username := strings.TrimSpace(req.Username)
	var (
		id        int64
		hash      string
		storedAs  string
		isAdmin   bool
		interests []string
	)
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, username, password_hash, is_admin, interests FROM auth.users WHERE username = $1`, username,
	).Scan(&id, &storedAs, &hash, &isAdmin, pq.Array(&interests))
	if errors.Is(err, sql.ErrNoRows) {
		bariohttp.WriteError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if err != nil {
		slog.Error("lookup user", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not sign in")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		bariohttp.WriteError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := s.tokens.Issue(id, storedAs)
	if err != nil {
		slog.Error("issue token", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not sign in")
		return
	}

	if interests == nil {
		interests = []string{}
	}

	bariohttp.WriteJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user": map[string]any{
			"id":        id,
			"username":  storedAs,
			"is_admin":  isAdmin,
			"interests": interests,
		},
	})
}

func (s *server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}

	var (
		username  string
		isAdmin   bool
		interests []string
		createdAt time.Time
	)
	err := s.db.QueryRowContext(r.Context(),
		`SELECT username, is_admin, interests, created_at FROM auth.users WHERE id = $1`, id,
	).Scan(&username, &isAdmin, pq.Array(&interests), &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		bariohttp.WriteError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		slog.Error("get user", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load user")
		return
	}

	if interests == nil {
		interests = []string{}
	}

	bariohttp.WriteJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"username":   username,
		"is_admin":   isAdmin,
		"interests":  interests,
		"created_at": createdAt,
	})
}

func parseID(w http.ResponseWriter, raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		bariohttp.WriteError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
