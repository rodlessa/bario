// Command uploads stores post images on a PersistentVolume and serves them back
// with their original content type.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bario/pkg/db"
	bariohttp "bario/pkg/http"
	"bario/pkg/jwt"
	"bario/pkg/metrics"
)

const (
	schema        = "uploads"
	maxUploadSize = 10 << 20 // 10 MiB

	moderationTimeout     = 5 * time.Second
	moderationMaxResponse = 1 << 20 // 1 MiB
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type server struct {
	db         *sql.DB
	tokens     *jwt.Manager
	uploadDir  string
	moderation *moderationClient
}

// moderationScores is the exact JSON object returned by the NSFWJS sidecar.
type moderationScores struct {
	Neutral float64 `json:"Neutral"`
	Drawing float64 `json:"Drawing"`
	Sexy    float64 `json:"Sexy"`
	Hentai  float64 `json:"Hentai"`
	Porn    float64 `json:"Porn"`
}

// risky sums the classes that trigger a rejection.
func (s moderationScores) risky(includeSexy bool) float64 {
	total := s.Porn + s.Hentai
	if includeSexy {
		total += s.Sexy
	}
	return total
}

// valid rejects malformed classifier responses so a broken sidecar cannot
// silently approve everything.
func (s moderationScores) valid() bool {
	values := []float64{s.Neutral, s.Drawing, s.Sexy, s.Hentai, s.Porn}
	sum := 0.0
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return false
		}
		sum += value
	}
	return sum > 0.95 && sum < 1.05
}

// moderationClient posts raw image bytes to the NSFWJS sidecar.
type moderationClient struct {
	baseURL     string
	httpClient  *http.Client
	threshold   float64
	includeSexy bool
	failOpen    bool
}

func newModerationClient(baseURL string, threshold float64, includeSexy, failOpen bool) *moderationClient {
	return &moderationClient{
		baseURL:     strings.TrimRight(baseURL, "/"),
		httpClient:  &http.Client{Timeout: moderationTimeout},
		threshold:   threshold,
		includeSexy: includeSexy,
		failOpen:    failOpen,
	}
}

