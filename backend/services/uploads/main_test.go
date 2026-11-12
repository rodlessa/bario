package main

// Test infrastructure: the suite starts postgres:16-alpine with the Docker CLI
// on a random loopback port, waits with db.Connect, applies the same
// uploadMigrations as run(), and removes the container in TestMain.
//
// Both classifier dependencies are faked with httptest servers, and the hash
// matcher is a direct HashMatcher implementation. No real sidecars, auth
// service, or manually running Postgres instance are required.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bario/pkg/db"
	bariohttp "bario/pkg/http"
	"bario/pkg/jwt"
	"bario/pkg/metrics"
)

const (
	testPostgresImage = "postgres:16-alpine"
	testDBPassword    = "bario-test-password"
	testJWTSecret     = "bario-uploads-tests"
)

var testPool *sql.DB

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	container, dsn, err := startTestPostgres()
	if err != nil {
		log.Fatalf("uploads tests: %v", err)
	}
	cleanup := func() {
		if output, err := exec.Command("docker", "rm", "-f", container).CombinedOutput(); err != nil {
			log.Printf("uploads tests: remove %s: %v (%s)", container, err, strings.TrimSpace(string(output)))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	pool, err := db.Connect(ctx, dsn, "uploads")
	cancel()
	if err != nil {
		cleanup()
		log.Fatalf("uploads tests: connect to postgres: %v", err)
	}
	if err := db.Migrate(context.Background(), pool, uploadMigrations...); err != nil {
		pool.Close()
		cleanup()
		log.Fatalf("uploads tests: apply migrations: %v", err)
	}
	testPool = pool

	code := m.Run()

	pool.Close()
	cleanup()
	os.Exit(code)
}

func startTestPostgres() (string, string, error) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return "", "", fmt.Errorf("docker CLI is required to run uploads tests")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("allocate postgres port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	container := fmt.Sprintf("bario-uploads-test-%d", time.Now().UnixNano())
	args := []string{
		"run", "-d", "--rm", "--name", container,
		"-e", "POSTGRES_USER=bario",
		"-e", fmt.Sprintf("POSTGRES_PASSWORD=%s", testDBPassword),
		"-e", "POSTGRES_DB=bario",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		testPostgresImage,
	}
	output, err := exec.Command(docker, args...).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start %s: %w: %s", testPostgresImage, err, strings.TrimSpace(string(output)))
	}

	dsn := fmt.Sprintf("postgres://bario:%s@127.0.0.1:%d/bario?sslmode=disable", testDBPassword, port)
	return container, dsn, nil
}

type sidecarFake struct {
	status int
	body   string
	calls  atomic.Int32
	server *httptest.Server
}

func newSidecarFake(t *testing.T, status int, body string) *sidecarFake {
	t.Helper()
	fake := &sidecarFake{status: status, body: body}
	fake.server = httptest.NewServer(fake)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *sidecarFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	if f.body != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.body)
	}
}

type fakeHashMatcher struct {
	matched bool
	err     error
	calls   atomic.Int32
}

func (f *fakeHashMatcher) Match(context.Context, []byte) (bool, error) {
	f.calls.Add(1)
	return f.matched, f.err
}

type uploadTestEnv struct {
	handler       http.Handler
	tokens        *jwt.Manager
	uploadDir     string
	quarantineDir string
	nsfw          *sidecarFake
	gore          *sidecarFake
	hash          *fakeHashMatcher
}

func newUploadTestEnv(t *testing.T, nsfw, gore *sidecarFake, hash *fakeHashMatcher, failOpen bool) *uploadTestEnv {
	t.Helper()
	resetUploads(t)
	if hash == nil {
		hash = &fakeHashMatcher{}
	}

	root := t.TempDir()
	uploadDir := filepath.Join(root, "uploads")
	quarantineDir := filepath.Join(root, "quarantine")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatalf("create upload dir: %v", err)
	}

	tokens, err := jwt.NewManager(testJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("new jwt manager: %v", err)
	}

	srv := &server{
		db:            testPool,
		tokens:        tokens,
		uploadDir:     uploadDir,
		quarantineDir: quarantineDir,
		moderation:    newModerationClient(nsfw.server.URL, 0.7, false, failOpen),
		gore:          newGoreClient(gore.server.URL, 0.7),
		hashMatcher:   hash,
	}
	router := bariohttp.NewRouter(metrics.New("uploads"))
	srv.registerRoutes(router)
	return &uploadTestEnv{
		handler:       router.Handler(),
		tokens:        tokens,
		uploadDir:     uploadDir,
		quarantineDir: quarantineDir,
		nsfw:          nsfw,
		gore:          gore,
		hash:          hash,
	}
}

