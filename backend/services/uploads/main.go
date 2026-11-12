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
	"sync"
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

	// hashMatcherName labels quarantine rows. Replace it with a stable
	// provider name (for example "photodna" or "ncmec") when a real,
	// registered hash-matching client is implemented.
	hashMatcherName = "hash-matcher"
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type server struct {
	db            *sql.DB
	tokens        *jwt.Manager
	uploadDir     string
	quarantineDir string
	moderation    *moderationClient
	gore          *goreClient
	hashMatcher   HashMatcher
}

// HashMatcher checks image bytes against a known-abuse hash corpus.
//
// Bario does not ship a CSAM detector. A real implementation would wrap the
// Microsoft PhotoDNA Cloud Service or an NCMEC CyberTipline hash-matching
// integration. Both require prior legal/organizational registration and an
// agreement with the provider, so implementing one speculatively here would
// be wrong. Implement this interface only after that agreement exists, then
// replace noopHashMatcher in run().
type HashMatcher interface {
	Match(ctx context.Context, data []byte) (matched bool, err error)
}

// noopHashMatcher is the default seam. It performs no I/O and makes no network
// calls, so current upload behavior and latency are unchanged.
type noopHashMatcher struct{}

func (noopHashMatcher) Match(context.Context, []byte) (bool, error) {
	return false, nil
}

// nsfwScores is the exact JSON object returned by the NSFWJS sidecar.
type nsfwScores struct {
	Neutral float64 `json:"Neutral"`
	Drawing float64 `json:"Drawing"`
	Sexy    float64 `json:"Sexy"`
	Hentai  float64 `json:"Hentai"`
	Porn    float64 `json:"Porn"`
}

// risky sums the classes that trigger a rejection.
func (s nsfwScores) risky(includeSexy bool) float64 {
	total := s.Porn + s.Hentai
	if includeSexy {
		total += s.Sexy
	}
	return total
}

// valid rejects malformed classifier responses so a broken sidecar cannot
// silently approve everything.
func (s nsfwScores) valid() bool {
	return validProbabilities(s.Neutral, s.Drawing, s.Sexy, s.Hentai, s.Porn)
}

// goreScores is the exact JSON object returned by the CLIP gore sidecar.
type goreScores struct {
	GraphicViolence float64 `json:"graphic violence"`
	Gore            float64 `json:"gore"`
	Blood           float64 `json:"blood"`
	NormalPhoto     float64 `json:"a normal photo"`
	EverydayScene   float64 `json:"everyday scene"`
}

// risky sums the explicit violence/gore/blood prompts. The neutral control
// prompts are deliberately excluded.
func (s goreScores) risky() float64 {
	return s.GraphicViolence + s.Gore + s.Blood
}

func (s goreScores) valid() bool {
	return validProbabilities(s.GraphicViolence, s.Gore, s.Blood, s.NormalPhoto, s.EverydayScene)
}

// combinedScores is the JSONB shape stored in moderation_scores.
type combinedScores struct {
	NSFW *nsfwScores `json:"nsfw"`
	Gore *goreScores `json:"gore"`
}

func validProbabilities(values ...float64) bool {
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
	// failOpen is the shared policy for both classifiers (MODERATION_FAIL_OPEN).
	failOpen bool
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

// goreClient posts raw image bytes to the CLIP gore sidecar.
type goreClient struct {
	baseURL    string
	httpClient *http.Client
	threshold  float64
}

func newGoreClient(baseURL string, threshold float64) *goreClient {
	return &goreClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: moderationTimeout},
		threshold:  threshold,
	}
}

// postClassify sends the image to one sidecar and decodes its JSON response.
// The caller is responsible for validating the returned probabilities.
func postClassify(ctx context.Context, httpClient *http.Client, baseURL string, data []byte, contentType string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, moderationTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/classify", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, moderationMaxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("classifier returned %s", resp.Status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode classifier response: %w", err)
	}
	return nil
}

func (c *moderationClient) classify(ctx context.Context, data []byte, contentType string) (nsfwScores, error) {
	var scores nsfwScores
	if err := postClassify(ctx, c.httpClient, c.baseURL, data, contentType, &scores); err != nil {
		return scores, err
	}
	if !scores.valid() {
		return scores, errors.New("classifier returned invalid probabilities")
	}
	return scores, nil
}

