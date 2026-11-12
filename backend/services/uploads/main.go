// Command uploads stores post images on a PersistentVolume and serves them back
// with their original content type.
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"chirp/pkg/db"
	chirphttp "chirp/pkg/http"
	"chirp/pkg/jwt"
	"chirp/pkg/metrics"
)

const (
	schema        = "uploads"
	maxUploadSize = 10 << 20 // 10 MiB
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type server struct {
	db        *sql.DB
	tokens    *jwt.Manager
	uploadDir string
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("uploads service stopped", "error", err)
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
		`CREATE TABLE IF NOT EXISTS uploads.images (
			id TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL,
			size BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS images_user_id_idx ON uploads.images (user_id)`,
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

	srv := &server{db: pool, tokens: tokens, uploadDir: uploadDir}
	m := metrics.New("uploads")
	router := chirphttp.NewRouter(m)
	srv.registerRoutes(router)

	return chirphttp.Serve(ctx, ":"+env("HTTP_PORT", "8080"), router.Handler(), ":"+env("METRICS_PORT", "9090"), m.Handler())
}

func (s *server) registerRoutes(router *chirphttp.Router) {
	router.Get("/healthz", "health", chirphttp.Healthy)
	router.Get("/api/uploads/healthz", "health", chirphttp.Healthy)

	upload := chirphttp.RequireAuth(s.tokens, s.handleUpload)
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
			chirphttp.WriteError(w, http.StatusRequestEntityTooLarge, "image must be at most 10 MiB")
			return
		}
		chirphttp.WriteError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		chirphttp.WriteError(w, http.StatusBadRequest, `multipart field "file" is required`)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUploadSize+1))
	if err != nil {
		chirphttp.WriteError(w, http.StatusBadRequest, "could not read uploaded file")
		return
	}
	if len(data) == 0 {
		chirphttp.WriteError(w, http.StatusBadRequest, "uploaded file is empty")
		return
	}
	if len(data) > maxUploadSize {
		chirphttp.WriteError(w, http.StatusRequestEntityTooLarge, "image must be at most 10 MiB")
		return
	}

	contentType := detectImageType(data)
	if !strings.HasPrefix(contentType, "image/") {
		chirphttp.WriteError(w, http.StatusUnsupportedMediaType, "uploaded file is not a supported image")
		return
	}

	id, err := newID()
	if err != nil {
		slog.Error("generate image id", "error", err)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not store image")
		return
	}

	path := filepath.Join(s.uploadDir, id)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Error("write image", "error", err)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not store image")
		return
	}

	filename := filepath.Base(header.Filename)
	if filename == "." || filename == string(filepath.Separator) || filename == "" {
		filename = id
	}
	filename = truncate(filename, 200)

	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO uploads.images (id, user_id, filename, content_type, size)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, claims.UserID, filename, contentType, len(data),
	)
	if err != nil {
		slog.Error("insert image metadata", "error", err)
		if removeErr := os.Remove(path); removeErr != nil {
			slog.Error("remove orphaned image", "error", removeErr, "path", path)
		}
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not store image")
		return
	}

	chirphttp.WriteJSON(w, http.StatusCreated, map[string]string{
		"id":  id,
		"url": "/api/uploads/" + id,
	})
}

func (s *server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		chirphttp.WriteError(w, http.StatusBadRequest, "invalid image id")
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
		chirphttp.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	if err != nil {
		slog.Error("lookup image", "error", err)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not load image")
		return
	}

	path := filepath.Join(s.uploadDir, id)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Error("image file missing", "id", id)
			chirphttp.WriteError(w, http.StatusNotFound, "image not found")
			return
		}
		slog.Error("open image", "error", err, "id", id)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not load image")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		slog.Error("stat image", "error", err, "id", id)
		chirphttp.WriteError(w, http.StatusInternalServerError, "could not load image")
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