func resetUploads(t *testing.T) {
	t.Helper()
	if _, err := testPool.ExecContext(context.Background(),
		`TRUNCATE uploads.images, uploads.moderation_audit, uploads.quarantine`); err != nil {
		t.Fatalf("reset uploads tables: %v", err)
	}
}

func tokenFor(t *testing.T, tokens *jwt.Manager, userID int64, username string) string {
	t.Helper()
	token, err := tokens.Issue(userID, username)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func sidecarBody(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal sidecar response: %v", err)
	}
	return string(raw)
}

func cleanNSFWValues() nsfwScores {
	return nsfwScores{Neutral: 0.95, Drawing: 0.03, Sexy: 0.005, Hentai: 0.005, Porn: 0.01}
}

func rejectNSFWValues() nsfwScores {
	return nsfwScores{Neutral: 0.05, Drawing: 0.03, Sexy: 0.02, Hentai: 0.10, Porn: 0.80}
}

func cleanGoreValues() goreScores {
	return goreScores{
		GraphicViolence: 0.02,
		Gore:            0.03,
		Blood:           0.01,
		NormalPhoto:     0.90,
		EverydayScene:   0.04,
	}
}

func rejectGoreValues() goreScores {
	return goreScores{
		GraphicViolence: 0.60,
		Gore:            0.20,
		Blood:           0.10,
		NormalPhoto:     0.05,
		EverydayScene:   0.05,
	}
}

func invalidNSFWValues() nsfwScores {
	return nsfwScores{Neutral: 0.5, Drawing: 0.1, Sexy: 0.1, Hentai: 0.1, Porn: 0.1}
}

func invalidGoreValues() goreScores {
	return goreScores{
		GraphicViolence: 0.1,
		Gore:            0.1,
		Blood:           0.1,
		NormalPhoto:     0.1,
		EverydayScene:   0.1,
	}
}

func cleanNSFW(t *testing.T) *sidecarFake {
	return newSidecarFake(t, 0, sidecarBody(t, cleanNSFWValues()))
}

func cleanGore(t *testing.T) *sidecarFake {
	return newSidecarFake(t, 0, sidecarBody(t, cleanGoreValues()))
}

func upload(t *testing.T, env *uploadTestEnv, token, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create multipart field: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write multipart field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/uploads", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	env.handler.ServeHTTP(recorder, request)
	return recorder
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buffer.Bytes()
}

type uploadResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type imageRecord struct {
	Status string
	Scores combinedScores
}

type auditRecord struct {
	ID     string
	Status string
	Scores combinedScores
	Reason string
}

type quarantineRecord struct {
	ID          string
	UserID      int64
	Filename    string
	ContentType string
	Size        int64
	MatchedBy   string
}

func decodeResponse[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return value
}

func assertJSONKeys(t *testing.T, recorder *httptest.ResponseRecorder, keys ...string) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response object %q: %v", recorder.Body.String(), err)
	}
	if len(payload) != len(keys) {
		t.Fatalf("response has %d keys, want %d: %s", len(payload), len(keys), recorder.Body.String())
	}
	for _, key := range keys {
		if _, ok := payload[key]; !ok {
			t.Fatalf("response is missing key %q: %s", key, recorder.Body.String())
		}
	}
}

func assertError(t *testing.T, recorder *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, status, recorder.Body.String())
	}
	got := decodeResponse[errorResponse](t, recorder)
	if got.Error != message {
		t.Fatalf("error = %q, want %q", got.Error, message)
	}
}

func decodeScores(t *testing.T, raw []byte) combinedScores {
	t.Helper()
	var scores combinedScores
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &scores); err != nil {
			t.Fatalf("decode moderation scores %q: %v", raw, err)
		}
	}
	return scores
}