func (c *goreClient) classify(ctx context.Context, data []byte, contentType string) (goreScores, error) {
	var scores goreScores
	if err := postClassify(ctx, c.httpClient, c.baseURL, data, contentType, &scores); err != nil {
		return scores, err
	}
	if !scores.valid() {
		return scores, errors.New("classifier returned invalid probabilities")
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
		`CREATE TABLE IF NOT EXISTS uploads.quarantine (
			id TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL,
			size BIGINT NOT NULL,
			matched_by TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS quarantine_user_id_idx ON uploads.quarantine (user_id)`,
		`CREATE INDEX IF NOT EXISTS quarantine_created_at_idx ON uploads.quarantine (created_at)`,
	); err != nil {
		return err
	}

	uploadDir := filepath.Clean(env("UPLOAD_DIR", "/data/uploads"))
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return err
	}
	// Keep quarantined bytes outside the public upload tree. The directory is
	// created lazily only when a matcher reports a hit.
	quarantineDir := filepath.Clean(env("QUARANTINE_DIR", filepath.Join(filepath.Dir(uploadDir), "quarantine")))

	tokens, err := jwt.NewManager(os.Getenv("JWT_SECRET"), 24*time.Hour)
	if err != nil {
		return err
	}

	threshold, err := envFloat("MODERATION_REJECT_THRESHOLD", 0.7)
	if err != nil {
		return err
	}
	if err := validateThreshold("MODERATION_REJECT_THRESHOLD", threshold); err != nil {
		return err
	}
	goreThreshold, err := envFloat("GORE_REJECT_THRESHOLD", 0.7)
	if err != nil {
		return err
	}
	if err := validateThreshold("GORE_REJECT_THRESHOLD", goreThreshold); err != nil {
		return err
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
	goreURL := env("GORE_MODERATION_URL", "http://localhost:8086")
	slog.Info("moderation configured",
		"nsfw_url", moderationURL,
		"gore_url", goreURL,
		"reject_threshold", threshold,
		"gore_reject_threshold", goreThreshold,
		"include_sexy", includeSexy,
		"fail_open", failOpen,
	)

	srv := &server{
		db:            pool,
		tokens:        tokens,
		uploadDir:     uploadDir,
		quarantineDir: quarantineDir,
		moderation:    newModerationClient(moderationURL, threshold, includeSexy, failOpen),
		gore:          newGoreClient(goreURL, goreThreshold),
		// This is the single seam for CSAM hash matching. Do not replace it
		// with a network client until a PhotoDNA or NCMEC agreement exists.
		hashMatcher: noopHashMatcher{},
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
	// Keep this list identical to backend/services/moderation/src/images.js.
	// Go's sniffer also returns image/x-icon for ICO, but neither tfjs nor
	// sharp decodes ICO, so it must not reach the classifier.
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/bmp", "image/webp":
	default:
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

	var (
		nsfwResult  nsfwScores
		goreResult  goreScores
		nsfwErr     error
		goreErr     error
		hashMatched bool
		hashErr     error
	)

	// The sidecars and the hash matcher are independent, so run them
	// concurrently instead of paying their latencies serially.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		nsfwResult, nsfwErr = s.moderation.classify(r.Context(), data, contentType)
	}()
	go func() {
		defer wg.Done()
		goreResult, goreErr = s.gore.classify(r.Context(), data, contentType)
	}()
	go func() {
		defer wg.Done()
		matchCtx, cancel := context.WithTimeout(r.Context(), moderationTimeout)
		defer cancel()
		hashMatched, hashErr = s.hashMatcher.Match(matchCtx, data)
	}()
	wg.Wait()

	// Hash matching is a separate safeguard from the content classifiers. If
	// the matcher errors, fail closed regardless of MODERATION_FAIL_OPEN, and
	// never fall through to normal storage.
	if hashErr != nil {
		slog.Error("csam hash matcher failed", "error", hashErr, "user_id", claims.UserID)
		bariohttp.WriteError(w, http.StatusServiceUnavailable, "image moderation is unavailable; try again later")
		return
	}
	if hashMatched {
		if err := s.quarantineUpload(r.Context(), id, claims.UserID, filename, contentType, data); err != nil {
			slog.Error("could not quarantine hash-matched upload",
				"error", err,
				"id", id,
				"user_id", claims.UserID,
			)
			bariohttp.WriteError(w, http.StatusInternalServerError, "could not store image")
			return
		}
		slog.Error("possible CSAM hash match; upload quarantined",
			"event", "csam_hash_match",
			"id", id,
			"user_id", claims.UserID,
			"size", len(data),
			"matched_by", hashMatcherName,
		)
		bariohttp.WriteError(w, http.StatusUnavailableForLegalReasons, "image rejected by content moderation")
		return
	}

	scores := &combinedScores{}
	if nsfwErr == nil {
		scores.NSFW = &nsfwResult
	}
	if goreErr == nil {
		scores.Gore = &goreResult
	}

	var reasons []string
	rejected := false

	if nsfwErr == nil {
		risky := nsfwResult.risky(s.moderation.includeSexy)
		if risky >= s.moderation.threshold {
			rejected = true
			if s.moderation.includeSexy {
				reasons = append(reasons, fmt.Sprintf(
					"nsfw: porn+hentai+sexy=%.4f >= threshold %g", risky, s.moderation.threshold))
			} else {
				reasons = append(reasons, fmt.Sprintf(
					"nsfw: porn+hentai=%.4f >= threshold %g", risky, s.moderation.threshold))
			}
		}
	}

	if goreErr == nil {
		risky := goreResult.risky()
		if risky >= s.gore.threshold {
			rejected = true
			reasons = append(reasons, fmt.Sprintf(
				"gore: graphic_violence+gore+blood=%.4f >= threshold %g", risky, s.gore.threshold))
		}
	}

	if nsfwErr != nil {
		reasons = append(reasons, fmt.Sprintf("nsfw: classifier error: %v", nsfwErr))
	}
	if goreErr != nil {
		reasons = append(reasons, fmt.Sprintf("gore: classifier error: %v", goreErr))
	}

	if rejected {
		reason := strings.Join(reasons, "; ")
		slog.Warn("image rejected by moderation",
			"id", id,
			"user_id", claims.UserID,
			"reasons", reason,
		)
		s.recordModerationAudit(r.Context(), id, claims.UserID, filename, contentType, len(data), "rejected", scores, reason)
		bariohttp.WriteError(w, http.StatusUnavailableForLegalReasons, "image rejected by content moderation")
		return
	}

	if nsfwErr != nil || goreErr != nil {
		reason := strings.Join(reasons, "; ")
		slog.Error("moderation failed", "id", id, "user_id", claims.UserID, "reasons", reason)

		if !s.moderation.failOpen {
			s.recordModerationAudit(r.Context(), id, claims.UserID, filename, contentType, len(data), "error", scores, reason)
			bariohttp.WriteError(w, http.StatusServiceUnavailable, "image moderation is unavailable; try again later")
			return
		}

		slog.Warn("moderation failed open; accepting upload", "id", id, "user_id", claims.UserID)
		if err := s.storeImage(r.Context(), id, claims.UserID, filename, contentType, data, "unavailable", scores); err != nil {
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

	if err := s.storeImage(r.Context(), id, claims.UserID, filename, contentType, data, "approved", scores); err != nil {
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
func (s *server) storeImage(ctx context.Context, id string, userID int64, filename, contentType string, data []byte, status string, scores *combinedScores) error {
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
func (s *server) recordModerationAudit(ctx context.Context, id string, userID int64, filename, contentType string, size int, status string, scores *combinedScores, reason string) {
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

// quarantineUpload stores bytes outside the public upload tree and records the
// hit in uploads.quarantine. This is the operational trigger point for a
// future NCMEC CyberTipline report; the report itself is out of scope.
func (s *server) quarantineUpload(ctx context.Context, id string, userID int64, filename, contentType string, data []byte) error {
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), moderationTimeout)
	defer cancel()

	if err := os.MkdirAll(s.quarantineDir, 0o700); err != nil {
		return fmt.Errorf("create quarantine directory: %w", err)
	}
	path := filepath.Join(s.quarantineDir, id)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write quarantine file: %w", err)
	}

	_, err := s.db.ExecContext(qctx,
		`INSERT INTO uploads.quarantine
			(id, user_id, filename, content_type, size, matched_by)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, userID, filename, contentType, len(data), hashMatcherName,
	)
	if err != nil {
		if removeErr := os.Remove(path); removeErr != nil {
			slog.Error("remove orphaned quarantine file", "error", removeErr, "path", path)
		}
		return fmt.Errorf("insert quarantine row: %w", err)
	}
	return nil
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

func validateThreshold(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return fmt.Errorf("%s must be a number between 0 and 1", name)
	}
	return nil
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