func (c *moderationClient) classify(ctx context.Context, data []byte, contentType string) (moderationScores, error) {
	var scores moderationScores

	ctx, cancel := context.WithTimeout(ctx, moderationTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/classify", bytes.NewReader(data))
	if err != nil {
		return scores, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return scores, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, moderationMaxResponse))
	if err != nil {
		return scores, err
	}
	if resp.StatusCode != http.StatusOK {
		return scores, fmt.Errorf("moderation service returned %s", resp.Status)
	}
	if err := json.Unmarshal(body, &scores); err != nil {
		return scores, fmt.Errorf("decode moderation response: %w", err)
	}
	if !scores.valid() {
		return scores, errors.New("moderation service returned invalid probabilities")
	}
	return scores, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("uploads service stopped", "error", err)
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
		`CREATE TABLE IF NOT EXISTS uploads.images (
			id TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL,
			size BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`ALTER TABLE uploads.images ADD COLUMN IF NOT EXISTS moderation_status TEXT NOT NULL DEFAULT 'approved'`,
		`ALTER TABLE uploads.images ADD COLUMN IF NOT EXISTS moderation_scores JSONB`,
		`CREATE INDEX IF NOT EXISTS images_user_id_idx ON uploads.images (user_id)`,
		`CREATE TABLE IF NOT EXISTS uploads.moderation_audit (
			id TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL,
			size BIGINT NOT NULL,
			moderation_status TEXT NOT NULL,
			moderation_scores JSONB,
			reason TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS moderation_audit_user_id_idx ON uploads.moderation_audit (user_id)`,
		`CREATE INDEX IF NOT EXISTS moderation_audit_created_at_idx ON uploads.moderation_audit (created_at)`,
	); err != nil {
		return err
	}

	uploadDir := env("UPLOAD_DIR", "/data/uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return err
	}

	tokens, err := jwt.NewManager(os.Getenv("JWT_SECRET"), 24*time.Hour)
	if err != nil {
		return err
	}

	threshold, err := envFloat("MODERATION_REJECT_THRESHOLD", 0.7)
	if err != nil {
		return err
	}
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 1 {
		return errors.New("MODERATION_REJECT_THRESHOLD must be a number between 0 and 1")
	}
	includeSexy, err := envBool("MODERATION_INCLUDE_SEXY", false)
	if err != nil {
		return err
	}
	failOpen, err := envBool("MODERATION_FAIL_OPEN", false)
	if err != nil {
		return err
	}
	moderationURL := env("MODERATION_URL", "http://localhost:8085")
	slog.Info("moderation configured",
		"url", moderationURL,
		"reject_threshold", threshold,
		"include_sexy", includeSexy,
		"fail_open", failOpen,
	)

	srv := &server{
		db:         pool,
		tokens:     tokens,
		uploadDir:  uploadDir,
		moderation: newModerationClient(moderationURL, threshold, includeSexy, failOpen),
	}
	m := metrics.New("uploads")
	router := bariohttp.NewRouter(m)
	srv.registerRoutes(router)

	return bariohttp.Serve(ctx, ":"+env("HTTP_PORT", "8080"), router.Handler(), ":"+env("METRICS_PORT", "9090"), m.Handler())
}

func (s *server) registerRoutes(router *bariohttp.Router) {
	router.Get("/healthz", "health", bariohttp.Healthy)
	router.Get("/api/uploads/healthz", "health", bariohttp.Healthy)

	upload := bariohttp.RequireAuth(s.tokens, s.handleUpload)
	router.Post("/uploads", "upload_image", upload)
	router.Post("/api/uploads", "upload_image", upload)
	router.Post("/api/uploads/uploads", "upload_image", upload)

	router.Get("/uploads/{id}", "get_image", s.handleGetImage)
	router.Get("/api/uploads/{id}", "get_image", s.handleGetImage)
	router.Get("/api/uploads/uploads/{id}", "get_image", s.handleGetImage)
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request, claims *jwt.Claims) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+(1<<20))
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			bariohttp.WriteError(w, http.StatusRequestEntityTooLarge, "image must be at most 10 MiB")
			return
		}
		bariohttp.WriteError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		bariohttp.WriteError(w, http.StatusBadRequest, `multipart field "file" is required`)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUploadSize+1))
	if err != nil {
		bariohttp.WriteError(w, http.StatusBadRequest, "could not read uploaded file")
		return
	}
	if len(data) == 0 {
		bariohttp.WriteError(w, http.StatusBadRequest, "uploaded file is empty")
		return
	}
	if len(data) > maxUploadSize {
		bariohttp.WriteError(w, http.StatusRequestEntityTooLarge, "image must be at most 10 MiB")
		return
	}

	contentType := detectImageType(data)
	if !strings.HasPrefix(contentType, "image/") {
		bariohttp.WriteError(w, http.StatusUnsupportedMediaType, "uploaded file is not a supported image")
		return
	}

	id, err := newID()
	if err != nil {
		slog.Error("generate image id", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not store image")
		return
	}

	filename := filepath.Base(header.Filename)
	if filename == "." || filename == string(filepath.Separator) || filename == "" {
		filename = id
	}
	filename = truncate(filename, 200)

	scores, modErr := s.moderation.classify(r.Context(), data, contentType)
	if modErr != nil {
		slog.Error("moderate image", "error", modErr, "user_id", claims.UserID)
		if s.moderation.failOpen {
			slog.Warn("moderation failed open; accepting upload", "id", id, "user_id", claims.UserID)
			if err := s.storeImage(r.Context(), id, claims.UserID, filename, contentType, data, "unavailable", nil); err != nil {
				slog.Error("store image", "error", err, "id", id)
				bariohttp.WriteError(w, http.StatusInternalServerError, "could not store image")
				return
			}
			bariohttp.WriteJSON(w, http.StatusCreated, map[string]string{
				"id":  id,
				"url": "/api/uploads/" + id,
			})
			return
		}

		s.recordModerationAudit(r.Context(), id, claims.UserID, filename, contentType, len(data), "error", nil, modErr.Error())
		bariohttp.WriteError(w, http.StatusServiceUnavailable, "image moderation is unavailable; try again later")
		return
	}

	risky := scores.risky(s.moderation.includeSexy)
	if risky >= s.moderation.threshold {
		reason := fmt.Sprintf("porn+hentai=%.4f >= threshold %g", scores.Porn+scores.Hentai, s.moderation.threshold)
		if s.moderation.includeSexy {
			reason = fmt.Sprintf("porn+hentai+sexy=%.4f >= threshold %g", risky, s.moderation.threshold)
		}
		slog.Warn("image rejected by moderation",
			"id", id,
			"user_id", claims.UserID,
			"risky_score", risky,
			"threshold", s.moderation.threshold,
		)
		s.recordModerationAudit(r.Context(), id, claims.UserID, filename, contentType, len(data), "rejected", &scores, reason)
		bariohttp.WriteError(w, http.StatusUnavailableForLegalReasons, "image rejected by content moderation")
		return
	}

	if err := s.storeImage(r.Context(), id, claims.UserID, filename, contentType, data, "approved", &scores); err != nil {
		slog.Error("store image", "error", err, "id", id)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not store image")
		return
	}

	bariohttp.WriteJSON(w, http.StatusCreated, map[string]string{
		"id":  id,
		"url": "/api/uploads/" + id,
	})
}

// storeImage writes an accepted image and its metadata. Rejected uploads must
// never reach this method.
func (s *server) storeImage(ctx context.Context, id string, userID int64, filename, contentType string, data []byte, status string, scores *moderationScores) error {
	var scoresJSON any
	if scores != nil {
		encoded, err := json.Marshal(scores)
		if err != nil {
			return fmt.Errorf("encode moderation scores: %w", err)
		}
		scoresJSON = string(encoded)
	}

	path := filepath.Join(s.uploadDir, id)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO uploads.images
			(id, user_id, filename, content_type, size, moderation_status, moderation_scores)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		id, userID, filename, contentType, len(data), status, scoresJSON,
	)
	if err != nil {
		if removeErr := os.Remove(path); removeErr != nil {
			slog.Error("remove orphaned image", "error", removeErr, "path", path)
		}
		return err
	}
	return nil
}

// recordModerationAudit records rejected and failed uploads separately from
// uploads.images, so that table only ever references files that exist. It uses
// a detached context so a client disconnect cannot erase the audit trail.
func (s *server) recordModerationAudit(ctx context.Context, id string, userID int64, filename, contentType string, size int, status string, scores *moderationScores, reason string) {
	var scoresJSON any
	if scores != nil {
		encoded, err := json.Marshal(scores)
		if err != nil {
			slog.Error("encode moderation scores for audit", "error", err, "id", id)
			return
		}
		scoresJSON = string(encoded)
	}

	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), moderationTimeout)
	defer cancel()

	_, err := s.db.ExecContext(auditCtx,
		`INSERT INTO uploads.moderation_audit
			(id, user_id, filename, content_type, size, moderation_status, moderation_scores, reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8)`,
		id, userID, filename, contentType, size, status, scoresJSON, truncate(reason, 500),
	)
	if err != nil {
		slog.Error("record moderation audit", "error", err, "id", id, "status", status)
	}
}

func (s *server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		bariohttp.WriteError(w, http.StatusBadRequest, "invalid image id")
		return
	}

	var (
		filename    string
		contentType string
	)
	err := s.db.QueryRowContext(r.Context(),
		`SELECT filename, content_type FROM uploads.images WHERE id = $1`, id,
	).Scan(&filename, &contentType)
	if errors.Is(err, sql.ErrNoRows) {
		bariohttp.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	if err != nil {
		slog.Error("lookup image", "error", err)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load image")
		return
	}

	path := filepath.Join(s.uploadDir, id)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Error("image file missing", "id", id)
			bariohttp.WriteError(w, http.StatusNotFound, "image not found")
			return
		}
		slog.Error("open image", "error", err, "id", id)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load image")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		slog.Error("stat image", "error", err, "id", id)
		bariohttp.WriteError(w, http.StatusInternalServerError, "could not load image")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, filename, info.ModTime(), file)
}

// detectImageType sniffs the content type, including WebP which Go's default
// sniffer does not recognise.
func detectImageType(data []byte) string {
	contentType := http.DetectContentType(data)
	if contentType == "application/octet-stream" &&
		len(data) >= 12 &&
		string(data[0:4]) == "RIFF" &&
		string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return contentType
}

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envFloat(key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", key, err)
	}
	return value, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return value, nil
}
