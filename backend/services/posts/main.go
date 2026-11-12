// Command posts is the Bario timeline service. It stores posts, serves single
// posts and user profiles' posts, and builds the home feed from the accounts the
// caller follows.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"

	"bario/pkg/authclient"
	"bario/pkg/db"
	bariohttp "bario/pkg/http"
	"bario/pkg/jwt"
	"bario/pkg/metrics"
)

const (
	schema       = "posts"
	maxContent   = 280
	defaultLimit = 20
	maxLimit     = 100
)

var imageIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// postMigrations is shared with main_test.go so tests apply exactly the schema
// run() uses.
var postMigrations = []string{
	`CREATE TABLE IF NOT EXISTS posts.posts (
		id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL,
		content TEXT NOT NULL,
		image_id TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS posts_user_id_id_idx ON posts.posts (user_id, id DESC)`,
}

type server struct {
	db      *sql.DB
	tokens  *jwt.Manager
	auth    *authclient.Client
	follows *followsClient
	ads     *adsClient
}

type author struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type post struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Content   string    `json:"content"`
	ImageID   *string   `json:"image_id"`
	CreatedAt time.Time `json:"created_at"`
	Author    author    `json:"author"`
}

type postList struct {
	Posts      []post  `json:"posts"`
	NextCursor *string `json:"next_cursor"`
}

type feedPostItem struct {
	Kind string `json:"kind"`
	post
}

type feedAdItem struct {
	Kind string `json:"kind"`
	servedAd
}

