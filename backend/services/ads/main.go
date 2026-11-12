// Command ads serves advertising campaigns for Bario. This scaffold implements
// authenticated campaign creation, listing, and owner-only status updates;
// serving and click tracking arrive in follow-up tasks.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
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
	schema = "ads"

	maxTitleLength = 120
	maxBodyLength  = 2000

	// Intentionally small status enum for this scaffold.
	statusActive = "active"
	statusPaused = "paused"
)

// adsMigrations is shared with main_test.go so tests apply exactly the schema
// run() uses.
var adsMigrations = []string{
	`CREATE TABLE IF NOT EXISTS ads.advertisements (
		id BIGSERIAL PRIMARY KEY,
		advertiser_id BIGINT NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		target_tags TEXT[] NOT NULL DEFAULT '{}',
		budget_cents BIGINT NOT NULL CHECK (budget_cents >= 0),
		status TEXT NOT NULL DEFAULT 'active',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS advertisements_advertiser_id_idx ON ads.advertisements (advertiser_id)`,
	`CREATE INDEX IF NOT EXISTS advertisements_status_idx ON ads.advertisements (status)`,
	`CREATE TABLE IF NOT EXISTS ads.impressions (
		id BIGSERIAL PRIMARY KEY,
		ad_id BIGINT NOT NULL REFERENCES ads.advertisements (id) ON DELETE CASCADE,
		user_id BIGINT NOT NULL,
		served_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS impressions_ad_id_idx ON ads.impressions (ad_id)`,
	`CREATE INDEX IF NOT EXISTS impressions_user_id_idx ON ads.impressions (user_id)`,
	`CREATE TABLE IF NOT EXISTS ads.clicks (
		id BIGSERIAL PRIMARY KEY,
		ad_id BIGINT NOT NULL REFERENCES ads.advertisements (id) ON DELETE CASCADE,
		user_id BIGINT NOT NULL,
		clicked_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS clicks_ad_id_idx ON ads.clicks (ad_id)`,
	`CREATE INDEX IF NOT EXISTS clicks_user_id_idx ON ads.clicks (user_id)`,
}

type server struct {
	db     *sql.DB
	tokens *jwt.Manager
	auth   *authclient.Client
}

type ad struct {
	ID           int64     `json:"id"`
	AdvertiserID int64     `json:"advertiser_id"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	TargetTags   []string  `json:"target_tags"`
	BudgetCents  int64     `json:"budget_cents"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type servedAd struct {
	ID           int64  `json:"id"`
	AdvertiserID int64  `json:"advertiser_id"`
	Title        string `json:"title"`
	Body         string `json:"body"`
}

type createAdRequest struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	TargetTags  []string `json:"target_tags"`
	BudgetCents int64    `json:"budget_cents"`
}

