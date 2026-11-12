package main

// Test infrastructure: the suite starts postgres:16-alpine with the Docker CLI
// on a random loopback port, waits with db.Connect, applies the same
// adsMigrations as run(), and removes the container in TestMain.
//
// Handlers are driven with httptest; no real service ports are bound.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"bario/pkg/authclient"
	"bario/pkg/db"
	bariohttp "bario/pkg/http"
	"bario/pkg/jwt"
	"bario/pkg/metrics"
)

const (
	testPostgresImage = "postgres:16-alpine"
	testDBPassword    = "bario-test-password"
	testJWTSecret     = "bario-ads-tests-secret"
)

var testPool *sql.DB

func TestMain(m *testing.M) {
	container, dsn, err := startTestPostgres()
	if err != nil {
		log.Fatalf("ads tests: %v", err)
	}
	cleanup := func() {
		if output, err := exec.Command("docker", "rm", "-f", container).CombinedOutput(); err != nil {
			log.Printf("ads tests: remove %s: %v (%s)", container, err, strings.TrimSpace(string(output)))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	pool, err := db.Connect(ctx, dsn, "ads")
	cancel()
	if err != nil {
		cleanup()
		log.Fatalf("ads tests: connect to postgres: %v", err)
	}
	if err := db.Migrate(context.Background(), pool, adsMigrations...); err != nil {
		pool.Close()
		cleanup()
		log.Fatalf("ads tests: apply migrations: %v", err)
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
		return "", "", fmt.Errorf("docker CLI is required to run ads tests")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("allocate postgres port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	container := fmt.Sprintf("bario-ads-test-%d", time.Now().UnixNano())
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

func resetAds(t *testing.T) {
	t.Helper()
	if _, err := testPool.ExecContext(context.Background(),
		`TRUNCATE ads.advertisements, ads.impressions, ads.clicks RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset ads tables: %v", err)
	}
}

func newTestAPI(t *testing.T) (http.Handler, *jwt.Manager) {
	return newTestAPIWithAuth(t, nil, http.StatusOK)
}

func newTestAPIWithAuth(t *testing.T, interests []string, authStatus int) (http.Handler, *jwt.Manager) {
	t.Helper()
	resetAds(t)

	tokens, err := jwt.NewManager(testJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("new jwt manager: %v", err)
	}

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authStatus != http.StatusOK {
			http.Error(w, "auth unavailable", authStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authclient.User{
			ID:        1,
			Username:  "caller",
			Interests: interests,
		})
	}))
	t.Cleanup(authServer.Close)

	srv := &server{db: testPool, tokens: tokens, auth: authclient.New(authServer.URL)}
	router := bariohttp.NewRouter(metrics.New("ads"))
	srv.registerRoutes(router)
	return router.Handler(), tokens
}

func tokenFor(t *testing.T, tokens *jwt.Manager, userID int64, username string) string {
	t.Helper()
	token, err := tokens.Issue(userID, username)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

type adInput struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	TargetTags  []string `json:"target_tags"`
	BudgetCents int64    `json:"budget_cents"`
}

type adResponse struct {
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

type servedAdResponse struct {
	ID           int64  `json:"id"`
	AdvertiserID int64  `json:"advertiser_id"`
	Title        string `json:"title"`
	Body         string `json:"body"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func requestJSON(t *testing.T, handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeResponse[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return value
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

func assertAdKeys(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode ad object %q: %v", recorder.Body.String(), err)
	}
	keys := []string{
		"id", "advertiser_id", "title", "body", "target_tags",
		"budget_cents", "status", "created_at", "updated_at",
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

func createAd(t *testing.T, handler http.Handler, token string, input adInput) adResponse {
	t.Helper()
	recorder := requestJSON(t, handler, http.MethodPost, "/ads", token, input)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create ad: status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	return decodeResponse[adResponse](t, recorder)
}

func serveAds(t *testing.T, handler http.Handler, token string, pages, limit int) *httptest.ResponseRecorder {
	t.Helper()
	path := fmt.Sprintf("/ads/serve?pages=%d&limit=%d", pages, limit)
	return requestJSON(t, handler, http.MethodGet, path, token, nil)
}

func clickAd(t *testing.T, handler http.Handler, token string, id int64) *httptest.ResponseRecorder {
	t.Helper()
	return requestJSON(t, handler, http.MethodPost, fmt.Sprintf("/ads/%d/click", id), token, nil)
}

func assertServedAdShape(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var payload []map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode served ads %q: %v", recorder.Body.String(), err)
	}
	if len(payload) == 0 {
		t.Fatal("no served ads to inspect")
	}
	ad := payload[0]
	if len(ad) != 4 {
		t.Fatalf("served ad has %d keys, want 4: %s", len(ad), recorder.Body.String())
	}
	for _, key := range []string{"id", "advertiser_id", "title", "body"} {
		if _, ok := ad[key]; !ok {
			t.Fatalf("served ad is missing key %q: %s", key, recorder.Body.String())
		}
	}
}

func impressionAdIDs(t *testing.T, userID int64) []int64 {
	t.Helper()
	rows, err := testPool.QueryContext(context.Background(),
		`SELECT ad_id FROM ads.impressions WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("query impressions: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan impression: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate impressions: %v", err)
	}
	return ids
}

func TestCreateAdSuccess(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 7, "advertiser")
	input := adInput{
		Title:       "Launch week",
		Body:        "Try the new Bario timeline.",
		TargetTags:  []string{"tech", "go"},
		BudgetCents: 5000,
	}

	recorder := requestJSON(t, handler, http.MethodPost, "/ads", token, input)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	assertAdKeys(t, recorder)

	got := decodeResponse[adResponse](t, recorder)
	if got.ID <= 0 {
		t.Fatalf("id = %d", got.ID)
	}
	if got.AdvertiserID != 7 || got.Title != input.Title || got.Body != input.Body {
		t.Fatalf("ad = %+v", got)
	}
	if !slices.Equal(got.TargetTags, input.TargetTags) {
		t.Fatalf("target_tags = %v", got.TargetTags)
	}
	if got.BudgetCents != input.BudgetCents || got.Status != "active" {
		t.Fatalf("budget/status = %d/%q", got.BudgetCents, got.Status)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps = %v / %v", got.CreatedAt, got.UpdatedAt)
	}

	var (
		advertiserID int64
		title        string
		body         string
		tags         []string
		budget       int64
		status       string
	)
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT advertiser_id, title, body, target_tags, budget_cents, status
		 FROM ads.advertisements WHERE id = $1`, got.ID,
	).Scan(&advertiserID, &title, &body, pq.Array(&tags), &budget, &status); err != nil {
		t.Fatalf("query advertisement: %v", err)
	}
	if advertiserID != 7 || title != input.Title || body != input.Body || !slices.Equal(tags, input.TargetTags) || budget != input.BudgetCents || status != "active" {
		t.Fatalf("stored ad = %d %q %q %v %d %q", advertiserID, title, body, tags, budget, status)
	}
}

func TestCreateAdEmptyFields(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 7, "advertiser")
	base := adInput{Title: "Title", Body: "Body", BudgetCents: 1}

	cases := []struct {
		name  string
		input adInput
		want  string
	}{
		{"empty_title", adInput{Title: "", Body: base.Body, BudgetCents: 1}, "title must not be empty"},
		{"whitespace_title", adInput{Title: "   ", Body: base.Body, BudgetCents: 1}, "title must not be empty"},
		{"empty_body", adInput{Title: base.Title, Body: "", BudgetCents: 1}, "body must not be empty"},
		{"whitespace_body", adInput{Title: base.Title, Body: "   ", BudgetCents: 1}, "body must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := requestJSON(t, handler, http.MethodPost, "/ads", token, tc.input)
			assertError(t, recorder, http.StatusBadRequest, tc.want)
		})
	}
}

func TestCreateAdLengthLimits(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 7, "advertiser")

	t.Run("title_too_long", func(t *testing.T) {
		recorder := requestJSON(t, handler, http.MethodPost, "/ads", token, adInput{
			Title: strings.Repeat("t", maxTitleLength+1), Body: "Body", BudgetCents: 1,
		})
		assertError(t, recorder, http.StatusBadRequest, "title must be at most 120 characters")
	})

	t.Run("body_too_long", func(t *testing.T) {
		recorder := requestJSON(t, handler, http.MethodPost, "/ads", token, adInput{
			Title: "Title", Body: strings.Repeat("b", maxBodyLength+1), BudgetCents: 1,
		})
		assertError(t, recorder, http.StatusBadRequest, "body must be at most 2000 characters")
	})

	t.Run("at_limits", func(t *testing.T) {
		input := adInput{
			Title:       strings.Repeat("t", maxTitleLength),
			Body:        strings.Repeat("b", maxBodyLength),
			BudgetCents: 1,
		}
		got := createAd(t, handler, token, input)
		if len([]rune(got.Title)) != maxTitleLength || len([]rune(got.Body)) != maxBodyLength {
			t.Fatalf("stored lengths = %d/%d", len([]rune(got.Title)), len([]rune(got.Body)))
		}
	})
}

func TestCreateAdNegativeBudget(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 7, "advertiser")

	recorder := requestJSON(t, handler, http.MethodPost, "/ads", token, adInput{
		Title: "Title", Body: "Body", BudgetCents: -1,
	})
	assertError(t, recorder, http.StatusBadRequest, "budget_cents must be zero or greater")
}

func TestCreateAdEmptyTargetTags(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 7, "advertiser")

	recorder := requestJSON(t, handler, http.MethodPost, "/ads", token, adInput{
		Title: "Title", Body: "Body", TargetTags: nil, BudgetCents: 0,
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	got := decodeResponse[adResponse](t, recorder)
	if got.TargetTags == nil || len(got.TargetTags) != 0 {
		t.Fatalf("target_tags = %v, want []", got.TargetTags)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(payload["target_tags"]) != "[]" {
		t.Fatalf("target_tags JSON = %s, want []", payload["target_tags"])
	}
}

func TestAdsUnauthenticated(t *testing.T) {
	handler, _ := newTestAPI(t)

	t.Run("create", func(t *testing.T) {
		recorder := requestJSON(t, handler, http.MethodPost, "/ads", "", adInput{
			Title: "Title", Body: "Body", BudgetCents: 1,
		})
		assertError(t, recorder, http.StatusUnauthorized, "authentication required")
	})

	t.Run("list", func(t *testing.T) {
		recorder := requestJSON(t, handler, http.MethodGet, "/ads", "", nil)
		assertError(t, recorder, http.StatusUnauthorized, "authentication required")
	})
}

func TestListAdsOnlyCaller(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token1 := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")

	first := createAd(t, handler, token1, adInput{Title: "First", Body: "one", TargetTags: []string{"a"}, BudgetCents: 100})
	second := createAd(t, handler, token1, adInput{Title: "Second", Body: "two", TargetTags: []string{"b"}, BudgetCents: 200})
	other := createAd(t, handler, token2, adInput{Title: "Other", Body: "three", TargetTags: []string{"c"}, BudgetCents: 300})

	recorder := requestJSON(t, handler, http.MethodGet, "/ads", token1, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	ads := decodeResponse[[]adResponse](t, recorder)
	if len(ads) != 2 {
		t.Fatalf("ads = %d, want 2", len(ads))
	}
	if ads[0].ID != second.ID || ads[1].ID != first.ID {
		t.Fatalf("ids = %d,%d, want %d,%d", ads[0].ID, ads[1].ID, second.ID, first.ID)
	}
	for _, a := range ads {
		if a.AdvertiserID != 1 || a.ID == other.ID {
			t.Fatalf("caller saw another advertiser's ad: %+v", a)
		}
	}

	otherRecorder := requestJSON(t, handler, http.MethodGet, "/ads", token2, nil)
	otherAds := decodeResponse[[]adResponse](t, otherRecorder)
	if len(otherAds) != 1 || otherAds[0].ID != other.ID {
		t.Fatalf("other advertiser ads = %+v", otherAds)
	}
}

func TestListAdsEmpty(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 99, "nobody")

	recorder := requestJSON(t, handler, http.MethodGet, "/ads", token, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("body = %s, want []", recorder.Body.String())
	}
}

func TestSchemaTablesExist(t *testing.T) {
	for _, table := range []string{"advertisements", "impressions", "clicks"} {
		t.Run(table, func(t *testing.T) {
			var exists bool
			if err := testPool.QueryRowContext(context.Background(),
				`SELECT EXISTS (
					SELECT 1 FROM information_schema.tables
					WHERE table_schema = 'ads' AND table_name = $1
				)`, table).Scan(&exists); err != nil {
				t.Fatalf("query table %s: %v", table, err)
			}
			if !exists {
				t.Fatalf("table ads.%s does not exist", table)
			}
		})
	}
}

func patchAd(t *testing.T, handler http.Handler, token string, id int64, status string) *httptest.ResponseRecorder {
	t.Helper()
	return requestJSON(t, handler, http.MethodPatch, fmt.Sprintf("/ads/%d", id), token,
		map[string]string{"status": status})
}

func TestUpdateAdStatus(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 7, "advertiser")
	created := createAd(t, handler, token, adInput{
		Title: "Toggle", Body: "Status test", TargetTags: []string{"tech"}, BudgetCents: 100,
	})

	time.Sleep(2 * time.Millisecond)
	pausedRecorder := patchAd(t, handler, token, created.ID, "paused")
	if pausedRecorder.Code != http.StatusOK {
		t.Fatalf("pause status = %d, want %d; body = %s",
			pausedRecorder.Code, http.StatusOK, pausedRecorder.Body.String())
	}
	assertAdKeys(t, pausedRecorder)
	paused := decodeResponse[adResponse](t, pausedRecorder)
	if paused.ID != created.ID || paused.Status != "paused" {
		t.Fatalf("paused ad = %+v", paused)
	}
	if !paused.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created_at changed: %v -> %v", created.CreatedAt, paused.CreatedAt)
	}
	if !paused.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("updated_at did not increase: %v -> %v", created.UpdatedAt, paused.UpdatedAt)
	}

	time.Sleep(2 * time.Millisecond)
	activeRecorder := patchAd(t, handler, token, created.ID, "active")
	if activeRecorder.Code != http.StatusOK {
		t.Fatalf("activate status = %d, want %d; body = %s",
			activeRecorder.Code, http.StatusOK, activeRecorder.Body.String())
	}
	active := decodeResponse[adResponse](t, activeRecorder)
	if active.Status != "active" {
		t.Fatalf("active ad status = %q", active.Status)
	}
	if !active.UpdatedAt.After(paused.UpdatedAt) {
		t.Fatalf("updated_at did not increase again: %v -> %v", paused.UpdatedAt, active.UpdatedAt)
	}

	var dbStatus string
	var dbUpdated time.Time
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT status, updated_at FROM ads.advertisements WHERE id = $1`, created.ID,
	).Scan(&dbStatus, &dbUpdated); err != nil {
		t.Fatalf("query updated ad: %v", err)
	}
	if dbStatus != "active" || !dbUpdated.Equal(active.UpdatedAt) {
		t.Fatalf("stored status/updated_at = %q/%v", dbStatus, dbUpdated)
	}
}

func TestUpdateAdInvalidStatus(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 7, "advertiser")
	created := createAd(t, handler, token, adInput{
		Title: "Invalid", Body: "Status test", BudgetCents: 100,
	})

	cases := []struct {
		name   string
		status string
	}{
		{"archived", "archived"},
		{"empty", ""},
		{"uppercase", "ACTIVE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := patchAd(t, handler, token, created.ID, tc.status)
			assertError(t, recorder, http.StatusBadRequest, `status must be "active" or "paused"`)
		})
	}

	var dbStatus string
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT status FROM ads.advertisements WHERE id = $1`, created.ID,
	).Scan(&dbStatus); err != nil {
		t.Fatalf("query ad status: %v", err)
	}
	if dbStatus != "active" {
		t.Fatalf("invalid update changed status to %q", dbStatus)
	}
}

func TestUpdateAdOtherOwner(t *testing.T) {
	handler, tokens := newTestAPI(t)
	ownerToken := tokenFor(t, tokens, 1, "alice")
	otherToken := tokenFor(t, tokens, 2, "bob")
	created := createAd(t, handler, ownerToken, adInput{
		Title: "Owned", Body: "Status test", BudgetCents: 100,
	})

	recorder := patchAd(t, handler, otherToken, created.ID, "paused")
	assertError(t, recorder, http.StatusNotFound, "ad not found")

	var dbStatus string
	var dbUpdated time.Time
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT status, updated_at FROM ads.advertisements WHERE id = $1`, created.ID,
	).Scan(&dbStatus, &dbUpdated); err != nil {
		t.Fatalf("query ad: %v", err)
	}
	if dbStatus != "active" {
		t.Fatalf("other owner changed status to %q", dbStatus)
	}
	if !dbUpdated.Equal(created.UpdatedAt) {
		t.Fatalf("other owner touched updated_at: %v -> %v", created.UpdatedAt, dbUpdated)
	}
}

func TestUpdateAdNotFound(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 1, "alice")

	recorder := patchAd(t, handler, token, 999999, "paused")
	assertError(t, recorder, http.StatusNotFound, "ad not found")
}

func TestUpdateAdUnauthenticated(t *testing.T) {
	handler, tokens := newTestAPI(t)
	token := tokenFor(t, tokens, 1, "alice")
	created := createAd(t, handler, token, adInput{
		Title: "Auth", Body: "Status test", BudgetCents: 100,
	})

	recorder := patchAd(t, handler, "", created.ID, "paused")
	assertError(t, recorder, http.StatusUnauthorized, "authentication required")
}

func TestServeAdsRatio(t *testing.T) {
	handler, tokens := newTestAPI(t)
	advertiser := tokenFor(t, tokens, 2, "advertiser")
	for i := 0; i < 10; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("general-%02d", i), Body: "general", BudgetCents: 1,
		})
	}
	caller := tokenFor(t, tokens, 1, "caller")

	cases := []struct {
		name  string
		pages int
		limit int
		want  int
	}{
		{"zero_pages", 0, 20, 1},
		{"five_pages", 5, 20, 3},
		{"hundred_pages_capped", 100, 20, 6},
		{"negative_pages_clamped", -5, 20, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveAds(t, handler, caller, tc.pages, tc.limit)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			ads := decodeResponse[[]servedAdResponse](t, recorder)
			if len(ads) != tc.want {
				t.Fatalf("ads = %d, want %d", len(ads), tc.want)
			}
			if len(ads) > 0 {
				assertServedAdShape(t, recorder)
				for i := 1; i < len(ads); i++ {
					if ads[i-1].ID <= ads[i].ID {
						t.Fatalf("ads are not newest-first: %v", ads)
					}
				}
			}
		})
	}
}

func TestServeAdsTargetedOnlyWhenOverBatch(t *testing.T) {
	handler, tokens := newTestAPIWithAuth(t, []string{"tech"}, http.StatusOK)
	advertiser := tokenFor(t, tokens, 2, "advertiser")
	for i := 0; i < 5; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("targeted-%02d", i), Body: "targeted",
			TargetTags: []string{"tech"}, BudgetCents: 1,
		})
	}
	for i := 0; i < 3; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("general-%02d", i), Body: "general", BudgetCents: 1,
		})
	}
	caller := tokenFor(t, tokens, 1, "caller")

	recorder := serveAds(t, handler, caller, 5, 20) // ad_count = 3
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	ads := decodeResponse[[]servedAdResponse](t, recorder)
	if len(ads) != 3 {
		t.Fatalf("ads = %d, want 3", len(ads))
	}
	for _, ad := range ads {
		if !strings.HasPrefix(ad.Title, "targeted-") {
			t.Fatalf("non-targeted ad returned: %+v", ad)
		}
	}
	if ads[0].ID <= ads[1].ID || ads[1].ID <= ads[2].ID {
		t.Fatalf("targeted ads are not newest-first: %v", ads)
	}
}

func TestServeAdsTargetedThenGeneralFill(t *testing.T) {
	handler, tokens := newTestAPIWithAuth(t, []string{"tech"}, http.StatusOK)
	advertiser := tokenFor(t, tokens, 2, "advertiser")
	for i := 0; i < 2; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("targeted-%02d", i), Body: "targeted",
			TargetTags: []string{"tech"}, BudgetCents: 1,
		})
	}
	for i := 0; i < 5; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("general-%02d", i), Body: "general", BudgetCents: 1,
		})
	}
	caller := tokenFor(t, tokens, 1, "caller")

	ads := decodeResponse[[]servedAdResponse](t, serveAds(t, handler, caller, 5, 20))
	if len(ads) != 3 {
		t.Fatalf("ads = %d, want 3", len(ads))
	}
	if !strings.HasPrefix(ads[0].Title, "targeted-") || !strings.HasPrefix(ads[1].Title, "targeted-") {
		t.Fatalf("targeted ads are not first: %v", ads)
	}
	if !strings.HasPrefix(ads[2].Title, "general-") {
		t.Fatalf("remaining slot was not filled from general: %v", ads)
	}
}

func TestServeAdsGeneralOnlyWithoutInterests(t *testing.T) {
	handler, tokens := newTestAPIWithAuth(t, nil, http.StatusOK)
	advertiser := tokenFor(t, tokens, 2, "advertiser")
	for i := 0; i < 2; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("targeted-%02d", i), Body: "targeted",
			TargetTags: []string{"tech"}, BudgetCents: 1,
		})
	}
	for i := 0; i < 2; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("general-%02d", i), Body: "general", BudgetCents: 1,
		})
	}
	caller := tokenFor(t, tokens, 1, "caller")

	ads := decodeResponse[[]servedAdResponse](t, serveAds(t, handler, caller, 5, 20))
	if len(ads) != 2 {
		t.Fatalf("ads = %d, want 2", len(ads))
	}
	for _, ad := range ads {
		if !strings.HasPrefix(ad.Title, "general-") {
			t.Fatalf("targeted ad served without interests: %+v", ad)
		}
	}
}

func TestServeAdsGeneralOnlyWhenNoOverlap(t *testing.T) {
	handler, tokens := newTestAPIWithAuth(t, []string{"sports"}, http.StatusOK)
	advertiser := tokenFor(t, tokens, 2, "advertiser")
	for i := 0; i < 2; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("targeted-%02d", i), Body: "targeted",
			TargetTags: []string{"tech"}, BudgetCents: 1,
		})
	}
	for i := 0; i < 2; i++ {
		createAd(t, handler, advertiser, adInput{
			Title: fmt.Sprintf("general-%02d", i), Body: "general", BudgetCents: 1,
		})
	}
	caller := tokenFor(t, tokens, 1, "caller")

	ads := decodeResponse[[]servedAdResponse](t, serveAds(t, handler, caller, 5, 20))
	if len(ads) != 2 {
		t.Fatalf("ads = %d, want 2", len(ads))
	}
	for _, ad := range ads {
		if !strings.HasPrefix(ad.Title, "general-") {
			t.Fatalf("non-overlapping targeted ad served: %+v", ad)
		}
	}
}

func TestServeAdsEmpty(t *testing.T) {
	handler, tokens := newTestAPI(t)
	caller := tokenFor(t, tokens, 1, "caller")

	recorder := serveAds(t, handler, caller, 5, 20)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("body = %s, want []", recorder.Body.String())
	}
	if got := impressionAdIDs(t, 1); len(got) != 0 {
		t.Fatalf("impressions = %v, want none", got)
	}
}

func TestServeAdsRecordsImpressions(t *testing.T) {
	handler, tokens := newTestAPI(t)
	advertiser := tokenFor(t, tokens, 2, "advertiser")
	first := createAd(t, handler, advertiser, adInput{Title: "general-0", Body: "general", BudgetCents: 1})
	second := createAd(t, handler, advertiser, adInput{Title: "general-1", Body: "general", BudgetCents: 1})
	caller := tokenFor(t, tokens, 1, "caller")

	ads := decodeResponse[[]servedAdResponse](t, serveAds(t, handler, caller, 5, 20))
	if len(ads) != 2 {
		t.Fatalf("ads = %d, want 2", len(ads))
	}
	got := impressionAdIDs(t, 1)
	want := []int64{second.ID, first.ID}
	if !slices.Equal(got, want) {
		t.Fatalf("impression ad ids = %v, want %v", got, want)
	}
}

func TestServeAdsAuthDownDegradesToGeneral(t *testing.T) {
	handler, tokens := newTestAPIWithAuth(t, nil, http.StatusInternalServerError)
	advertiser := tokenFor(t, tokens, 2, "advertiser")
	createAd(t, handler, advertiser, adInput{
		Title: "targeted-0", Body: "targeted", TargetTags: []string{"tech"}, BudgetCents: 1,
	})
	createAd(t, handler, advertiser, adInput{Title: "general-0", Body: "general", BudgetCents: 1})
	caller := tokenFor(t, tokens, 1, "caller")

	recorder := serveAds(t, handler, caller, 5, 20)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	ads := decodeResponse[[]servedAdResponse](t, recorder)
	if len(ads) != 1 || !strings.HasPrefix(ads[0].Title, "general-") {
		t.Fatalf("auth-down ads = %+v, want general-only", ads)
	}
}

func TestClickAdSuccess(t *testing.T) {
	handler, tokens := newTestAPI(t)
	advertiser := tokenFor(t, tokens, 2, "advertiser")
	ad := createAd(t, handler, advertiser, adInput{Title: "Click", Body: "Click me", BudgetCents: 1})
	clicker := tokenFor(t, tokens, 1, "clicker")

	recorder := clickAd(t, handler, clicker, ad.ID)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("click body = %q, want empty", recorder.Body.String())
	}

	var adID, userID int64
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT ad_id, user_id FROM ads.clicks ORDER BY id DESC LIMIT 1`).Scan(&adID, &userID); err != nil {
		t.Fatalf("query click: %v", err)
	}
	if adID != ad.ID || userID != 1 {
		t.Fatalf("click = ad:%d user:%d", adID, userID)
	}

	// Clicking one's own ad is allowed too.
	if recorder := clickAd(t, handler, advertiser, ad.ID); recorder.Code != http.StatusNoContent {
		t.Fatalf("owner click status = %d", recorder.Code)
	}
	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM ads.clicks`).Scan(&count); err != nil {
		t.Fatalf("count clicks: %v", err)
	}
	if count != 2 {
		t.Fatalf("clicks = %d, want 2", count)
	}
}

func TestClickAdNotFound(t *testing.T) {
	handler, tokens := newTestAPI(t)
	clicker := tokenFor(t, tokens, 1, "clicker")

	recorder := clickAd(t, handler, clicker, 999999)
	assertError(t, recorder, http.StatusNotFound, "ad not found")
}

func TestServeAndClickUnauthenticated(t *testing.T) {
	handler, _ := newTestAPI(t)

	t.Run("serve", func(t *testing.T) {
		recorder := requestJSON(t, handler, http.MethodGet, "/ads/serve?limit=20", "", nil)
		assertError(t, recorder, http.StatusUnauthorized, "authentication required")
	})

	t.Run("click", func(t *testing.T) {
		recorder := clickAd(t, handler, "", 1)
		assertError(t, recorder, http.StatusUnauthorized, "authentication required")
	})
}