type feedList struct {
	Items      []any   `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("posts service stopped", "error", err)
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

	if err := db.Migrate(ctx, pool, postMigrations...); err != nil {
		return err
	}

	tokens, err := jwt.NewManager(os.Getenv("JWT_SECRET"), 24*time.Hour)
	if err != nil {
		return err
	}

	srv := &server{
		db:      pool,
		tokens:  tokens,
		auth:    authclient.New(env("AUTH_URL", "http://localhost:8081")),
		follows: newFollowsClient(env("FOLLOWS_URL", "http://localhost:8083")),
		ads:     newAdsClient(env("ADS_URL", "http://localhost:8087")),
	}

	m := metrics.New("posts")
	router := bariohttp.NewRouter(m)
	srv.registerRoutes(router)

	return bariohttp.Serve(ctx, ":"+env("HTTP_PORT", "8080"), router.Handler(), ":"+env("METRICS_PORT", "9090"), m.Handler())
}

func (s *server) registerRoutes(router *bariohttp.Router) {
	router.Get("/healthz", "health", bariohttp.Healthy)
	router.Get("/api/posts/healthz", "health", bariohttp.Healthy)

	createPost := bariohttp.RequireAuth(s.tokens, s.handleCreatePost)
	router.Post("/posts", "create_post", createPost)
	router.Post("/api/posts", "create_post", createPost)
	router.Post("/api/posts/posts", "create_post", createPost)

	getFeed := bariohttp.RequireAuth(s.tokens, s.handleFeed)
	router.Get("/feed", "get_feed", getFeed)
	router.Get("/api/posts/feed", "get_feed", getFeed)

	router.Get("/posts/{id}", "get_post", s.handleGetPost)
	router.Get("/api/posts/{id}", "get_post", s.handleGetPost)
	router.Get("/api/posts/posts/{id}", "get_post", s.handleGetPost)

	router.Get("/users/{id}/posts", "list_user_posts", s.handleListUserPosts)
	router.Get("/api/posts/users/{id}/posts", "list_user_posts", s.handleListUserPosts)
}

type createPostRequest struct {
	Content string  `json:"content"`
	ImageID *string `json:"image_id"`
}

func (s *server) handleCreatePost(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	var req createPostRequest
	if !bariohttp.DecodeJSON(w, r, &req) {
		return
	}

	content := strings.TrimSpace(req.Content)
	if content == "" {
		bariohttp.WriteError(w, http.StatusBadRequest, "content must not be empty")
		return
	}
	if utf8.RuneCountInString(content) > maxContent {
		bariohttp.WriteError(w, http.StatusBadRequest, "content must be at most 280 characters")
		return
	}

	imageID := sql.NullString{}
	if req.ImageID != nil {
		trimmed := strings.TrimSpace(*req.ImageID)
		if trimmed != "" {
			if !imageIDPattern.MatchString(trimmed) {
				bariohttp.WriteError(w, http.StatusBadRequest, "invalid image_id")
				return
			}
			imageID = sql.NullString{String: trimmed, Valid: true}
		}
	}

	created := post{UserID: claims.UserID, Content: content}
	err := s.db.QueryRowContext(r.Context(),
		`INSERT INTO posts.posts (user_id, content, image_id)
		 VALUES ($1, $2, $3)
		 RETURNING id, created_at`,
		claims.UserID, content, imageID,
	).Scan(&created.ID, &created.CreatedAt)
	if err != nil {
		slog.Error("insert post", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not create post")
		return
	}
	if imageID.Valid {
		created.ImageID = &imageID.String
	}
	created.Author = author{ID: claims.UserID, Username: claims.Username}

	bariohttp.WriteJSON(w, http.StatusCreated, created)
}

func (s *server) handleGetPost(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}

	row := s.db.QueryRowContext(r.Context(),
		`SELECT id, user_id, content, image_id, created_at
		 FROM posts.posts WHERE id = $1`, id)
	found, err := scanPost(row)
	if errors.Is(err, sql.ErrNoRows) {
		bariohttp.WriteError(w, http.StatusNotFound, "post not found")
		return
	}
	if err != nil {
		slog.Error("get post", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load post")
		return
	}

	posts := []post{found}
	s.attachAuthors(r.Context(), posts)
	bariohttp.WriteJSON(w, http.StatusOK, posts[0])
}

func (s *server) handleListUserPosts(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	limit, cursor, ok := parsePagination(w, r)
	if !ok {
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, user_id, content, image_id, created_at
		 FROM posts.posts
		 WHERE user_id = $1 AND ($2 = 0 OR id < $2)
		 ORDER BY id DESC
		 LIMIT $3`,
		userID, cursor, limit,
	)
	if err != nil {
		slog.Error("list user posts", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load posts")
		return
	}
	defer rows.Close()

	posts, err := collectPosts(rows)
	if err != nil {
		slog.Error("scan user posts", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load posts")
		return
	}
	s.attachAuthors(r.Context(), posts)

	bariohttp.WriteJSON(w, http.StatusOK, postList{Posts: posts, NextCursor: nextCursor(posts, limit)})
}

func (s *server) handleFeed(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	limit, cursor, ok := parsePagination(w, r)
	if !ok {
		return
	}
	pages, ok := parsePages(w, r)
	if !ok {
		return
	}

	followedIDs, err := s.follows.FollowingIDs(r.Context(), claims.UserID)
	if err != nil {
		slog.Error("load following", "error", err, "user_id", claims.UserID)
		bariohttp.WriteError(w, http.StatusBadGateway, "could not load the accounts you follow")
		return
	}
	if len(followedIDs) == 0 {
		bariohttp.WriteJSON(w, http.StatusOK, feedList{Items: []any{}, NextCursor: nil})
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, user_id, content, image_id, created_at
		 FROM posts.posts
		 WHERE user_id = ANY($1) AND ($2 = 0 OR id < $2)
		 ORDER BY id DESC
		 LIMIT $3`,
		pq.Array(followedIDs), cursor, limit,
	)
	if err != nil {
		slog.Error("load feed", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load feed")
		return
	}
	defer rows.Close()

	posts, err := collectPosts(rows)
	if err != nil {
		slog.Error("scan feed", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load feed")
		return
	}
	s.attachAuthors(r.Context(), posts)

	// Ads are not load-bearing: any failure degrades to an organic-only feed.
	var ads []servedAd
	if s.ads != nil {
		ads, err = s.ads.Serve(r.Context(), r.Header.Get("Authorization"), pages, limit)
		if err != nil {
			slog.Warn("serve ads", "error", err, "user_id", claims.UserID)
			ads = nil
		}
	}

	bariohttp.WriteJSON(w, http.StatusOK, feedList{
		Items:      interleaveFeed(posts, ads),
		NextCursor: nextCursor(posts, limit),
	})
}

// interleaveFeed spreads ads evenly between organic posts. Ads never appear at
// the front or trailing end unless there is no room between posts, and they do
// not participate in cursor pagination.
func interleaveFeed(posts []post, ads []servedAd) []any {
	items := make([]any, 0, len(posts)+len(ads))
	if len(posts) == 0 || len(ads) == 0 {
		for _, p := range posts {
			items = append(items, feedPostItem{Kind: "post", post: p})
		}
		return items
	}

	positions := make([]int, len(ads))
	for i := range ads {
		positions[i] = (i + 1) * len(posts) / (len(ads) + 1)
	}

	adIndex := 0
	for adIndex < len(ads) && positions[adIndex] == 0 {
		items = append(items, feedAdItem{Kind: "ad", servedAd: ads[adIndex]})
		adIndex++
	}
	for i, p := range posts {
		items = append(items, feedPostItem{Kind: "post", post: p})
		for adIndex < len(ads) && positions[adIndex] == i+1 {
			items = append(items, feedAdItem{Kind: "ad", servedAd: ads[adIndex]})
			adIndex++
		}
	}
	return items
}

// attachAuthors resolves display names through the auth service, caching each
// lookup within a single response.
func (s *server) attachAuthors(ctx context.Context, posts []post) {
	names := make(map[int64]string, len(posts))
	for i := range posts {
		userID := posts[i].UserID
		name, ok := names[userID]
		if !ok {
			name = s.auth.Username(ctx, userID)
			names[userID] = name
		}
		posts[i].Author = author{ID: userID, Username: name}
	}
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPost(row rowScanner) (post, error) {
	var (
		p       post
		imageID sql.NullString
	)
	if err := row.Scan(&p.ID, &p.UserID, &p.Content, &imageID, &p.CreatedAt); err != nil {
		return post{}, err
	}
	if imageID.Valid {
		p.ImageID = &imageID.String
	}
	return p, nil
}

func collectPosts(rows *sql.Rows) ([]post, error) {
	posts := make([]post, 0, defaultLimit)
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

func parsePagination(w http.ResponseWriter, r *http.Request) (limit int, cursor int64, ok bool) {
	limit = defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxLimit {
			bariohttp.WriteError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return 0, 0, false
		}
		limit = value
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			bariohttp.WriteError(w, http.StatusBadRequest, "invalid cursor")
			return 0, 0, false
		}
		cursor = value
	}
	return limit, cursor, true
}

func nextCursor(posts []post, limit int) *string {
	if len(posts) < limit || len(posts) == 0 {
		return nil
	}
	cursor := strconv.FormatInt(posts[len(posts)-1].ID, 10)
	return &cursor
}

// parsePages reads the optional pages counter used to scale ad density. It
// mirrors the ads service: absent or negative values mean zero, invalid values
// are a client error.
func parsePages(w http.ResponseWriter, r *http.Request) (int, bool) {
	pages := 0
	if raw := r.URL.Query().Get("pages"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			bariohttp.WriteError(w, http.StatusBadRequest, "invalid pages")
			return 0, false
		}
		if value > 0 {
			pages = value
		}
	}
	return pages, true
}

func parseID(w http.ResponseWriter, raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		bariohttp.WriteError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

type followsClient struct {
	baseURL string
	http    *http.Client
}

func newFollowsClient(baseURL string) *followsClient {
	return &followsClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

// FollowingIDs asks the follows service which accounts userID follows.
func (c *followsClient) FollowingIDs(ctx context.Context, userID int64) ([]int64, error) {
	if c.baseURL == "" {
		return nil, errors.New("FOLLOWS_URL is not configured")
	}
	url := fmt.Sprintf("%s/users/%d/following", c.baseURL, userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call follows service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("follows service returned %s", resp.Status)
	}

	var users []struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&users); err != nil {
		return nil, fmt.Errorf("decode follows response: %w", err)
	}

	ids := make([]int64, 0, len(users))
	for _, user := range users {
		if user.ID > 0 {
			ids = append(ids, user.ID)
		}
	}
	return ids, nil
}

type servedAd struct {
	ID           int64  `json:"id"`
	AdvertiserID int64  `json:"advertiser_id"`
	Title        string `json:"title"`
	Body         string `json:"body"`
}

type adsClient struct {
	baseURL string
	http    *http.Client
}

func newAdsClient(baseURL string) *adsClient {
	return &adsClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

// Serve asks the ads service for ads for this feed page. The caller's bearer
// token is forwarded because ads/serve requires auth.
func (c *adsClient) Serve(ctx context.Context, authorization string, pages, limit int) ([]servedAd, error) {
	if c.baseURL == "" {
		return nil, errors.New("ADS_URL is not configured")
	}
	url := fmt.Sprintf("%s/ads/serve?pages=%d&limit=%d", c.baseURL, pages, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call ads service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ads service returned %s", resp.Status)
	}

	var ads []servedAd
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ads); err != nil {
		return nil, fmt.Errorf("decode ads response: %w", err)
	}
	return ads, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
