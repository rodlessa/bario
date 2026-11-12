// Package authclient is a small HTTP client used by the posts and follows
// services to resolve user ids to usernames through the auth service.
package authclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// User is the public profile shape returned by the auth service.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Client talks to the auth service's JSON API.
type Client struct {
	baseURL string
	http    *http.Client
}

// New creates a client for baseURL, e.g. http://bario-auth:8080.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

// User fetches a single profile by id.
func (c *Client) User(ctx context.Context, id int64) (User, error) {
	if c.baseURL == "" {
		return User{}, fmt.Errorf("AUTH_URL is not configured")
	}
	url := fmt.Sprintf("%s/users/%d", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return User{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return User{}, fmt.Errorf("call auth service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return User{}, fmt.Errorf("auth service returned %s for user %d", resp.Status, id)
	}
	var user User
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user); err != nil {
		return User{}, fmt.Errorf("decode auth response: %w", err)
	}
	if user.ID == 0 {
		user.ID = id
	}
	return user, nil
}

// Username resolves a display name and degrades gracefully when the auth
// service is unavailable.
func (c *Client) Username(ctx context.Context, id int64) string {
	user, err := c.User(ctx, id)
	if err != nil || user.Username == "" {
		return "user-" + strconv.FormatInt(id, 10)
	}
	return user.Username
}
