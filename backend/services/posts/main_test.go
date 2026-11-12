package main

// Test infrastructure: the suite starts postgres:16-alpine with the Docker CLI
// on a random loopback port, waits with db.Connect, applies the same
// postMigrations as run(), and removes the container in TestMain.
//
// The auth and follows services are faked with httptest servers, because both
// are concrete HTTP clients in this package. No real auth/follows services or
// manually running Postgres instance are required.

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
	testJWTSecret     = "bario-posts-tests"
)

var testPool *sql.DB

func TestMain(m *testing.M) {
	container, dsn, err := startTestPostgres()
	if err != nil {
		log.Fatalf("posts tests: %v", err)
	}
	cleanup := func() {
		if output, err := exec.Command("docker", "rm", "-f", container).CombinedOutput(); err != nil {
			log.Printf("posts tests: remove %s: %v (%s)", container, err, strings.TrimSpace(string(output)))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	pool, err := db.Connect(ctx, dsn, "posts")
	cancel()
	if err != nil {
		cleanup()
		log.Fatalf("posts tests: connect to postgres: %v", err)
	}
	if err := db.Migrate(context.Background(), pool, postMigrations...); err != nil {
		pool.Close()
		cleanup()
		log.Fatalf("posts tests: apply migrations: %v", err)
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
		return "", "", fmt.Errorf("docker CLI is required to run posts tests")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("allocate postgres port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	container := fmt.Sprintf("bario-posts-test-%d", time.Now().UnixNano())
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

type followedUser struct {
	ID int64 `json:"id"`
}

type fakeFollows struct {
	ids    []int64
	status int
}

type adsRequest struct {
	pages         string
	limit         string
	authorization string
}

type fakeAds struct {
	ads      []servedAd
	status   int
	requests []adsRequest
}

type errorResponse struct {
	Error string `json:"error"`
}

type testFeedItem struct {
	Kind         string  `json:"kind"`
	ID           int64   `json:"id"`
	UserID       int64   `json:"user_id"`
	Content      string  `json:"content"`
	ImageID      *string `json:"image_id"`
	Author       author  `json:"author"`
	AdvertiserID int64   `json:"advertiser_id"`
	Title        string  `json:"title"`
	Body         string  `json:"body"`
}

type testFeed struct {
	Items      []testFeedItem `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

func organicIDs(items []testFeedItem) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		if item.Kind == "post" {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func feedKinds(items []testFeedItem) []string {
	kinds := make([]string, len(items))
	for i, item := range items {
		kinds[i] = item.Kind
	}
	return kinds
}

func resetPosts(t *testing.T) {
	t.Helper()
	if _, err := testPool.ExecContext(context.Background(), `TRUNCATE posts.posts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset posts.posts: %v", err)
	}
}

func newTestAPI(t *testing.T, follows fakeFollows, names map[int64]string) (http.Handler, *jwt.Manager) {
	t.Helper()
	return newTestAPIWithAds(t, follows, &fakeAds{}, names)
}

func newTestAPIWithAds(t *testing.T, follows fakeFollows, ads *fakeAds, names map[int64]string) (http.Handler, *jwt.Manager) {
	t.Helper()
	resetPosts(t)

	if ads == nil {
		ads = &fakeAds{}
	}

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

	followsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if follows.status != 0 && follows.status != http.StatusOK {
			http.Error(w, "follows unavailable", follows.status)
			return
		}
		users := make([]followedUser, 0, len(follows.ids))
		for _, id := range follows.ids {
			users = append(users, followedUser{ID: id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(users)
	}))
	t.Cleanup(followsServer.Close)

	adsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ads.requests = append(ads.requests, adsRequest{
			pages:         r.URL.Query().Get("pages"),
			limit:         r.URL.Query().Get("limit"),
			authorization: r.Header.Get("Authorization"),
		})
		if r.URL.Path != "/ads/serve" {
			http.NotFound(w, r)
			return
		}
		if ads.status != 0 && ads.status != http.StatusOK {
			http.Error(w, "ads unavailable", ads.status)
			return
		}
		payload := ads.ads
		if payload == nil {
			payload = []servedAd{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(adsServer.Close)

	srv := &server{
		db:      testPool,
		tokens:  tokens,
		auth:    authclient.New(authServer.URL),
		follows: newFollowsClient(followsServer.URL),
		ads:     newAdsClient(adsServer.URL),
	}
	router := bariohttp.NewRouter(metrics.New("posts"))
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

func requestRaw(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestJSON(t *testing.T, handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return requestRaw(t, handler, method, path, token, string(raw))
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

func createPost(t *testing.T, handler http.Handler, token, content string) post {
	t.Helper()
	recorder := requestJSON(t, handler, http.MethodPost, "/posts", token, map[string]string{"content": content})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create post: status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	return decodeResponse[post](t, recorder)
}

func postIDs(posts []post) []int64 {
	ids := make([]int64, len(posts))
	for i, p := range posts {
		ids[i] = p.ID
	}
	return ids
}

func cursorValue(cursor *string) string {
	if cursor == nil {
		return ""
	}
	return *cursor
}

func assertIDs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func assertKinds(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}

func TestCreatePostSuccess(t *testing.T) {
	handler, tokens := newTestAPI(t, fakeFollows{}, nil)
	token := tokenFor(t, tokens, 7, "ada")

	recorder := requestJSON(t, handler, http.MethodPost, "/posts", token, map[string]string{"content": "hello world"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	assertJSONKeys(t, recorder, "id", "user_id", "content", "image_id", "created_at", "author")

	got := decodeResponse[post](t, recorder)
	if got.ID <= 0 || got.UserID != 7 || got.Content != "hello world" {
		t.Fatalf("post = %+v", got)
	}
	if got.ImageID != nil {
		t.Fatalf("image_id = %v, want nil", got.ImageID)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at is zero")
	}
	if got.Author.ID != 7 || got.Author.Username != "ada" {
		t.Fatalf("author = %+v", got.Author)
	}

	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM posts.posts`).Scan(&count); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	if count != 1 {
		t.Fatalf("posts = %d, want 1", count)
	}
}

func TestCreatePostContentValidation(t *testing.T) {
	handler, tokens := newTestAPI(t, fakeFollows{}, nil)
	token := tokenFor(t, tokens, 7, "ada")

	t.Run("empty", func(t *testing.T) {
		recorder := requestJSON(t, handler, http.MethodPost, "/posts", token, map[string]string{"content": ""})
		assertError(t, recorder, http.StatusBadRequest, "content must not be empty")
	})

	t.Run("whitespace_only", func(t *testing.T) {
		recorder := requestJSON(t, handler, http.MethodPost, "/posts", token, map[string]string{"content": "   "})
		assertError(t, recorder, http.StatusBadRequest, "content must not be empty")
	})

	t.Run("too_long", func(t *testing.T) {
		content := strings.Repeat("a", maxContent+1)
		recorder := requestJSON(t, handler, http.MethodPost, "/posts", token, map[string]string{"content": content})
		assertError(t, recorder, http.StatusBadRequest, "content must be at most 280 characters")
	})

	t.Run("max_length", func(t *testing.T) {
		content := strings.Repeat("a", maxContent)
		recorder := requestJSON(t, handler, http.MethodPost, "/posts", token, map[string]string{"content": content})
		if recorder.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
		}
		if got := decodeResponse[post](t, recorder); got.Content != content {
			t.Fatalf("content length = %d, want %d", len(got.Content), len(content))
		}
	})
}

func TestCreatePostUnauthenticated(t *testing.T) {
	handler, _ := newTestAPI(t, fakeFollows{}, nil)

	recorder := requestJSON(t, handler, http.MethodPost, "/posts", "", map[string]string{"content": "hello"})
	assertError(t, recorder, http.StatusUnauthorized, "authentication required")

	var count int
	if err := testPool.QueryRowContext(context.Background(), `SELECT count(*) FROM posts.posts`).Scan(&count); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	if count != 0 {
		t.Fatalf("posts = %d, want 0", count)
	}
}

func TestGetPostSuccess(t *testing.T) {
	handler, tokens := newTestAPI(t, fakeFollows{}, map[int64]string{7: "ada"})
	token := tokenFor(t, tokens, 7, "ada")
	created := createPost(t, handler, token, "a single post")

	recorder := requestRaw(t, handler, http.MethodGet, fmt.Sprintf("/posts/%d", created.ID), "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	assertJSONKeys(t, recorder, "id", "user_id", "content", "image_id", "created_at", "author")

	got := decodeResponse[post](t, recorder)
	if got.ID != created.ID || got.UserID != 7 || got.Content != "a single post" {
		t.Fatalf("post = %+v", got)
	}
	if got.Author.ID != 7 || got.Author.Username != "ada" {
		t.Fatalf("author = %+v", got.Author)
	}
}

func TestGetPostNotFound(t *testing.T) {
	handler, _ := newTestAPI(t, fakeFollows{}, nil)

	recorder := requestRaw(t, handler, http.MethodGet, "/posts/999999", "", "")
	assertError(t, recorder, http.StatusNotFound, "post not found")
}

func TestListUserPostsPagination(t *testing.T) {
	handler, tokens := newTestAPI(t, fakeFollows{}, map[int64]string{7: "ada", 8: "bob"})
	token7 := tokenFor(t, tokens, 7, "ada")
	token8 := tokenFor(t, tokens, 8, "bob")

	var user7Posts []post
	for i := 1; i <= 5; i++ {
		user7Posts = append(user7Posts, createPost(t, handler, token7, fmt.Sprintf("post-%d", i)))
	}
	user8Post := createPost(t, handler, token8, "unrelated")

	page1 := requestRaw(t, handler, http.MethodGet, "/users/7/posts?limit=2", "", "")
	if page1.Code != http.StatusOK {
		t.Fatalf("page1 status = %d; body = %s", page1.Code, page1.Body.String())
	}
	assertJSONKeys(t, page1, "posts", "next_cursor")
	got1 := decodeResponse[postList](t, page1)
	assertIDs(t, postIDs(got1.Posts), user7Posts[4].ID, user7Posts[3].ID)
	if cursor := cursorValue(got1.NextCursor); cursor != strconv.FormatInt(user7Posts[3].ID, 10) {
		t.Fatalf("page1 cursor = %q", cursor)
	}
	if got1.Posts[0].Author.Username != "ada" {
		t.Fatalf("author = %q, want ada", got1.Posts[0].Author.Username)
	}

	page2 := requestRaw(t, handler, http.MethodGet,
		fmt.Sprintf("/users/7/posts?limit=2&cursor=%d", user7Posts[3].ID), "", "")
	got2 := decodeResponse[postList](t, page2)
	assertIDs(t, postIDs(got2.Posts), user7Posts[2].ID, user7Posts[1].ID)
	if cursor := cursorValue(got2.NextCursor); cursor != strconv.FormatInt(user7Posts[1].ID, 10) {
		t.Fatalf("page2 cursor = %q", cursor)
	}

	page3 := requestRaw(t, handler, http.MethodGet,
		fmt.Sprintf("/users/7/posts?limit=2&cursor=%d", user7Posts[1].ID), "", "")
	got3 := decodeResponse[postList](t, page3)
	assertIDs(t, postIDs(got3.Posts), user7Posts[0].ID)
	if got3.NextCursor != nil {
		t.Fatalf("page3 cursor = %q, want nil", *got3.NextCursor)
	}

	all := requestRaw(t, handler, http.MethodGet, "/users/7/posts?limit=10", "", "")
	gotAll := decodeResponse[postList](t, all)
	if len(gotAll.Posts) != 5 || gotAll.NextCursor != nil {
		t.Fatalf("all posts = %d, cursor = %v", len(gotAll.Posts), gotAll.NextCursor)
	}
	for _, p := range gotAll.Posts {
		if p.ID == user8Post.ID {
			t.Fatal("list included another user's post")
		}
	}
}

func TestListUserPostsEmpty(t *testing.T) {
	handler, _ := newTestAPI(t, fakeFollows{}, nil)

	recorder := requestRaw(t, handler, http.MethodGet, "/users/999/posts", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	assertJSONKeys(t, recorder, "posts", "next_cursor")
	got := decodeResponse[postList](t, recorder)
	if len(got.Posts) != 0 || got.NextCursor != nil {
		t.Fatalf("posts = %d, cursor = %v", len(got.Posts), got.NextCursor)
	}
	if !strings.Contains(recorder.Body.String(), `"posts":[]`) {
		t.Fatalf("empty posts was not encoded as []: %s", recorder.Body.String())
	}
}

func TestFeedFilteringAndOrdering(t *testing.T) {
	names := map[int64]string{2: "bob", 3: "carol", 4: "dave"}
	handler, tokens := newTestAPI(t, fakeFollows{ids: []int64{2, 3}}, names)
	caller := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")
	token3 := tokenFor(t, tokens, 3, "carol")
	token4 := tokenFor(t, tokens, 4, "dave")

	createPost(t, handler, token2, "b1")
	createPost(t, handler, token2, "b2")
	createPost(t, handler, token3, "c1")
	createPost(t, handler, token3, "c2")
	unfollowed := createPost(t, handler, token4, "d1")

	recorder := requestRaw(t, handler, http.MethodGet, "/feed", caller, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	assertJSONKeys(t, recorder, "items", "next_cursor")
	got := decodeResponse[testFeed](t, recorder)
	assertIDs(t, organicIDs(got.Items), 4, 3, 2, 1)
	if got.NextCursor != nil {
		t.Fatalf("cursor = %q, want nil", *got.NextCursor)
	}
	for _, item := range got.Items {
		if item.Kind != "post" {
			t.Fatalf("kind = %q, want post", item.Kind)
		}
		if item.ID == unfollowed.ID {
			t.Fatal("feed included an unfollowed account's post")
		}
		if item.UserID != 2 && item.UserID != 3 {
			t.Fatalf("feed included user %d", item.UserID)
		}
	}
	if got.Items[0].Author.Username != "carol" || got.Items[2].Author.Username != "bob" {
		t.Fatalf("authors = %q, %q", got.Items[0].Author.Username, got.Items[2].Author.Username)
	}
}

func TestFeedPagination(t *testing.T) {
	names := map[int64]string{2: "bob", 3: "carol", 4: "dave"}
	handler, tokens := newTestAPI(t, fakeFollows{ids: []int64{2, 3}}, names)
	caller := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")
	token3 := tokenFor(t, tokens, 3, "carol")
	token4 := tokenFor(t, tokens, 4, "dave")

	createPost(t, handler, token2, "b1")
	createPost(t, handler, token2, "b2")
	createPost(t, handler, token2, "b3")
	createPost(t, handler, token3, "c1")
	createPost(t, handler, token3, "c2")
	unfollowed := createPost(t, handler, token4, "d1")

	page1 := requestRaw(t, handler, http.MethodGet, "/feed?limit=2", caller, "")
	got1 := decodeResponse[testFeed](t, page1)
	assertIDs(t, organicIDs(got1.Items), 5, 4)
	if cursor := cursorValue(got1.NextCursor); cursor != "4" {
		t.Fatalf("page1 cursor = %q, want 4", cursor)
	}

	page2 := requestRaw(t, handler, http.MethodGet, "/feed?limit=2&cursor=4", caller, "")
	got2 := decodeResponse[testFeed](t, page2)
	assertIDs(t, organicIDs(got2.Items), 3, 2)
	if cursor := cursorValue(got2.NextCursor); cursor != "2" {
		t.Fatalf("page2 cursor = %q, want 2", cursor)
	}

	page3 := requestRaw(t, handler, http.MethodGet, "/feed?limit=2&cursor=2", caller, "")
	got3 := decodeResponse[testFeed](t, page3)
	assertIDs(t, organicIDs(got3.Items), 1)
	if got3.NextCursor != nil {
		t.Fatalf("page3 cursor = %q, want nil", *got3.NextCursor)
	}
	for _, item := range got1.Items {
		if item.ID == unfollowed.ID {
			t.Fatal("feed included an unfollowed account's post")
		}
	}
}

func TestFeedEmptyWhenFollowingNobody(t *testing.T) {
	handler, tokens := newTestAPI(t, fakeFollows{ids: nil}, nil)
	caller := tokenFor(t, tokens, 1, "alice")

	recorder := requestRaw(t, handler, http.MethodGet, "/feed", caller, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	got := decodeResponse[testFeed](t, recorder)
	if len(got.Items) != 0 || got.NextCursor != nil {
		t.Fatalf("items = %d, cursor = %v", len(got.Items), got.NextCursor)
	}
	if !strings.Contains(recorder.Body.String(), `"items":[]`) {
		t.Fatalf("empty feed was not encoded as []: %s", recorder.Body.String())
	}
}

func TestFeedFollowsError(t *testing.T) {
	handler, tokens := newTestAPI(t, fakeFollows{status: http.StatusInternalServerError}, nil)
	caller := tokenFor(t, tokens, 1, "alice")

	recorder := requestRaw(t, handler, http.MethodGet, "/feed", caller, "")
	assertError(t, recorder, http.StatusBadGateway, "could not load the accounts you follow")
}

func TestFeedInterleavesAds(t *testing.T) {
	names := map[int64]string{2: "bob"}
	fake := &fakeAds{ads: []servedAd{
		{ID: 901, AdvertiserID: 50, Title: "Ad one", Body: "First sponsored body"},
		{ID: 902, AdvertiserID: 51, Title: "Ad two", Body: "Second sponsored body"},
	}}
	handler, tokens := newTestAPIWithAds(t, fakeFollows{ids: []int64{2}}, fake, names)
	caller := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")

	for i := 1; i <= 4; i++ {
		createPost(t, handler, token2, fmt.Sprintf("post-%d", i))
	}

	recorder := requestRaw(t, handler, http.MethodGet, "/feed", caller, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	got := decodeResponse[testFeed](t, recorder)

	// Two ads across four posts are spread between organic items, not appended.
	assertKinds(t, feedKinds(got.Items), "post", "ad", "post", "ad", "post", "post")
	assertIDs(t, organicIDs(got.Items), 4, 3, 2, 1)
	if got.Items[len(got.Items)-1].Kind != "post" {
		t.Fatal("feed ended with an ad, want the last organic post")
	}

	if got.Items[1].ID != 901 || got.Items[1].Title != "Ad one" || got.Items[1].Body != "First sponsored body" {
		t.Fatalf("first ad = %+v", got.Items[1])
	}
	if got.Items[3].ID != 902 || got.Items[3].AdvertiserID != 51 {
		t.Fatalf("second ad = %+v", got.Items[3])
	}

	var payload struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode feed items: %v", err)
	}
	postKeys := []string{"kind", "id", "user_id", "content", "image_id", "created_at", "author"}
	adKeys := []string{"kind", "id", "advertiser_id", "title", "body"}
	for i, item := range payload.Items {
		want := postKeys
		if i == 1 || i == 3 {
			want = adKeys
		}
		if len(item) != len(want) {
			t.Fatalf("item %d has %d keys, want %d: %v", i, len(item), len(want), item)
		}
		for _, key := range want {
			if _, ok := item[key]; !ok {
				t.Fatalf("item %d is missing key %q: %v", i, key, item)
			}
		}
	}
}

func TestFeedAdsErrorDegradesToOrganicOnly(t *testing.T) {
	names := map[int64]string{2: "bob"}
	fake := &fakeAds{status: http.StatusInternalServerError}
	handler, tokens := newTestAPIWithAds(t, fakeFollows{ids: []int64{2}}, fake, names)
	caller := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")

	createPost(t, handler, token2, "b1")
	createPost(t, handler, token2, "b2")

	recorder := requestRaw(t, handler, http.MethodGet, "/feed", caller, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	got := decodeResponse[testFeed](t, recorder)
	assertKinds(t, feedKinds(got.Items), "post", "post")
	assertIDs(t, organicIDs(got.Items), 2, 1)
	if got.NextCursor != nil {
		t.Fatalf("cursor = %q, want nil", *got.NextCursor)
	}
}

func TestFeedForwardsPagesAndAuthToAds(t *testing.T) {
	names := map[int64]string{2: "bob"}
	fake := &fakeAds{}
	handler, tokens := newTestAPIWithAds(t, fakeFollows{ids: []int64{2}}, fake, names)
	caller := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")
	createPost(t, handler, token2, "b1")

	if recorder := requestRaw(t, handler, http.MethodGet, "/feed?pages=3&limit=2", caller, ""); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	if len(fake.requests) != 1 {
		t.Fatalf("ads requests = %d, want 1", len(fake.requests))
	}
	got := fake.requests[0]
	if got.pages != "3" || got.limit != "2" {
		t.Fatalf("ads query pages=%q limit=%q, want 3 and 2", got.pages, got.limit)
	}
	if got.authorization != "Bearer "+caller {
		t.Fatalf("ads authorization = %q, want caller token", got.authorization)
	}

	fake.requests = nil
	if recorder := requestRaw(t, handler, http.MethodGet, "/feed", caller, ""); recorder.Code != http.StatusOK {
		t.Fatalf("default pages status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	if len(fake.requests) != 1 || fake.requests[0].pages != "0" {
		t.Fatalf("default ads request = %+v, want pages=0", fake.requests)
	}
}

func TestFeedInvalidPages(t *testing.T) {
	fake := &fakeAds{}
	handler, tokens := newTestAPIWithAds(t, fakeFollows{ids: []int64{2}}, fake, nil)
	caller := tokenFor(t, tokens, 1, "alice")

	recorder := requestRaw(t, handler, http.MethodGet, "/feed?pages=nope", caller, "")
	assertError(t, recorder, http.StatusBadRequest, "invalid pages")
	if len(fake.requests) != 0 {
		t.Fatalf("ads was called for an invalid pages value: %+v", fake.requests)
	}
}

func TestFeedCursorUnaffectedByAdInterleaving(t *testing.T) {
	names := map[int64]string{2: "bob"}
	fake := &fakeAds{ads: []servedAd{
		{ID: 701, AdvertiserID: 50, Title: "Ad A", Body: "A"},
		{ID: 702, AdvertiserID: 50, Title: "Ad B", Body: "B"},
	}}
	handler, tokens := newTestAPIWithAds(t, fakeFollows{ids: []int64{2}}, fake, names)
	caller := tokenFor(t, tokens, 1, "alice")
	token2 := tokenFor(t, tokens, 2, "bob")

	for i := 1; i <= 5; i++ {
		createPost(t, handler, token2, fmt.Sprintf("post-%d", i))
	}

	page1 := requestRaw(t, handler, http.MethodGet, "/feed?limit=2", caller, "")
	got1 := decodeResponse[testFeed](t, page1)
	assertIDs(t, organicIDs(got1.Items), 5, 4)
	if cursor := cursorValue(got1.NextCursor); cursor != "4" {
		t.Fatalf("page1 cursor = %q, want 4", cursor)
	}

	page2 := requestRaw(t, handler, http.MethodGet, "/feed?limit=2&cursor=4", caller, "")
	got2 := decodeResponse[testFeed](t, page2)
	assertIDs(t, organicIDs(got2.Items), 3, 2)
	if cursor := cursorValue(got2.NextCursor); cursor != "2" {
		t.Fatalf("page2 cursor = %q, want 2", cursor)
	}

	page3 := requestRaw(t, handler, http.MethodGet, "/feed?limit=2&cursor=2", caller, "")
	got3 := decodeResponse[testFeed](t, page3)
	assertIDs(t, organicIDs(got3.Items), 1)
	if got3.NextCursor != nil {
		t.Fatalf("page3 cursor = %q, want nil", *got3.NextCursor)
	}

	seen := append(append(organicIDs(got1.Items), organicIDs(got2.Items)...), organicIDs(got3.Items)...)
	assertIDs(t, seen, 5, 4, 3, 2, 1)
}