func queryImage(t *testing.T, id string) imageRecord {
	t.Helper()
	var status string
	var raw []byte
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT moderation_status, moderation_scores FROM uploads.images WHERE id = $1`, id,
	).Scan(&status, &raw); err != nil {
		t.Fatalf("query image %s: %v", id, err)
	}
	return imageRecord{Status: status, Scores: decodeScores(t, raw)}
}

func queryAudit(t *testing.T) auditRecord {
	t.Helper()
	var record auditRecord
	var raw []byte
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT id, moderation_status, moderation_scores, reason
		 FROM uploads.moderation_audit ORDER BY created_at DESC LIMIT 1`,
	).Scan(&record.ID, &record.Status, &raw, &record.Reason); err != nil {
		t.Fatalf("query moderation audit: %v", err)
	}
	record.Scores = decodeScores(t, raw)
	return record
}

func queryQuarantine(t *testing.T) quarantineRecord {
	t.Helper()
	var record quarantineRecord
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT id, user_id, filename, content_type, size, matched_by
		 FROM uploads.quarantine ORDER BY created_at DESC LIMIT 1`,
	).Scan(&record.ID, &record.UserID, &record.Filename, &record.ContentType,
		&record.Size, &record.MatchedBy); err != nil {
		t.Fatalf("query quarantine: %v", err)
	}
	return record
}

func countImages(t *testing.T) int {
	t.Helper()
	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM uploads.images`).Scan(&count); err != nil {
		t.Fatalf("count images: %v", err)
	}
	return count
}

func countAudit(t *testing.T) int {
	t.Helper()
	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM uploads.moderation_audit`).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return count
}

func countQuarantine(t *testing.T) int {
	t.Helper()
	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM uploads.quarantine`).Scan(&count); err != nil {
		t.Fatalf("count quarantine rows: %v", err)
	}
	return count
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("dir %s has %d entries, want empty", dir, len(entries))
	}
}

func assertNoQuarantineDir(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("quarantine dir %s exists or stat failed: %v", dir, err)
	}
}

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file %s does not match uploaded bytes", path)
	}
}

func TestUploadSuccess(t *testing.T) {
	env := newUploadTestEnv(t, cleanNSFW(t), cleanGore(t), nil, false)
	token := tokenFor(t, env.tokens, 7, "ada")
	data := testPNG(t)

	recorder := upload(t, env, token, "avatar.png", data)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	assertJSONKeys(t, recorder, "id", "url")
	got := decodeResponse[uploadResponse](t, recorder)
	if !idPattern.MatchString(got.ID) {
		t.Fatalf("id = %q, want 32 hex characters", got.ID)
	}
	if got.URL != "/api/uploads/"+got.ID {
		t.Fatalf("url = %q", got.URL)
	}

	image := queryImage(t, got.ID)
	if image.Status != "approved" {
		t.Fatalf("status = %q, want approved", image.Status)
	}
	cleanNSFWScore := cleanNSFWValues()
	cleanGoreScore := cleanGoreValues()
	if image.Scores.NSFW == nil || *image.Scores.NSFW != cleanNSFWScore {
		t.Fatalf("nsfw scores = %+v, want %+v", image.Scores.NSFW, cleanNSFWScore)
	}
	if image.Scores.Gore == nil || *image.Scores.Gore != cleanGoreScore {
		t.Fatalf("gore scores = %+v, want %+v", image.Scores.Gore, cleanGoreScore)
	}

	assertFileEquals(t, filepath.Join(env.uploadDir, got.ID), data)
	assertNoQuarantineDir(t, env.quarantineDir)
	if env.nsfw.calls.Load() != 1 || env.gore.calls.Load() != 1 {
		t.Fatalf("classifier calls nsfw=%d gore=%d, want 1/1", env.nsfw.calls.Load(), env.gore.calls.Load())
	}
}

func TestUploadNSFWRejection(t *testing.T) {
	env := newUploadTestEnv(t, newSidecarFake(t, 0, sidecarBody(t, rejectNSFWValues())), cleanGore(t), nil, false)
	token := tokenFor(t, env.tokens, 7, "ada")

	recorder := upload(t, env, token, "rejected.png", testPNG(t))
	assertError(t, recorder, http.StatusUnavailableForLegalReasons, "image rejected by content moderation")
	assertDirEmpty(t, env.uploadDir)
	assertNoQuarantineDir(t, env.quarantineDir)
	if countImages(t) != 0 || countAudit(t) != 1 {
		t.Fatalf("images=%d audit=%d, want 0/1", countImages(t), countAudit(t))
	}

	audit := queryAudit(t)
	if audit.Status != "rejected" {
		t.Fatalf("audit status = %q", audit.Status)
	}
	if !strings.Contains(audit.Reason, "nsfw: porn+hentai") {
		t.Fatalf("audit reason = %q", audit.Reason)
	}
	if audit.Scores.NSFW == nil || audit.Scores.Gore == nil {
		t.Fatalf("audit scores = %+v", audit.Scores)
	}
}

