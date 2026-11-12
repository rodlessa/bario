// Command auth is the Chirp identity service. It registers users, issues JWTs
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

	"golang.org/x/crypto/bcrypt"

	"chirp/pkg/db"
	chirphttp "chirp/pkg/http"
	"chirp/pkg/jwt"
	"chirp/pkg/metrics"
)

const schema = "auth"

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)

type server struct {
	db     *sql.DB
	tokens *jwt.Manager
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("auth service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := chirphttp.SignalContext()
	defer stop()

	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"), schema)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool,
		`CREATE TABLE IF NOT EXISTS auth.users (
			id BIGSERIAL PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			email TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
	); err != nil {
		return err
	}

	tokens, err := jwt.NewManager(os.Getenv("JWT_SECRET"), 24*time.Hour)
	if err != nil {
		return err
	}

	srv := &server{db: pool, tokens: tokens}
	m := metrics.New("auth")
	router := chirphttp.NewRouter(m)
	srv.registerRoutes(router)

	return chirphttp.Serve(ctx, ":"+env("HTTP_PORT", "8080"), router.Handler(), ":"+env("METRICS_PORT", "9090"), m.Handler())
}

func (s *server) registerRoutes(router *chirphttp.Router) {
	for _, prefix := range []string{"", "/api/auth"} {
		router.Get(prefix+"/healthz", "health", chirphttp.Healthy)
		router.Post(prefix+"/register", "register", s.handleRegister)
		router.Post(prefix+"/login", "login", s.handleLogin)
		router.Get(prefix+"/users/{id}", "get_user", s.handleGetUser)
	}
}

type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !chirphttp.DecodeJSON(w, r, &req) {
		return
	}

	username := strings.TrimSpace(req.Username)
	email := strings.ToLower(strings.TrimSpace(req.Email))

	if !usernamePattern.MatchString(username) {
		chirphttp.WriteError(w, http.StatusBadRequest, "username must be 3-32 characters and contain only letters, digits or underscores")
		return
	}
	if len(req.Password) < 8 || len(req.Password) > 72 {
		chirphttp.WriteError(w, http.StatusBadRequest, "password must be between 8 and 72 characters")
		return
	}
	if addr, err := netmail.ParseAddress(email); err != nil || addr.Address != email || len(email) > 254 {
		chirphttp.WriteError(w, http.StatusBadRequest, "a valid email address is required")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("hash password", "error", err)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not create account")
		return
	}

	var id int64
	err = s.db.QueryRowContext(r.Context(),
		`INSERT INTO auth.users (username, password_hash, email) VALUES ($1, $2, $3) RETURNING id`,
		username, string(hash), email,
	).Scan(&id)
	if err != nil {
		if db.IsUniqueViolation(err) {
			chirphttp.WriteError(w, http.StatusConflict, "username is already taken")
			return
		}
		slog.Error("insert user", "error", err)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not create account")
		return
	}

	chirphttp.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "username": username})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !chirphttp.DecodeJSON(w, r, &req) {
		return
	}

	username := strings.TrimSpace(req.Username)
	var (
		id       int64
		hash     string
		storedAs string
	)
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, username, password_hash FROM auth.users WHERE username = $1`, username,
	).Scan(&id, &storedAs, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		chirphttp.WriteError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if err != nil {
		slog.Error("lookup user", "error", err)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not sign in")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		chirphttp.WriteError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := s.tokens.Issue(id, storedAs)
	if err != nil {
		slog.Error("issue token", "error", err)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not sign in")
		return
	}

	chirphttp.WriteJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user":  map[string]any{"id": id, "username": storedAs},
	})
}

func (s *server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}

	var (
		username  string
		createdAt time.Time
	)
	err := s.db.QueryRowContext(r.Context(),
		`SELECT username, created_at FROM auth.users WHERE id = $1`, id,
	).Scan(&username, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		chirphttp.WriteError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		slog.Error("get user", "error", err)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not load user")
		return
	}

	chirphttp.WriteJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"username":   username,
		"created_at": createdAt,
	})
}

func parseID(w http.ResponseWriter, raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		chirphttp.WriteError(w, http.StatusBadRequest, "invalid id")
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
