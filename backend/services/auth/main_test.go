package main

// Test infrastructure: the suite starts postgres:16-alpine with the Docker CLI
// on a random loopback port, waits for it with db.Connect, applies the same
// authMigrations as run(), and removes the container in TestMain. This keeps
// the auth module free of testcontainers-only dependencies and requires only
// Docker + Go. No manually running stack or fixed port is used.

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
	"strconv"
	"strings"
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
	testJWTSecret     = "bario-auth-tests"
	testPassword      = "correct-horse-battery"
)

var testPool *sql.DB

func TestMain(m *testing.M) {
	container, dsn, err := startTestPostgres()
	if err != nil {
		log.Fatalf("auth tests: %v", err)
	}
	cleanup := func() {
		if output, err := exec.Command("docker", "rm", "-f", container).CombinedOutput(); err != nil {
			log.Printf("auth tests: remove %s: %v (%s)", container, err, strings.TrimSpace(string(output)))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	pool, err := db.Connect(ctx, dsn, "auth")
	cancel()
	if err != nil {
		cleanup()
		log.Fatalf("auth tests: connect to postgres: %v", err)
	}
	if err := db.Migrate(context.Background(), pool, authMigrations...); err != nil {
		pool.Close()
		cleanup()
		log.Fatalf("auth tests: apply migrations: %v", err)
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
		return "", "", fmt.Errorf("docker CLI is required to run auth tests")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("allocate postgres port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	container := fmt.Sprintf("bario-auth-test-%d", time.Now().UnixNano())
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

type registerResponse struct {
	ID        int64    `json:"id"`
	Username  string   `json:"username"`
	IsAdmin   bool     `json:"is_admin"`
	Interests []string `json:"interests"`
}

type userResponse struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	IsAdmin   bool      `json:"is_admin"`
	Interests []string  `json:"interests"`
	CreatedAt time.Time `json:"created_at"`
}

type loginResponse struct {
	Token string       `json:"token"`
	User  userResponse `json:"user"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func resetAuthUsers(t *testing.T) {
	t.Helper()
	if _, err := testPool.ExecContext(context.Background(), `TRUNCATE auth.users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset auth.users: %v", err)
	}
}

func newTestAuth(t *testing.T) (http.Handler, *jwt.Manager) {
	return newTestAuthWithAdmin(t, "admin")
}

func newTestAuthWithAdmin(t *testing.T, adminUsername string) (http.Handler, *jwt.Manager) {
	t.Helper()
	resetAuthUsers(t)

	tokens, err := jwt.NewManager(testJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("new jwt manager: %v", err)
	}
	srv := &server{db: testPool, tokens: tokens, adminUsername: adminUsername}
	router := bariohttp.NewRouter(metrics.New("auth"))
	srv.registerRoutes(router)
	return router.Handler(), tokens
}

func credentials(username, password, email string) map[string]string {
	return map[string]string{
		"username": username,
		"password": password,
		"email":    email,
	}
}

func credentialsWithInterests(username, password, email string, interests []string) map[string]any {
	return map[string]any{
		"username":  username,
		"password":  password,
		"email":     email,
		"interests": interests,
	}
}

func loginBody(username, password string) map[string]string {
	return map[string]string{
		"username": username,
		"password": password,
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(raw)
}

func requestRaw(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return requestRaw(t, handler, method, path, mustJSON(t, body))
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

func registerUser(t *testing.T, handler http.Handler, username, password, email string) registerResponse {
	t.Helper()
	recorder := requestJSON(t, handler, http.MethodPost, "/register", credentials(username, password, email))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register %q: status = %d; body = %s", username, recorder.Code, recorder.Body.String())
	}
	return decodeResponse[registerResponse](t, recorder)
}

func registerUserWithInterests(t *testing.T, handler http.Handler, username, password, email string, interests []string) registerResponse {
	t.Helper()
	recorder := requestJSON(t, handler, http.MethodPost, "/register",
		credentialsWithInterests(username, password, email, interests))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register %q: status = %d; body = %s", username, recorder.Code, recorder.Body.String())
	}
	return decodeResponse[registerResponse](t, recorder)
}

func loginUser(t *testing.T, handler http.Handler, username, password string) loginResponse {
	t.Helper()
	recorder := requestJSON(t, handler, http.MethodPost, "/login", loginBody(username, password))
	if recorder.Code != http.StatusOK {
		t.Fatalf("login %q: status = %d; body = %s", username, recorder.Code, recorder.Body.String())
	}
	return decodeResponse[loginResponse](t, recorder)
}

func TestRegisterSuccess(t *testing.T) {
	handler, _ := newTestAuth(t)

	recorder := requestJSON(t, handler, http.MethodPost, "/register",
		credentials("ada_lovelace", testPassword, "ada@example.com"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	assertJSONKeys(t, recorder, "id", "username", "is_admin", "interests")
	got := decodeResponse[registerResponse](t, recorder)
	if got.ID <= 0 {
		t.Fatalf("id = %d, want positive", got.ID)
	}
	if got.Username != "ada_lovelace" {
		t.Fatalf("username = %q", got.Username)
	}
	if got.IsAdmin {
		t.Fatal("non-reserved username was marked admin")
	}
	if got.Interests == nil || len(got.Interests) != 0 {
		t.Fatalf("interests = %v, want []", got.Interests)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(payload["interests"]) != "[]" {
		t.Fatalf("interests JSON = %s, want []", payload["interests"])
	}

	var hash string
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT password_hash FROM auth.users WHERE id = $1`, got.ID).Scan(&hash); err != nil {
		t.Fatalf("query password hash: %v", err)
	}
	if hash == testPassword || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("password was not stored as a bcrypt hash: %q", hash)
	}
}

func TestRegisterDuplicateUsername(t *testing.T) {
	handler, _ := newTestAuth(t)
	registerUser(t, handler, "ada", testPassword, "ada@example.com")

	recorder := requestJSON(t, handler, http.MethodPost, "/register",
		credentials("ada", testPassword, "other@example.com"))
	assertError(t, recorder, http.StatusConflict, "username is already taken")

	var count int
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM auth.users WHERE username = 'ada'`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("ada rows = %d, want 1", count)
	}
}

func TestRegisterInvalidUsername(t *testing.T) {
	handler, _ := newTestAuth(t)
	cases := []struct {
		name     string
		username string
	}{
		{"too_short", "ab"},
		{"too_long", strings.Repeat("a", 33)},
		{"space", "bad name"},
		{"hyphen", "bad-name"},
		{"symbol", "bad!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := requestJSON(t, handler, http.MethodPost, "/register",
				credentials(tc.username, testPassword, "invalid@example.com"))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if got := decodeResponse[errorResponse](t, recorder); got.Error == "" {
				t.Fatal("expected a non-empty error message")
			}
		})
	}

	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM auth.users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Fatalf("created %d user(s) from invalid usernames", count)
	}
}

func TestRegisterMalformedInput(t *testing.T) {
	handler, _ := newTestAuth(t)

	unknownField := credentials("ada", testPassword, "ada@example.com")
	unknownField["admin"] = "true"

	cases := []struct {
		name string
		body string
	}{
		{"invalid_json", `{`},
		{"missing_all", `{}`},
		{"missing_password_and_email", `{"username":"ada"}`},
		{"missing_password", `{"username":"ada","email":"ada@example.com"}`},
		{"missing_email", mustJSON(t, loginBody("ada", testPassword))},
		{"unknown_field", mustJSON(t, unknownField)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := requestRaw(t, handler, http.MethodPost, "/register", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if got := decodeResponse[errorResponse](t, recorder); got.Error == "" {
				t.Fatal("expected a non-empty error message")
			}
		})
	}
}

func TestRegisterPasswordBounds(t *testing.T) {
	handler, _ := newTestAuth(t)
	cases := []struct {
		name     string
		username string
		password string
		want     int
	}{
		{"seven", "pw_seven", strings.Repeat("p", 7), http.StatusBadRequest},
		{"eight", "pw_eight", strings.Repeat("p", 8), http.StatusCreated},
		{"seventy_two", "pw_72", strings.Repeat("p", 72), http.StatusCreated},
		{"seventy_three", "pw_73", strings.Repeat("p", 73), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := requestJSON(t, handler, http.MethodPost, "/register",
				credentials(tc.username, tc.password, tc.username+"@example.com"))
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}

	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM auth.users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 2 {
		t.Fatalf("created %d users, want 2", count)
	}
}

func TestRegisterAdminFlag(t *testing.T) {
	t.Run("default_admin", func(t *testing.T) {
		handler, _ := newTestAuth(t)
		admin := registerUser(t, handler, "admin", testPassword, "admin@example.com")
		user := registerUser(t, handler, "ada", testPassword, "ada@example.com")

		if !admin.IsAdmin {
			t.Fatal("reserved admin username was not marked admin")
		}
		if user.IsAdmin {
			t.Fatal("non-reserved username was marked admin")
		}

		var storedAdmin, storedUser bool
		if err := testPool.QueryRowContext(context.Background(),
			`SELECT is_admin FROM auth.users WHERE username = 'admin'`).Scan(&storedAdmin); err != nil {
			t.Fatalf("query admin flag: %v", err)
		}
		if err := testPool.QueryRowContext(context.Background(),
			`SELECT is_admin FROM auth.users WHERE username = 'ada'`).Scan(&storedUser); err != nil {
			t.Fatalf("query user flag: %v", err)
		}
		if !storedAdmin || storedUser {
			t.Fatalf("stored flags = admin:%v user:%v", storedAdmin, storedUser)
		}
	})

	t.Run("custom_admin", func(t *testing.T) {
		handler, _ := newTestAuthWithAdmin(t, "moderator")
		moderator := registerUser(t, handler, "moderator", testPassword, "moderator@example.com")
		adminName := registerUser(t, handler, "admin", testPassword, "admin@example.com")

		if !moderator.IsAdmin {
			t.Fatal("custom admin username was not marked admin")
		}
		if adminName.IsAdmin {
			t.Fatal("the default admin name was marked admin when ADMIN_USERNAME is moderator")
		}
	})
}

func TestLoginSuccess(t *testing.T) {
	handler, _ := newTestAuth(t)
	user := registerUser(t, handler, "ada", testPassword, "ada@example.com")

	recorder := requestJSON(t, handler, http.MethodPost, "/login", loginBody("ada", testPassword))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	assertJSONKeys(t, recorder, "token", "user")

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	var userObject map[string]json.RawMessage
	if err := json.Unmarshal(payload["user"], &userObject); err != nil {
		t.Fatalf("decode login user: %v", err)
	}
	for _, key := range []string{"id", "username", "is_admin", "interests"} {
		if _, ok := userObject[key]; !ok {
			t.Fatalf("login user is missing key %q: %s", key, recorder.Body.String())
		}
	}
	if string(userObject["interests"]) != "[]" {
		t.Fatalf("login interests JSON = %s, want []", userObject["interests"])
	}

	got := decodeResponse[loginResponse](t, recorder)
	if got.Token == "" {
		t.Fatal("empty token")
	}
	if parts := strings.Split(got.Token, "."); len(parts) != 3 {
		t.Fatalf("token does not look like a JWT: %q", got.Token)
	}
	if got.User.ID != user.ID || got.User.Username != "ada" {
		t.Fatalf("login user = %+v, want id=%d username=ada", got.User, user.ID)
	}
	if got.User.IsAdmin {
		t.Fatal("non-admin login returned is_admin=true")
	}
	if got.User.Interests == nil || len(got.User.Interests) != 0 {
		t.Fatalf("login interests = %v, want []", got.User.Interests)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	handler, _ := newTestAuth(t)
	registerUser(t, handler, "ada", testPassword, "ada@example.com")

	recorder := requestJSON(t, handler, http.MethodPost, "/login", loginBody("ada", "wrong-password"))
	assertError(t, recorder, http.StatusUnauthorized, "invalid username or password")
}

func TestLoginNonexistentUser(t *testing.T) {
	handler, _ := newTestAuth(t)

	recorder := requestJSON(t, handler, http.MethodPost, "/login", loginBody("ghost", testPassword))
	assertError(t, recorder, http.StatusUnauthorized, "invalid username or password")
}

func TestLoginJWTClaims(t *testing.T) {
	handler, tokens := newTestAuth(t)
	user := registerUser(t, handler, "ada", testPassword, "ada@example.com")
	login := loginUser(t, handler, "ada", testPassword)

	claims, err := tokens.Parse(login.Token)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	if claims.UserID != user.ID {
		t.Fatalf("user_id claim = %d, want %d", claims.UserID, user.ID)
	}
	if claims.Username != "ada" {
		t.Fatalf("username claim = %q", claims.Username)
	}
	if claims.Issuer != jwt.Issuer {
		t.Fatalf("issuer claim = %q, want %q", claims.Issuer, jwt.Issuer)
	}
	if claims.Subject != strconv.FormatInt(user.ID, 10) {
		t.Fatalf("subject claim = %q, want %d", claims.Subject, user.ID)
	}
	if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(time.Now()) {
		t.Fatalf("expiry claim is not in the future: %+v", claims.ExpiresAt)
	}
}

func TestGetUserSuccess(t *testing.T) {
	handler, _ := newTestAuth(t)
	user := registerUser(t, handler, "ada", testPassword, "ada@example.com")

	recorder := requestRaw(t, handler, http.MethodGet, fmt.Sprintf("/users/%d", user.ID), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	assertJSONKeys(t, recorder, "id", "username", "is_admin", "interests", "created_at")

	got := decodeResponse[userResponse](t, recorder)
	if got.ID != user.ID || got.Username != "ada" {
		t.Fatalf("user = %+v, want id=%d username=ada", got, user.ID)
	}
	if got.IsAdmin {
		t.Fatal("non-admin user returned is_admin=true")
	}
	if got.Interests == nil || len(got.Interests) != 0 {
		t.Fatalf("interests = %v, want []", got.Interests)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at is zero")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(payload["interests"]) != "[]" {
		t.Fatalf("interests JSON = %s, want []", payload["interests"])
	}
}

func TestGetUserNotFound(t *testing.T) {
	handler, _ := newTestAuth(t)

	recorder := requestRaw(t, handler, http.MethodGet, "/users/999999", "")
	assertError(t, recorder, http.StatusNotFound, "user not found")
}

func TestGetUserIsAdmin(t *testing.T) {
	handler, _ := newTestAuth(t)
	admin := registerUser(t, handler, "admin", testPassword, "admin@example.com")
	user := registerUser(t, handler, "ada", testPassword, "ada@example.com")

	adminRecorder := requestRaw(t, handler, http.MethodGet, fmt.Sprintf("/users/%d", admin.ID), "")
	if adminRecorder.Code != http.StatusOK {
		t.Fatalf("admin status = %d; body = %s", adminRecorder.Code, adminRecorder.Body.String())
	}
	if got := decodeResponse[userResponse](t, adminRecorder); !got.IsAdmin {
		t.Fatal("admin profile returned is_admin=false")
	}

	userRecorder := requestRaw(t, handler, http.MethodGet, fmt.Sprintf("/users/%d", user.ID), "")
	if userRecorder.Code != http.StatusOK {
		t.Fatalf("user status = %d; body = %s", userRecorder.Code, userRecorder.Body.String())
	}
	if got := decodeResponse[userResponse](t, userRecorder); got.IsAdmin {
		t.Fatal("normal profile returned is_admin=true")
	}
}

func TestRegisterWithInterests(t *testing.T) {
	handler, _ := newTestAuth(t)
	interests := []string{"technology", "music", "travel"}

	recorder := requestJSON(t, handler, http.MethodPost, "/register",
		credentialsWithInterests("ada", testPassword, "ada@example.com", interests))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	assertJSONKeys(t, recorder, "id", "username", "is_admin", "interests")

	got := decodeResponse[registerResponse](t, recorder)
	if !slices.Equal(got.Interests, interests) {
		t.Fatalf("interests = %v, want %v", got.Interests, interests)
	}
}

func TestRegisterWithoutInterestsDefaultsToEmpty(t *testing.T) {
	handler, _ := newTestAuth(t)
	cases := []struct {
		name     string
		username string
		body     any
	}{
		{
			name:     "omitted",
			username: "omitted",
			body:     credentials("omitted", testPassword, "omitted@example.com"),
		},
		{
			name:     "explicit_empty",
			username: "empty",
			body:     credentialsWithInterests("empty", testPassword, "empty@example.com", []string{}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := requestJSON(t, handler, http.MethodPost, "/register", tc.body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
			}
			assertJSONKeys(t, recorder, "id", "username", "is_admin", "interests")
			got := decodeResponse[registerResponse](t, recorder)
			if got.Interests == nil || len(got.Interests) != 0 {
				t.Fatalf("interests = %v, want []", got.Interests)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if string(payload["interests"]) != "[]" {
				t.Fatalf("interests JSON = %s, want []", payload["interests"])
			}
		})
	}
}

func TestRegisterTooManyInterests(t *testing.T) {
	handler, _ := newTestAuth(t)
	interests := make([]string, maxInterests+1)
	for i := range interests {
		interests[i] = fmt.Sprintf("tag-%02d", i)
	}

	recorder := requestJSON(t, handler, http.MethodPost, "/register",
		credentialsWithInterests("ada", testPassword, "ada@example.com", interests))
	assertError(t, recorder, http.StatusBadRequest, "interests must contain at most 32 tags")

	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM auth.users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Fatalf("created %d user(s) with too many interests", count)
	}
}

func TestRegisterInterestTooLong(t *testing.T) {
	handler, _ := newTestAuth(t)
	interests := []string{strings.Repeat("x", maxInterestLength+1)}

	recorder := requestJSON(t, handler, http.MethodPost, "/register",
		credentialsWithInterests("ada", testPassword, "ada@example.com", interests))
	assertError(t, recorder, http.StatusBadRequest, "each interest must be at most 64 characters")
}

func TestLoginReturnsInterests(t *testing.T) {
	handler, _ := newTestAuth(t)
	interests := []string{"technology", "music"}
	registerUserWithInterests(t, handler, "ada", testPassword, "ada@example.com", interests)

	login := loginUser(t, handler, "ada", testPassword)
	if !slices.Equal(login.User.Interests, interests) {
		t.Fatalf("login interests = %v, want %v", login.User.Interests, interests)
	}
}

func TestGetUserReturnsInterests(t *testing.T) {
	handler, _ := newTestAuth(t)
	interests := []string{"technology", "music"}
	user := registerUserWithInterests(t, handler, "ada", testPassword, "ada@example.com", interests)

	recorder := requestRaw(t, handler, http.MethodGet, fmt.Sprintf("/users/%d", user.ID), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	got := decodeResponse[userResponse](t, recorder)
	if !slices.Equal(got.Interests, interests) {
		t.Fatalf("interests = %v, want %v", got.Interests, interests)
	}
}
