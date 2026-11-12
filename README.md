# Bario

Bario is a small Twitter-like social network: short posts, image attachments,
follow/unfollow, and a home feed. It is built as four Go services behind a Vue 3
SPA, deployed to local Kubernetes with a Helm chart. Every service exposes
Prometheus metrics on a private, cluster-only port.

## Stack

| Layer    | Tech                                                     |
| -------- | -------------------------------------------------------- |
| Backend  | Go (4 services: auth, posts, uploads, follows)           |
| Frontend | Vue 3 + TypeScript + Vite, served by nginx               |
| Database | PostgreSQL 16, one schema per service                    |
| Deploy   | Docker images + Helm chart + nginx Ingress               |
| Metrics  | Prometheus, scraping cluster-only `*-metrics` Services   |

Each Go service listens on `:8080` for JSON and `:9090` for `/metrics`. Both
ports come from `HTTP_PORT` and `METRICS_PORT`.

## Layout

```
bario/
├── ARCHITECTURE.md
├── Makefile
├── README.md
├── backend/
│   ├── go.work
│   ├── pkg/                  # shared: metrics, http, jwt, db, authclient
│   └── services/
│       ├── auth/             # go.mod + main.go + Dockerfile
│       ├── posts/
│       ├── uploads/
│       └── follows/
├── frontend/                 # Vue 3 + TS + Vite + nginx
└── deploy/bario/             # Helm chart
```

## Quick start on Kubernetes (kind)