func TestUploadGoreRejection(t *testing.T) {
	env := newUploadTestEnv(t, cleanNSFW(t), newSidecarFake(t, 0, sidecarBody(t, rejectGoreValues())), nil, false)
	token := tokenFor(t, env.tokens, 7, "ada")

	recorder := upload(t, env, token, "rejected.png", testPNG(t))
	assertError(t, recorder, http.StatusUnavailableForLegalReasons, "image rejected by content moderation")
	assertDirEmpty(t, env.uploadDir)
	assertNoQuarantineDir(t, env.quarantineDir)
	if countImages(t) != 0 || countAudit(t) != 1 {
		t.Fatalf("images=%d audit=%d, want 0/1", countImages(t), countAudit(t))
	}

	audit := queryAudit(t)
	if audit.Status != "rejected" {
		t.Fatalf("audit status = %q", audit.Status)
	}
	if !strings.Contains(audit.Reason, "gore: graphic_violence+gore+blood") {
		t.Fatalf("audit reason = %q", audit.Reason)
	}
	if audit.Scores.NSFW == nil || audit.Scores.Gore == nil {
		t.Fatalf("audit scores = %+v", audit.Scores)
	}
}

func TestUploadFailClosed(t *testing.T) {
	t.Run("nsfw_down", func(t *testing.T) {
		env := newUploadTestEnv(t, newSidecarFake(t, http.StatusInternalServerError, ""), cleanGore(t), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "fail.png", testPNG(t))
		assertError(t, recorder, http.StatusServiceUnavailable, "image moderation is unavailable; try again later")
		assertDirEmpty(t, env.uploadDir)
		assertNoQuarantineDir(t, env.quarantineDir)
		if countImages(t) != 0 || countAudit(t) != 1 {
			t.Fatalf("images=%d audit=%d", countImages(t), countAudit(t))
		}
		audit := queryAudit(t)
		if audit.Status != "error" || !strings.Contains(audit.Reason, "nsfw: classifier error") {
			t.Fatalf("audit = %+v", audit)
		}
		if audit.Scores.NSFW != nil || audit.Scores.Gore == nil {
			t.Fatalf("audit scores = %+v", audit.Scores)
		}
	})

	t.Run("gore_down", func(t *testing.T) {
		env := newUploadTestEnv(t, cleanNSFW(t), newSidecarFake(t, http.StatusInternalServerError, ""), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "fail.png", testPNG(t))
		assertError(t, recorder, http.StatusServiceUnavailable, "image moderation is unavailable; try again later")
		assertDirEmpty(t, env.uploadDir)
		assertNoQuarantineDir(t, env.quarantineDir)
		if countImages(t) != 0 || countAudit(t) != 1 {
			t.Fatalf("images=%d audit=%d", countImages(t), countAudit(t))
		}
		audit := queryAudit(t)
		if audit.Status != "error" || !strings.Contains(audit.Reason, "gore: classifier error") {
			t.Fatalf("audit = %+v", audit)
		}
		if audit.Scores.NSFW == nil || audit.Scores.Gore != nil {
			t.Fatalf("audit scores = %+v", audit.Scores)
		}
	})
}

