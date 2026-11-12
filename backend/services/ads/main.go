// Command ads serves advertising campaigns for Bario. This scaffold implements
// authenticated campaign creation and listing; serving, status changes and
// click tracking arrive in follow-up tasks.
package main

import (
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"

	"bario/pkg/db"
	bariohttp "bario/pkg/http"
	"bario/pkg/jwt"
	"bario/pkg/metrics"
)

const (
	schema = "ads"

	maxTitleLength = 120
	maxBodyLength  = 2000
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

type createAdRequest struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	TargetTags  []string `json:"target_tags"`
	BudgetCents int64    `json:"budget_cents"`
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

	srv := &server{db: pool, tokens: tokens}
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

	router.Post("/ads", "create_ad", create)
	router.Post("/api/ads", "create_ad", create)
	router.Get("/ads", "list_ads", list)
	router.Get("/api/ads", "list_ads", list)
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