Prerequisites: Go 1.24+, Node 20+, Docker, `kubectl`, Helm 3, and
[kind](https://kind.sigs.k8s.io/). An nginx ingress controller is required.

```bash
# 1. Cluster
kind create cluster --name bario

# 2. nginx ingress controller (kind flavour)
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/main/deploy/static/provider/kind/deploy.yaml
kubectl wait --namespace ingress-nginx \
  --for=condition=ready pod \
  --selector=app.kubernetes.io/component=controller \
  --timeout=180s

# 3. Images
make docker-build
kind load docker-image \
  bario/auth:latest bario/posts:latest bario/uploads:latest \
  bario/follows:latest bario/frontend:latest --name bario

# 4. Deploy
make helm-install
```

Point `bario.local` at the ingress:

```bash
echo "127.0.0.1 bario.local" | sudo tee -a /etc/hosts
```

Open <http://bario.local>. Register two accounts, follow one from the other,
post something, and the feed fills up.

On minikube, load images with `minikube image load <image>` instead of
`kind load`.

## Configuration and secrets

`deploy/bario/values.yaml` parametrizes images, replicas, storage, ingress host,
and resources. Secrets are never committed:

- `postgres.password` and `jwt.secret` are empty by default. On first install
  the chart generates random values with `randAlphaNum` and stores them in the
  `bario-postgres` and `bario-jwt` Kubernetes Secrets. Upgrades reuse the
  existing Secret values via `lookup`.
- Each service receives `DATABASE_URL` as
  `postgres://<user>:$(POSTGRES_PASSWORD)@<release>-postgres:5432/<db>?sslmode=disable`.
  The password itself comes from the Secret through `POSTGRES_PASSWORD`; it is
  expanded at container start and is never baked into an image or a manifest.
- `JWT_SECRET` is mounted from the `bario-jwt` Secret and shared by all four
  services.

To supply your own values for a local install:

```bash
helm upgrade --install bario deploy/bario \
  --namespace bario --create-namespace \
  --set postgres.password="$(openssl rand -hex 24)" \
  --set jwt.secret="$(openssl rand -hex 32)"
```

Do not commit an override file containing real secrets. To use a different
image registry or tag:

```bash
make docker-build REGISTRY=registry.example.com/bario TAG=1.0.0
make helm-install \
  REGISTRY=registry.example.com/bario TAG=1.0.0
# or override each service image explicitly with --set services.auth.image.repository=...
```

## Local development without Kubernetes

Start PostgreSQL with a generated password:

```bash
export POSTGRES_PASSWORD="$(openssl rand -hex 24)"
docker run --rm -d --name bario-postgres \
  -e POSTGRES_USER=bario \
  -e POSTGRES_DB=bario \
  -e POSTGRES_PASSWORD="$POSTGRES_PASSWORD" \
  -p 5432:5432 postgres:16-alpine

export DATABASE_URL="postgres://bario:${POSTGRES_PASSWORD}@localhost:5432/bario?sslmode=disable"
export JWT_SECRET="$(openssl rand -hex 32)"
```

Run the four services on separate ports (the frontend dev proxy expects these):

```bash
cd backend

(cd services/auth && HTTP_PORT=8081 METRICS_PORT=9091 go run .) &
(cd services/follows && HTTP_PORT=8083 METRICS_PORT=9093 \
  AUTH_URL=http://localhost:8081 go run .) &
(cd services/posts && HTTP_PORT=8082 METRICS_PORT=9092 \
  AUTH_URL=http://localhost:8081 FOLLOWS_URL=http://localhost:8083 go run .) &
(cd services/uploads && HTTP_PORT=8084 METRICS_PORT=9094 \
  UPLOAD_DIR=/tmp/bario-uploads go run .) &
```

Then run the SPA:

```bash
cd frontend
npm ci
npm run dev
```

The dev server listens on <http://localhost:5173> and proxies `/api/auth`,
`/api/posts`, `/api/follows`, and `/api/uploads` to the ports above. Override
targets with `BARIO_AUTH_URL`, `BARIO_POSTS_URL`, `BARIO_FOLLOWS_URL`, and
`BARIO_UPLOADS_URL` if needed.

## API

All APIs are also reachable through the ingress under `/api/<service>`; the
services mount the same handlers under both the bare path and that prefix.

| Service | Method + path                            | Auth | Description                              |
| ------- | ---------------------------------------- | ---- | ---------------------------------------- |
| auth    | `POST /register`                         | no   | `{username, password, email}` → `201`    |
| auth    | `POST /login`                            | no   | `{username, password}` → `{token, user}` |
| auth    | `GET /users/{id}`                        | no   | Public profile                           |
| posts   | `POST /posts`                            | yes  | `{content, image_id?}` → `201`           |
| posts   | `GET /posts/{id}`                        | no   | Post plus author                         |
| posts   | `GET /users/{id}/posts?cursor=&limit=`   | no   | A user's posts, newest first             |
| posts   | `GET /feed?cursor=&limit=`               | yes  | Posts from followed accounts             |
| uploads | `POST /uploads`                          | yes  | `multipart/form-data`, field `file`      |
| uploads | `GET /uploads/{id}`                      | no   | Image bytes with the stored content type |
| follows | `POST /users/{id}/follow`                | yes  | Follow a user                            |
| follows | `DELETE /users/{id}/follow`              | yes  | Unfollow a user                          |
| follows | `GET /users/{id}/followers`              | no   | `[{id, username}]`                       |
| follows | `GET /users/{id}/following`              | no   | `[{id, username}]`                       |

Protected requests use `Authorization: Bearer <token>`.

Example:

```bash
curl -s -X POST http://bario.local/api/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"ada","password":"correct-horse-battery","email":"ada@example.com"}'

TOKEN=$(curl -s -X POST http://bario.local/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"ada","password":"correct-horse-battery"}' | jq -r .token)

curl -s -X POST http://bario.local/api/posts \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"content":"Hello, Bario."}'
```

## Metrics and isolation

Every service records, on its own `:9090/metrics` endpoint:

```
bario_http_requests_total{service, endpoint, method, status}
bario_http_request_duration_seconds{service, endpoint, method}   # histogram
```

The `endpoint` label is a canonical route name, not a raw path, for example:
`register`, `login`, `get_user`, `create_post`, `get_post`, `list_user_posts`,
`get_feed`, `upload_image`, `get_image`, `follow_user`, `unfollow_user`,
`list_followers`, `list_following`.

Each service's metrics port is exposed only through a `ClusterIP` Service named
`<release>-<service>-metrics`. These Services have labels
`app.kubernetes.io/component: <service>-metrics` and are **not** referenced by
the Ingress, which only routes `/api/auth`, `/api/posts`, `/api/uploads`,
`/api/follows`, and `/` to the frontend. There is no NodePort, LoadBalancer, or
public metrics path. Prometheus scrapes the metrics Services inside the cluster.

Inspect Prometheus locally with a port-forward:

```bash
kubectl -n bario port-forward svc/bario-prometheus 9090:9090
# then open http://localhost:9090 and query bario_http_requests_total
```

## Frontend layouts

The SPA has two presentations of the same components, switched in the header:

- **Classic** — a dense list with small type, square panels, and a sidebar,
  reminiscent of old Reddit.
- **Modern** — roomy rounded cards with larger type and more spacing.

The choice is stored in `localStorage` under `bario.layout` and applied as a
`layout-classic` / `layout-modern` class on the app root, so there is one
codebase and two CSS themes.

Pages: login, register, home feed, profile, single post, compose (with image
upload and preview).

## Make targets

| Target           | Action                                                        |
| ---------------- | ------------------------------------------------------------- |
| `make build`     | Build all Go services and the frontend production bundle      |
| `make test`      | `go vet` plus Vue/TypeScript type checking                    |
| `make docker-build` | Build the four service images and the frontend image       |
| `make helm-install` | `helm upgrade --install` into `NAMESPACE` (`bario`)        |
| `make helm-upgrade` | `helm upgrade` an existing release                         |
| `make helm-template` | Render the chart locally                                  |
| `make helm-lint` | Lint the chart                                                |

Override variables: `REGISTRY`, `TAG`, `NAMESPACE`, `RELEASE`, `HELM`.

## Notes

- Uploads live on a `ReadWriteOnce` PVC mounted at `/data/uploads`. Keep the
  uploads deployment at one replica while using that access mode.
- Uploaded images are limited to 10 MiB and must sniff as an `image/*` type.
- The feed intentionally contains posts from accounts you follow. Follow
  someone before expecting the home feed to fill up; a profile shows its own
  posts regardless.
- Passwords are bcrypt hashes; JWTs are HS256 with a 24-hour lifetime.