func TestUploadFailOpen(t *testing.T) {
	t.Run("nsfw_down", func(t *testing.T) {
		env := newUploadTestEnv(t, newSidecarFake(t, http.StatusInternalServerError, ""), cleanGore(t), nil, true)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "open.png", testPNG(t))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
		}
		got := decodeResponse[uploadResponse](t, recorder)
		image := queryImage(t, got.ID)
		if image.Status != "unavailable" {
			t.Fatalf("status = %q, want unavailable", image.Status)
		}
		if image.Scores.NSFW != nil || image.Scores.Gore == nil {
			t.Fatalf("scores = %+v", image.Scores)
		}
		assertFileEquals(t, filepath.Join(env.uploadDir, got.ID), testPNG(t))
		if countAudit(t) != 0 {
			t.Fatalf("audit rows = %d, want 0", countAudit(t))
		}
	})

	t.Run("gore_down", func(t *testing.T) {
		env := newUploadTestEnv(t, cleanNSFW(t), newSidecarFake(t, http.StatusInternalServerError, ""), nil, true)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "open.png", testPNG(t))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
		}
		got := decodeResponse[uploadResponse](t, recorder)
		image := queryImage(t, got.ID)
		if image.Status != "unavailable" {
			t.Fatalf("status = %q, want unavailable", image.Status)
		}
		if image.Scores.NSFW == nil || image.Scores.Gore != nil {
			t.Fatalf("scores = %+v", image.Scores)
		}
		assertFileEquals(t, filepath.Join(env.uploadDir, got.ID), testPNG(t))
	})
}

func TestUploadInvalidClassifierResponse(t *testing.T) {
	t.Run("nsfw_invalid_fail_closed", func(t *testing.T) {
		env := newUploadTestEnv(t, newSidecarFake(t, 0, sidecarBody(t, invalidNSFWValues())), cleanGore(t), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "invalid.png", testPNG(t))
		assertError(t, recorder, http.StatusServiceUnavailable, "image moderation is unavailable; try again later")
		if countImages(t) != 0 || countAudit(t) != 1 {
			t.Fatalf("images=%d audit=%d", countImages(t), countAudit(t))
		}
		audit := queryAudit(t)
		if audit.Status != "error" || !strings.Contains(audit.Reason, "invalid probabilities") {
			t.Fatalf("audit = %+v", audit)
		}
	})

	t.Run("gore_invalid_fail_closed", func(t *testing.T) {
		env := newUploadTestEnv(t, cleanNSFW(t), newSidecarFake(t, 0, sidecarBody(t, invalidGoreValues())), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "invalid.png", testPNG(t))
		assertError(t, recorder, http.StatusServiceUnavailable, "image moderation is unavailable; try again later")
		if countImages(t) != 0 || countAudit(t) != 1 {
			t.Fatalf("images=%d audit=%d", countImages(t), countAudit(t))
		}
		audit := queryAudit(t)
		if audit.Status != "error" || !strings.Contains(audit.Reason, "invalid probabilities") {
			t.Fatalf("audit = %+v", audit)
		}
	})

	t.Run("nsfw_invalid_fail_open", func(t *testing.T) {
		env := newUploadTestEnv(t, newSidecarFake(t, 0, sidecarBody(t, invalidNSFWValues())), cleanGore(t), nil, true)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "invalid.png", testPNG(t))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
		}
		got := decodeResponse[uploadResponse](t, recorder)
		image := queryImage(t, got.ID)
		if image.Status != "unavailable" || image.Scores.NSFW != nil || image.Scores.Gore == nil {
			t.Fatalf("image = %+v", image)
		}
		assertFileEquals(t, filepath.Join(env.uploadDir, got.ID), testPNG(t))
	})
}

