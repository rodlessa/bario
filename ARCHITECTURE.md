# Bario — a small social network (Twitter-like)

A minimal Twitter clone: post short messages, attach images, follow/unfollow users,
and get a home feed. Built for local Kubernetes, with per-service metrics that are
reachable only inside the cluster.

## Stack

- **Backend:** Go (4 services, one container each). Single static binary, small image.
- **Frontend:** Vue 3 + TypeScript + Vite. Has a **layout toggle** between a compact
  "old Reddit" style and a card-based "new Reddit" style.
- **Database:** PostgreSQL (one shared instance, one schema per service).
- **Deploy:** Kubernetes via a Helm chart.

## Components

```
                        ┌──────────────┐
   browser  ───────────▶│   Ingress    │──────────▶ frontend (Vue SPA)
                        │  /api/...    │
                        └──────┬───────┘
              /api/auth    /api/posts   /api/uploads   /api/follows
                 │             │             │              │
              ┌──▼──┐      ┌───▼──┐     ┌────▼───┐     ┌────▼───┐
              │auth │      │posts │     │uploads │     │follows │   ← Go services
              └──┬──┘      └──┬───┘     └───┬────┘     └───┬────┘
                 │  :8080 API + :9090 /metrics (ClusterIP only, never Ingress)
                 └───────────────┬──────────────┴───────────────┘
                                 │
                          ┌──────▼──────┐
                          │ PostgreSQL  │  (schema per service)
                          └─────────────┘
                          ┌──────▼──────┐
                          │ Prometheus  │  (ClusterIP only) scrapes /metrics
                          └─────────────┘
```

## Services

Every Go service:
- Listens on **:8080** (JSON API) and **:9090** (`/metrics`, Prometheus).
- Reads config from env vars: `DATABASE_URL`, `JWT_SECRET`, `METRICS_PORT`, `HTTP_PORT`.
- Verifies the JWT for protected endpoints (shared `JWT_SECRET`).
- Uses the same shared libraries from `backend/pkg/` (metrics, http, jwt, db).

### auth — schema `auth`
- `POST /register` `{username, password, email}` → `201 {id, username}`
- `POST /login` `{username, password}` → `200 {token, user}`
- `GET /users/{id}` → public profile `{id, username, created_at}`
- Table `users(id, username unique, password_hash, email, created_at)`.

### posts — schema `posts`
- `POST /posts` `{content, image_id?}` (auth) → `201 {id, ...}`
- `GET /posts/{id}` → post + author
- `GET /users/{id}/posts?cursor=&limit=` → that user's posts, newest first
- `GET /feed?cursor=&limit=` (auth) → posts from users the caller follows (calls the
  `follows` service over HTTP to get the followed ids), newest first
- Table `posts(id, user_id, content, image_id, created_at)`.

### uploads — schema `uploads`
- `POST /uploads` (auth, `multipart/form-data` with a `file` field) → `201 {id, url}`
- `GET /uploads/{id}` → image bytes (correct `Content-Type`)
- Files stored on a PersistentVolume at `UPLOAD_DIR`; metadata in the table.
- Table `images(id, user_id, filename, content_type, size, created_at)`.

### follows — schema `follows`
- `POST /users/{id}/follow` (auth) → `201`
- `DELETE /users/{id}/follow` (auth) → `204`
- `GET /users/{id}/followers` → `[{id, username}]`
- `GET /users/{id}/following` → `[{id, username}]`
- Table `follows(follower_id, followee_id, created_at, PRIMARY KEY(follower_id, followee_id))`.

## Metrics (the isolation requirement)

Each service exposes, on **:9090/metrics** only:

```
bario_http_requests_total{service, endpoint, method, status}
bario_http_request_duration_seconds{service, endpoint, method}   # histogram
```

Labels: `service` ∈ {auth, posts, uploads, follows}; `endpoint` is the canonical
route name (e.g. `create_post`, `get_feed`, `follow_user`, `upload_image`), not the raw
path. One counter/histogram per main endpoint — posting, image upload, and
follow/unfollow must each be individually labeled.

**Metrics must never be exposed publicly.** The `:9090` port is served by a Kubernetes
`Service` of type `ClusterIP` (named `<service>-metrics`) and is **not** referenced by
the Ingress. Prometheus runs inside the cluster and scrapes those ClusterIP services.
No `NodePort`, no `LoadBalancer`, no Ingress path for metrics.

## Frontend (Vue 3 + TypeScript + Vite)

- SPA, served by nginx (multi-stage build: `node` build → `nginx`).
- API calls go to `/api/...` (proxied by Ingress).
- **Layout toggle** between:
  - `classic` (old Reddit): dense compact list, small text, sidebar nav, minimal chrome.
  - `modern` (new Reddit): card layout, rounded corners, larger spacing.
  - A user toggle persists the choice in `localStorage`. Implement as two CSS themes /
    component variants, not two codebases.
- Pages: login/register, home feed, user profile, single post, compose (with image upload).

## Deploy — Helm chart (`deploy/bario/`)

```
deploy/bario/
├── Chart.yaml
├── values.yaml
└── templates/
    ├── postgres.yaml          # StatefulSet + PVC + Service + Secret (password)
    ├── auth.yaml              # Deployment + Service(:8080) + Service(:9090 -metrics)
    ├── posts.yaml
    ├── uploads.yaml           # + PVC for UPLOAD_DIR
    ├── follows.yaml
    ├── frontend.yaml          # Deployment (nginx) + Service
    ├── ingress.yaml           # routes /api/* → services, / → frontend (NO metrics)
    └── prometheus.yaml        # Deployment + ClusterIP Service + ConfigMap (scrape jobs)
```

Values (`values.yaml`) parametrize images, replicas, the Postgres password (Secret),
JWT secret (Secret), and resource requests. `prometheus.yaml` holds a scrape config
targeting each `<service>-metrics:9090` service.

## Directory layout

```
~/bario/
├── ARCHITECTURE.md
├── README.md
├── Makefile                   # build / docker-build / helm-install / helm-upgrade
├── backend/
│   ├── go.work
│   ├── pkg/                   # shared: metrics, http, jwt, db
│   └── services/
│       ├── auth/
│       ├── posts/
│       ├── uploads/
│       └── follows/
├── frontend/                  # Vue 3 + TS + Vite
└── deploy/bario/              # Helm chart
```

## Security

- No API key, token, password, or `.env` value is ever hardcoded or committed.
  All secrets come from Kubernetes `Secret` objects / env vars.
- JWT secret and Postgres password are generated, not committed.