type updateAdRequest struct {
	Status string `json:"status"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("ads service stopped", "error", err)
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

	if err := db.Migrate(ctx, pool, adsMigrations...); err != nil {
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
	m := metrics.New("ads")
	router := bariohttp.NewRouter(m)
	srv.registerRoutes(router)

	return bariohttp.Serve(ctx, ":"+env("HTTP_PORT", "8087"), router.Handler(), ":"+env("METRICS_PORT", "9097"), m.Handler())
}

func (s *server) registerRoutes(router *bariohttp.Router) {
	router.Get("/healthz", "health", bariohttp.Healthy)
	router.Get("/api/ads/healthz", "health", bariohttp.Healthy)

	create := bariohttp.RequireAuth(s.tokens, s.handleCreateAd)
	list := bariohttp.RequireAuth(s.tokens, s.handleListAds)
	update := bariohttp.RequireAuth(s.tokens, s.handleUpdateAd)
	serve := bariohttp.RequireAuth(s.tokens, s.handleServeAds)
	click := bariohttp.RequireAuth(s.tokens, s.handleClickAd)

	router.Post("/ads", "create_ad", create)
	router.Post("/api/ads", "create_ad", create)
	router.Get("/ads", "list_ads", list)
	router.Get("/api/ads", "list_ads", list)
	router.Patch("/ads/{id}", "update_ad", update)
	router.Patch("/api/ads/{id}", "update_ad", update)
	router.Get("/ads/serve", "serve_ads", serve)
	router.Get("/api/ads/serve", "serve_ads", serve)
	router.Post("/ads/{id}/click", "click_ad", click)
	router.Post("/api/ads/{id}/click", "click_ad", click)
}

func (s *server) handleCreateAd(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	var req createAdRequest
	if !bariohttp.DecodeJSON(w, r, &req) {
		return
	}

	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	if title == "" {
		bariohttp.WriteError(w, http.StatusBadRequest, "title must not be empty")
		return
	}
	if utf8.RuneCountInString(title) > maxTitleLength {
		bariohttp.WriteError(w, http.StatusBadRequest, "title must be at most 120 characters")
		return
	}
	if body == "" {
		bariohttp.WriteError(w, http.StatusBadRequest, "body must not be empty")
		return
	}
	if utf8.RuneCountInString(body) > maxBodyLength {
		bariohttp.WriteError(w, http.StatusBadRequest, "body must be at most 2000 characters")
		return
	}
	if req.BudgetCents < 0 {
		bariohttp.WriteError(w, http.StatusBadRequest, "budget_cents must be zero or greater")
		return
	}

	tags := req.TargetTags
	if tags == nil {
		tags = []string{}
	}

	created := ad{
		AdvertiserID: claims.UserID,
		Title:        title,
		Body:         body,
		TargetTags:   tags,
		BudgetCents:  req.BudgetCents,
	}
	err := s.db.QueryRowContext(r.Context(),
		`INSERT INTO ads.advertisements
			(advertiser_id, title, body, target_tags, budget_cents)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, status, created_at, updated_at`,
		claims.UserID, title, body, pq.Array(tags), req.BudgetCents,
	).Scan(&created.ID, &created.Status, &created.CreatedAt, &created.UpdatedAt)
	if err != nil {
		slog.Error("create advertisement", "error", err, "advertiser", claims.UserID)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not create ad")
		return
	}

	bariohttp.WriteJSON(w, http.StatusCreated, created)
}

func (s *server) handleUpdateAd(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}

	var req updateAdRequest
	if !bariohttp.DecodeJSON(w, r, &req) {
		return
	}

	// Intentionally a small enum for now; future statuses need an explicit
	// product decision and migration.
	status := strings.TrimSpace(req.Status)
	if status != statusActive && status != statusPaused {
		bariohttp.WriteError(w, http.StatusBadRequest, `status must be "active" or "paused"`)
		return
	}

	var updated ad
	err := s.db.QueryRowContext(r.Context(),
		`UPDATE ads.advertisements
		 SET status = $1, updated_at = now()
		 WHERE id = $2 AND advertiser_id = $3
		 RETURNING id, advertiser_id, title, body, target_tags, budget_cents, status, created_at, updated_at`,
		status, id, claims.UserID,
	).Scan(
		&updated.ID,
		&updated.AdvertiserID,
		&updated.Title,
		&updated.Body,
		pq.Array(&updated.TargetTags),
		&updated.BudgetCents,
		&updated.Status,
		&updated.CreatedAt,
		&updated.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Not-found and not-owned share this response so non-owners cannot
		// probe which ad ids exist.
		bariohttp.WriteError(w, http.StatusNotFound, "ad not found")
		return
	}
	if err != nil {
		slog.Error("update advertisement", "error", err, "id", id, "advertiser", claims.UserID)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not update ad")
		return
	}
	if updated.TargetTags == nil {
		updated.TargetTags = []string{}
	}

	bariohttp.WriteJSON(w, http.StatusOK, updated)
}

func (s *server) handleServeAds(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	pages, limit, ok := parseServeParams(w, r)
	if !ok {
		return
	}

	ratio := math.Min(0.30, 0.05+0.02*float64(pages))
	adCount := int(math.Round(ratio * float64(limit)))
	if adCount < 0 {
		adCount = 0
	}
	if adCount == 0 {
		bariohttp.WriteJSON(w, http.StatusOK, []servedAd{})
		return
	}

	interests := s.callerInterests(r.Context(), claims.UserID)
	ads := make([]servedAd, 0, adCount)

	if len(interests) > 0 {
		targeted, err := s.queryServedAds(r.Context(),
			`SELECT id, advertiser_id, title, body
			 FROM ads.advertisements
			 WHERE status = 'active' AND target_tags && $1
			 ORDER BY id DESC
			 LIMIT $2`,
			pq.Array(interests), adCount,
		)
		if err != nil {
			slog.Error("serve targeted ads", "error", err, "user_id", claims.UserID)
			bariohttp.WriteError(w, http.StatusInternalServerError, "could not load ads")
			return
		}
		ads = append(ads, targeted...)
	}

	remaining := adCount - len(ads)
	if remaining > 0 {
		general, err := s.queryServedAds(r.Context(),
			`SELECT id, advertiser_id, title, body
			 FROM ads.advertisements
			 WHERE status = 'active' AND target_tags = '{}'::text[]
			 ORDER BY id DESC
			 LIMIT $1`,
			remaining,
		)
		if err != nil {
			slog.Error("serve general ads", "error", err, "user_id", claims.UserID)
			bariohttp.WriteError(w, http.StatusInternalServerError, "could not load ads")
			return
		}
		ads = append(ads, general...)
	}

	for _, ad := range ads {
		if _, err := s.db.ExecContext(r.Context(),
			`INSERT INTO ads.impressions (ad_id, user_id) VALUES ($1, $2)`,
			ad.ID, claims.UserID,
		); err != nil {
			slog.Warn("record impression", "error", err, "ad_id", ad.ID, "user_id", claims.UserID)
		}
	}

	bariohttp.WriteJSON(w, http.StatusOK, ads)
}

func parseServeParams(w http.ResponseWriter, r *http.Request) (pages, limit int, ok bool) {
	pages = 0
	if raw := r.URL.Query().Get("pages"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			bariohttp.WriteError(w, http.StatusBadRequest, "invalid pages")
			return 0, 0, false
		}
		if value > 0 {
			pages = value
		}
	}

	rawLimit := r.URL.Query().Get("limit")
	if rawLimit == "" {
		bariohttp.WriteError(w, http.StatusBadRequest, "limit is required")
		return 0, 0, false
	}
	value, err := strconv.Atoi(rawLimit)
	if err != nil {
		bariohttp.WriteError(w, http.StatusBadRequest, "invalid limit")
		return 0, 0, false
	}
	if value < 1 {
		value = 1
	}
	return pages, value, true
}

func (s *server) callerInterests(ctx context.Context, userID int64) []string {
	if s.auth == nil {
		return []string{}
	}
	user, err := s.auth.User(ctx, userID)
	if err != nil {
		// Ads serving degrades to general-only when identity lookup fails.
		slog.Warn("load caller interests", "error", err, "user_id", userID)
		return []string{}
	}
	if len(user.Interests) == 0 {
		return []string{}
	}
	return user.Interests
}

func (s *server) queryServedAds(ctx context.Context, query string, args ...any) ([]servedAd, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ads := make([]servedAd, 0)
	for rows.Next() {
		var ad servedAd
		if err := rows.Scan(&ad.ID, &ad.AdvertiserID, &ad.Title, &ad.Body); err != nil {
			return nil, err
		}
		ads = append(ads, ad)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ads, nil
}

func (s *server) handleClickAd(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}

	result, err := s.db.ExecContext(r.Context(),
		`INSERT INTO ads.clicks (ad_id, user_id)
		 SELECT $1, $2
		 WHERE EXISTS (SELECT 1 FROM ads.advertisements WHERE id = $1)`,
		id, claims.UserID,
	)
	if err != nil {
		slog.Error("record click", "error", err, "ad_id", id, "user_id", claims.UserID)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not record click")
		return
	}
	affected, err := result.RowsAffected()
	if err != nil {
		slog.Error("click rows affected", "error", err, "ad_id", id, "user_id", claims.UserID)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not record click")
		return
	}
	if affected == 0 {
		bariohttp.WriteError(w, http.StatusNotFound, "ad not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleListAds(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, advertiser_id, title, body, target_tags, budget_cents, status, created_at, updated_at
		 FROM ads.advertisements
		 WHERE advertiser_id = $1
		 ORDER BY id DESC`,
		claims.UserID,
	)
	if err != nil {
		slog.Error("list advertisements", "error", err, "advertiser", claims.UserID)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load ads")
		return
	}
	defer rows.Close()

	ads := make([]ad, 0)
	for rows.Next() {
		var item ad
		if err := rows.Scan(
			&item.ID,
			&item.AdvertiserID,
			&item.Title,
			&item.Body,
			pq.Array(&item.TargetTags),
			&item.BudgetCents,
			&item.Status,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			slog.Error("scan advertisement", "error", err)
			bariohttp.WriteError(w, http.StatusInternalServerError, "could not load ads")
			return
		}
		if item.TargetTags == nil {
			item.TargetTags = []string{}
		}
		ads = append(ads, item)
	}
	if err := rows.Err(); err != nil {
		slog.Error("iterate advertisements", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load ads")
		return
	}

	bariohttp.WriteJSON(w, http.StatusOK, ads)
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func parseID(w http.ResponseWriter, raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		bariohttp.WriteError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}