func TestUploadHashMatchQuarantine(t *testing.T) {
	matcher := &fakeHashMatcher{matched: true}
	env := newUploadTestEnv(t, cleanNSFW(t), cleanGore(t), matcher, false)
	token := tokenFor(t, env.tokens, 7, "ada")
	data := testPNG(t)

	recorder := upload(t, env, token, "quarantine.png", data)
	assertError(t, recorder, http.StatusUnavailableForLegalReasons, "image rejected by content moderation")
	assertDirEmpty(t, env.uploadDir)
	if countImages(t) != 0 || countAudit(t) != 0 || countQuarantine(t) != 1 {
		t.Fatalf("images=%d audit=%d quarantine=%d", countImages(t), countAudit(t), countQuarantine(t))
	}
	if matcher.calls.Load() != 1 {
		t.Fatalf("hash matcher calls = %d, want 1", matcher.calls.Load())
	}

	record := queryQuarantine(t)
	if record.UserID != 7 || record.Filename != "quarantine.png" || record.ContentType != "image/png" {
		t.Fatalf("quarantine record = %+v", record)
	}
	if record.Size != int64(len(data)) || record.MatchedBy != hashMatcherName {
		t.Fatalf("quarantine record = %+v", record)
	}
	assertFileEquals(t, filepath.Join(env.quarantineDir, record.ID), data)

	dirInfo, err := os.Stat(env.quarantineDir)
	if err != nil {
		t.Fatalf("stat quarantine dir: %v", err)
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("quarantine dir permissions = %o", dirInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(filepath.Join(env.quarantineDir, record.ID))
	if err != nil {
		t.Fatalf("stat quarantine file: %v", err)
	}
	if fileInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("quarantine file permissions = %o", fileInfo.Mode().Perm())
	}
}

func TestUploadBasicValidation(t *testing.T) {
	t.Run("non_image", func(t *testing.T) {
		env := newUploadTestEnv(t, cleanNSFW(t), cleanGore(t), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "notes.txt", []byte("this is not an image"))
		assertError(t, recorder, http.StatusUnsupportedMediaType, "uploaded file is not a supported image")
		if env.nsfw.calls.Load() != 0 || env.gore.calls.Load() != 0 || env.hash.calls.Load() != 0 {
			t.Fatalf("moderation was called for a non-image: nsfw=%d gore=%d hash=%d",
				env.nsfw.calls.Load(), env.gore.calls.Load(), env.hash.calls.Load())
		}
		assertDirEmpty(t, env.uploadDir)
		if countImages(t) != 0 || countAudit(t) != 0 || countQuarantine(t) != 0 {
			t.Fatalf("rows created for a non-image")
		}
	})

	t.Run("too_large", func(t *testing.T) {
		env := newUploadTestEnv(t, cleanNSFW(t), cleanGore(t), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		data := bytes.Repeat([]byte{0xff}, maxUploadSize+1)
		recorder := upload(t, env, token, "huge.png", data)
		assertError(t, recorder, http.StatusRequestEntityTooLarge, "image must be at most 10 MiB")
		if env.nsfw.calls.Load() != 0 || env.gore.calls.Load() != 0 || env.hash.calls.Load() != 0 {
			t.Fatalf("moderation was called for an oversized image")
		}
		assertDirEmpty(t, env.uploadDir)
	})

	t.Run("empty", func(t *testing.T) {
		env := newUploadTestEnv(t, cleanNSFW(t), cleanGore(t), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		recorder := upload(t, env, token, "empty.png", nil)
		assertError(t, recorder, http.StatusBadRequest, "uploaded file is empty")
		if env.nsfw.calls.Load() != 0 || env.gore.calls.Load() != 0 || env.hash.calls.Load() != 0 {
			t.Fatalf("moderation was called for an empty image")
		}
	})
}

func TestGetImage(t *testing.T) {
	t.Run("stored_image_served", func(t *testing.T) {
		env := newUploadTestEnv(t, cleanNSFW(t), cleanGore(t), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		data := testPNG(t)
		recorder := upload(t, env, token, "serve.png", data)
		got := decodeResponse[uploadResponse](t, recorder)

		response := httptest.NewRecorder()
		env.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/uploads/"+got.ID, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
		}
		if contentType := response.Header().Get("Content-Type"); contentType != "image/png" {
			t.Fatalf("content type = %q", contentType)
		}
		if !bytes.Equal(response.Body.Bytes(), data) {
			t.Fatal("served image bytes differ")
		}
	})

	t.Run("rejected_not_found", func(t *testing.T) {
		env := newUploadTestEnv(t, newSidecarFake(t, 0, sidecarBody(t, rejectNSFWValues())), cleanGore(t), nil, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		upload(t, env, token, "rejected.png", testPNG(t))
		audit := queryAudit(t)

		response := httptest.NewRecorder()
		env.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/uploads/"+audit.ID, nil))
		assertError(t, response, http.StatusNotFound, "image not found")
	})

	t.Run("quarantined_not_found", func(t *testing.T) {
		env := newUploadTestEnv(t, cleanNSFW(t), cleanGore(t), &fakeHashMatcher{matched: true}, false)
		token := tokenFor(t, env.tokens, 7, "ada")
		upload(t, env, token, "quarantine.png", testPNG(t))
		record := queryQuarantine(t)

		response := httptest.NewRecorder()
		env.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/uploads/"+record.ID, nil))
		assertError(t, response, http.StatusNotFound, "image not found")
	})
}
