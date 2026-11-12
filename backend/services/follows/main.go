// Command follows manages the directed follower graph between Bario accounts.
package main

import (
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"bario/pkg/authclient"
	"bario/pkg/db"
	bariohttp "bario/pkg/http"
	"bario/pkg/jwt"
	"bario/pkg/metrics"
)

const schema = "follows"

type server struct {
	db     *sql.DB
	tokens *jwt.Manager
	auth   *authclient.Client
}

type userRef struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("follows service stopped", "error", err)
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

	if err := db.Migrate(ctx, pool,
		`CREATE TABLE IF NOT EXISTS follows.follows (
			follower_id BIGINT NOT NULL,
			followee_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (follower_id, followee_id)
		)`,
		`CREATE INDEX IF NOT EXISTS follows_followee_id_idx ON follows.follows (followee_id)`,
	); err != nil {
		return err
	}

	tokens, err := jwt.NewManager(os.Getenv("JWT_SECRET"), 24*time.Hour)
	if err != nil {
		return err
	}

	srv := &server{
		db:     pool,
		tokens: tokens,
		auth:   authclient.New(env("AUTH_URL", "http://localhost:8081")),
	}
	m := metrics.New("follows")
	router := bariohttp.NewRouter(m)
	srv.registerRoutes(router)

	return bariohttp.Serve(ctx, ":"+env("HTTP_PORT", "8080"), router.Handler(), ":"+env("METRICS_PORT", "9090"), m.Handler())
}

func (s *server) registerRoutes(router *bariohttp.Router) {
	router.Get("/healthz", "health", bariohttp.Healthy)
	router.Get("/api/follows/healthz", "health", bariohttp.Healthy)

	follow := bariohttp.RequireAuth(s.tokens, s.handleFollow)
	unfollow := bariohttp.RequireAuth(s.tokens, s.handleUnfollow)
	router.Post("/users/{id}/follow", "follow_user", follow)
	router.Delete("/users/{id}/follow", "unfollow_user", unfollow)
	router.Post("/api/follows/users/{id}/follow", "follow_user", follow)
	router.Delete("/api/follows/users/{id}/follow", "unfollow_user", unfollow)

	router.Get("/users/{id}/followers", "list_followers", s.handleFollowers)
	router.Get("/users/{id}/following", "list_following", s.handleFollowing)
	router.Get("/api/follows/users/{id}/followers", "list_followers", s.handleFollowers)
	router.Get("/api/follows/users/{id}/following", "list_following", s.handleFollowing)
}

func (s *server) handleFollow(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	followeeID, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	if followeeID == claims.UserID {
		bariohttp.WriteError(w, http.StatusBadRequest, "you cannot follow yourself")
		return
	}

	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO follows.follows (follower_id, followee_id)
		 VALUES ($1, $2)
		 ON CONFLICT (follower_id, followee_id) DO NOTHING`,
		claims.UserID, followeeID,
	)
	if err != nil {
		slog.Error("follow user", "error", err, "follower", claims.UserID, "followee", followeeID)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not follow user")
		return
	}

	bariohttp.WriteJSON(w, http.StatusCreated, map[string]bool{"following": true})
}

func (s *server) handleUnfollow(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	followeeID, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}

	_, err := s.db.ExecContext(r.Context(),
		`DELETE FROM follows.follows WHERE follower_id = $1 AND followee_id = $2`,
		claims.UserID, followeeID,
	)
	if err != nil {
		slog.Error("unfollow user", "error", err, "follower", claims.UserID, "followee", followeeID)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not unfollow user")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleFollowers(w http.ResponseWriter, r *http.Request) {
	s.listRelations(w, r, "followee_id", "follower_id")
}

func (s *server) handleFollowing(w http.ResponseWriter, r *http.Request) {
	s.listRelations(w, r, "follower_id", "followee_id")
}

// listRelations returns the users on one side of the graph for the user in the
// path. filterColumn is the column filtering by that user, idColumn is the
// column holding the ids to return.
func (s *server) listRelations(w http.ResponseWriter, r *http.Request, filterColumn, idColumn string) {
	userID, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}

	query := `SELECT ` + idColumn + ` FROM follows.follows WHERE ` + filterColumn + ` = $1 ORDER BY created_at DESC, ` + idColumn + ` DESC`
	rows, err := s.db.QueryContext(r.Context(), query, userID)
	if err != nil {
		slog.Error("list relations", "error", err, "user_id", userID)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load users")
		return
	}
	defer rows.Close()

	ids := make([]int64, 0, 16)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			slog.Error("scan relation", "error", err)
			bariohttp.WriteError(w, http.StatusInternalServerError, "could not load users")
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		slog.Error("iterate relations", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load users")
		return
	}

	users := make([]userRef, 0, len(ids))
	for _, id := range ids {
		users = append(users, userRef{ID: id, Username: s.auth.Username(r.Context(), id)})
	}

	bariohttp.WriteJSON(w, http.StatusOK, users)
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
