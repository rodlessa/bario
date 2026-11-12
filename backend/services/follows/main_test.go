package main

// Test infrastructure: the suite starts postgres:16-alpine with the Docker CLI
// on a random loopback port, waits with db.Connect, applies the same
// followsMigrations as run(), and removes the container in TestMain.
//
// The auth service is faked with an httptest server. No real auth service or
// manually running Postgres instance is required.

import (
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
	"strconv"
	"strings"
	"testing"
	"time"

	"bario/pkg/authclient"
	"bario/pkg/db"
	bariohttp "bario/pkg/http"
	"bario/pkg/jwt"
	"bario/pkg/metrics"
)

const (
	testPostgresImage = "postgres:16-alpine"
	testDBPassword    = "bario-test-password"
	testJWTSecret     = "bario-follows-tests"
)

var testPool *sql.DB

func TestMain(m *testing.M) {
	container, dsn, err := startTestPostgres()
	if err != nil {
		log.Fatalf("follows tests: %v", err)
	}
	cleanup := func() {
		if output, err := exec.Command("docker", "rm", "-f", container).CombinedOutput(); err != nil {
			log.Printf("follows tests: remove %s: %v (%s)", container, err, strings.TrimSpace(string(output)))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	pool, err := db.Connect(ctx, dsn, "follows")
	cancel()
	if err != nil {
		cleanup()
		log.Fatalf("follows tests: connect to postgres: %v", err)
	}
	if err := db.Migrate(context.Background(), pool, followsMigrations...); err != nil {
		pool.Close()
		cleanup()
		log.Fatalf("follows tests: apply migrations: %v", err)
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
		return "", "", fmt.Errorf("docker CLI is required to run follows tests")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("allocate postgres port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	container := fmt.Sprintf("bario-follows-test-%d", time.Now().UnixNano())
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

func resetFollows(t *testing.T) {
	t.Helper()
	if _, err := testPool.ExecContext(context.Background(), `TRUNCATE follows.follows`); err != nil {
		t.Fatalf("reset follows.follows: %v", err)
	}
}

func newTestAPI(t *testing.T, names map[int64]string) (http.Handler, *jwt.Manager) {
	t.Helper()
	resetFollows(t)

	tokens, err := jwt.NewManager(testJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("new jwt manager: %v", err)
	}

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawID := strings.TrimPrefix(r.URL.Path, "/users/")
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		name, ok := names[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authclient.User{ID: id, Username: name})
	}))
	t.Cleanup(authServer.Close)

	srv := &server{
		db:     testPool,
		tokens: tokens,
		auth:   authclient.New(authServer.URL),
	}
	router := bariohttp.NewRouter(metrics.New("follows"))
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

func request(t *testing.T, handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

type errorResponse struct {
	Error string `json:"error"`
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

func follow(t *testing.T, handler http.Handler, token string, followeeID int64) {
	t.Helper()
	recorder := request(t, handler, http.MethodPost, fmt.Sprintf("/users/%d/follow", followeeID), token)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("follow %d: status = %d; body = %s", followeeID, recorder.Code, recorder.Body.String())
	}
	var payload map[string]bool
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode follow response: %v", err)
	}
	if len(payload) != 1 || !payload["following"] {
		t.Fatalf("follow response = %s", recorder.Body.String())
	}
}

func unfollow(t *testing.T, handler http.Handler, token string, followeeID int64) {
	t.Helper()
	recorder := request(t, handler, http.MethodDelete, fmt.Sprintf("/users/%d/follow", followeeID), token)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("unfollow %d: status = %d; body = %s", followeeID, recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("unfollow response body = %q, want empty", recorder.Body.String())
	}
}

func getUsers(t *testing.T, handler http.Handler, path string) []userRef {
	t.Helper()
	recorder := request(t, handler, http.MethodGet, path, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d; body = %s", path, recorder.Code, recorder.Body.String())
	}
	return decodeResponse[[]userRef](t, recorder)
}

func getEmptyUsers(t *testing.T, handler http.Handler, path string) {
	t.Helper()
	recorder := request(t, handler, http.MethodGet, path, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d; body = %s", path, recorder.Code, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("GET %s body = %s, want []", path, recorder.Body.String())
	}
}

func assertUsers(t *testing.T, got []userRef, want map[int64]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("users = %+v, want %v", got, want)
	}
	seen := make(map[int64]bool, len(got))
	for _, user := range got {
		name, ok := want[user.ID]
		if !ok {
			t.Fatalf("unexpected user %+v", user)
		}
		if user.Username != name {
			t.Fatalf("user %d username = %q, want %q", user.ID, user.Username, name)
		}
		if seen[user.ID] {
			t.Fatalf("duplicate user %d", user.ID)
		}
		seen[user.ID] = true
	}
}

func countFollows(t *testing.T) int {
	t.Helper()
	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM follows.follows`).Scan(&count); err != nil {
		t.Fatalf("count follows: %v", err)
	}
	return count
}

func hasFollow(t *testing.T, followerID, followeeID int64) bool {
	t.Helper()
	var count int
	if err := testPool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM follows.follows WHERE follower_id = $1 AND followee_id = $2`,
		followerID, followeeID).Scan(&count); err != nil {
		t.Fatalf("lookup follow: %v", err)
	}
	return count == 1
}

func TestFollowSuccess(t *testing.T) {
	handler, tokens := newTestAPI(t, map[int64]string{1: "alice", 2: "bob"})
	token := tokenFor(t, tokens, 1, "alice")

	follow(t, handler, token, 2)

	if countFollows(t) != 1 || !hasFollow(t, 1, 2) {
		t.Fatalf("follows = %d, has(1->2) = %v", countFollows(t), hasFollow(t, 1, 2))
	}
}

func TestFollowSelf(t *testing.T) {
	handler, tokens := newTestAPI(t, map[int64]string{1: "alice"})
	token := tokenFor(t, tokens, 1, "alice")

	recorder := request(t, handler, http.MethodPost, "/users/1/follow", token)
	assertError(t, recorder, http.StatusBadRequest, "you cannot follow yourself")
	if countFollows(t) != 0 {
		t.Fatalf("self-follow created a row")
	}
}

func TestFollowIdempotent(t *testing.T) {
	handler, tokens := newTestAPI(t, map[int64]string{1: "alice", 2: "bob"})
	token := tokenFor(t, tokens, 1, "alice")

	follow(t, handler, token, 2)
	follow(t, handler, token, 2)

	if countFollows(t) != 1 || !hasFollow(t, 1, 2) {
		t.Fatalf("double follow produced %d rows", countFollows(t))
	}
}

func TestFollowUnauthenticated(t *testing.T) {
	handler, _ := newTestAPI(t, map[int64]string{1: "alice", 2: "bob"})

	recorder := request(t, handler, http.MethodPost, "/users/2/follow", "")
	assertError(t, recorder, http.StatusUnauthorized, "authentication required")
	if countFollows(t) != 0 {
		t.Fatalf("unauthenticated follow created a row")
	}
}

func TestUnfollowSuccess(t *testing.T) {
	handler, tokens := newTestAPI(t, map[int64]string{1: "alice", 2: "bob"})
	token := tokenFor(t, tokens, 1, "alice")
	follow(t, handler, token, 2)

	unfollow(t, handler, token, 2)

	if countFollows(t) != 0 || hasFollow(t, 1, 2) {
		t.Fatalf("follow still present after unfollow")
	}
}

func TestUnfollowNonexistent(t *testing.T) {
	handler, tokens := newTestAPI(t, map[int64]string{1: "alice", 2: "bob"})
	token := tokenFor(t, tokens, 1, "alice")

	unfollow(t, handler, token, 2)

	if countFollows(t) != 0 {
		t.Fatalf("unfollow of missing relation created a row")
	}
}

func TestUnfollowUnauthenticated(t *testing.T) {
	handler, _ := newTestAPI(t, map[int64]string{1: "alice", 2: "bob"})

	recorder := request(t, handler, http.MethodDelete, "/users/2/follow", "")
	assertError(t, recorder, http.StatusUnauthorized, "authentication required")
}

func TestFollowersList(t *testing.T) {
	names := map[int64]string{1: "alice", 2: "bob", 3: "carol", 4: "dave"}
	handler, tokens := newTestAPI(t, names)
	token1 := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")
	token3 := tokenFor(t, tokens, 3, "carol")
	token4 := tokenFor(t, tokens, 4, "dave")

	// 2, 3 and 4 follow 1; 4 also follows 2; 1 follows 4.
	follow(t, handler, token2, 1)
	follow(t, handler, token3, 1)
	follow(t, handler, token4, 1)
	follow(t, handler, token4, 2)
	follow(t, handler, token1, 4)

	assertUsers(t, getUsers(t, handler, "/users/1/followers"), map[int64]string{
		2: "bob",
		3: "carol",
		4: "dave",
	})
	assertUsers(t, getUsers(t, handler, "/users/2/followers"), map[int64]string{
		4: "dave",
	})
	getEmptyUsers(t, handler, "/users/99/followers")
}

func TestFollowingList(t *testing.T) {
	names := map[int64]string{1: "alice", 2: "bob", 3: "carol", 4: "dave", 5: "eve"}
	handler, tokens := newTestAPI(t, names)
	token1 := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")
	token5 := tokenFor(t, tokens, 5, "eve")

	// 1 follows 2 and 3; 2 follows 4; 5 follows 1.
	follow(t, handler, token1, 2)
	follow(t, handler, token1, 3)
	follow(t, handler, token2, 4)
	follow(t, handler, token5, 1)

	assertUsers(t, getUsers(t, handler, "/users/1/following"), map[int64]string{
		2: "bob",
		3: "carol",
	})
	assertUsers(t, getUsers(t, handler, "/users/2/following"), map[int64]string{
		4: "dave",
	})
	assertUsers(t, getUsers(t, handler, "/users/5/following"), map[int64]string{
		1: "alice",
	})
	getEmptyUsers(t, handler, "/users/3/following")
}

func TestFollowGraphInverse(t *testing.T) {
	names := map[int64]string{1: "alice", 2: "bob", 3: "carol", 4: "dave"}
	handler, tokens := newTestAPI(t, names)
	tokensByUser := map[int64]string{
		1: tokenFor(t, tokens, 1, "alice"),
		2: tokenFor(t, tokens, 2, "bob"),
		3: tokenFor(t, tokens, 3, "carol"),
		4: tokenFor(t, tokens, 4, "dave"),
	}

	edges := [][2]int64{
		{1, 2},
		{1, 3},
		{2, 3},
		{3, 1},
		{4, 1},
	}
	for _, edge := range edges {
		follow(t, handler, tokensByUser[edge[0]], edge[1])
	}

	wantFollowers := map[int64]map[int64]string{
		1: {3: "carol", 4: "dave"},
		2: {1: "alice"},
		3: {1: "alice", 2: "bob"},
		4: {},
	}
	wantFollowing := map[int64]map[int64]string{
		1: {2: "bob", 3: "carol"},
		2: {3: "carol"},
		3: {1: "alice"},
		4: {1: "alice"},
	}

	for userID := int64(1); userID <= 4; userID++ {
		assertUsers(t, getUsers(t, handler, fmt.Sprintf("/users/%d/followers", userID)), wantFollowers[userID])
		assertUsers(t, getUsers(t, handler, fmt.Sprintf("/users/%d/following", userID)), wantFollowing[userID])
	}

	for _, edge := range edges {
		followerID, followeeID := edge[0], edge[1]
		if _, ok := wantFollowing[followerID][followeeID]; !ok {
			t.Fatalf("expected %d to follow %d", followerID, followeeID)
		}
		if _, ok := wantFollowers[followeeID][followerID]; !ok {
			t.Fatalf("expected %d to be a follower of %d", followerID, followeeID)
		}
	}
}
