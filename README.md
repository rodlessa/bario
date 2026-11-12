# Bario

Bario is a small Twitter-like social network: short posts, image attachments,
follow/unfollow, and a home feed. It is built as five Go services behind a Vue 3
SPA, deployed to local Kubernetes with a Helm chart. Every service exposes
Prometheus metrics on a private, cluster-only port.

## Stack

| Layer    | Tech                                                     |
| -------- | -------------------------------------------------------- |
| Backend  | Go (5 services: auth, posts, uploads, follows, ads)      |
| Moderation | NSFWJS + CLIP gore sidecars (screen uploads)             |
| Frontend | Vue 3 + TypeScript + Vite, served by nginx               |
| Database | PostgreSQL 16, one schema per service                    |
| Deploy   | Docker images + Helm chart + nginx Ingress               |
| Metrics  | Prometheus, scraping cluster-only `*-metrics` Services   |

Each Go service listens on `:8080` for JSON and `:9090` for `/metrics`. Both
ports come from `HTTP_PORT` and `METRICS_PORT`. The moderation sidecars use
`:8085`/`:9095` (NSFWJS) and `:8086`/`:9096` (CLIP gore) locally; the uploads
service calls them at `MODERATION_URL` and `GORE_MODERATION_URL`. See
`backend/services/moderation/README.md`,
`backend/services/moderation-gore/README.md`, and
`backend/services/uploads/README.md`.

Local development ports:

| Service | HTTP | Metrics |
| ------- | ---- | ------- |
| auth | 8081 | 9091 |
| posts | 8082 | 9092 |
| follows | 8083 | 9093 |
| uploads | 8084 | 9094 |
| moderation (NSFWJS) | 8085 | 9095 |
| moderation-gore (CLIP) | 8086 | 9096 |
| ads | 8087 | 9097 |

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
│       ├── follows/
│       ├── ads/              # campaign create/list scaffold
│       ├── moderation/       # Node + NSFWJS sidecar (uploads moderation)
│       └── moderation-gore/  # Python + CLIP gore sidecar
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
docker build -t bario/moderation:latest backend/services/moderation
docker build -t bario/moderation-gore:latest backend/services/moderation-gore
kind load docker-image \
  bario/auth:latest bario/posts:latest bario/uploads:latest \
  bario/follows:latest bario/frontend:latest \
  bario/moderation:latest bario/moderation-gore:latest --name bario

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
# Prefix every chart-built image (services.* and frontend) with one registry:
helm upgrade --install bario deploy/bario \
  --namespace bario --create-namespace \
  --set global.registry=127.0.0.1:5000
# or point the Makefile at the registry (it overrides each image repository):
make docker-build REGISTRY=registry.example.com/bario TAG=1.0.0
make helm-install \
  REGISTRY=registry.example.com/bario TAG=1.0.0
# or override each service image explicitly with --set services.auth.image.repository=...
```

`global.registry` is a plain prefix (host and optional port), so the default
`bario/auth` becomes `127.0.0.1:5000/bario/auth`. Upstream images (`postgres`,
`prom/prometheus`) are not affected. For the Orange Pi cluster, build and push
multi-arch images with `make docker-buildx` before installing the chart.

Workload scheduling is configurable per service under `services.<name>`,
`frontend`, `postgres` and `prometheus` (`nodeSelector`, `affinity`,
`tolerations`). All default to empty, which preserves the previous behavior.
For the Orange Pi split, set the heavy workloads (`moderation`,
`moderation-gore`, `postgres`) to the master and the light ones (`auth`,
`posts`, `uploads`, `follows`, `frontend`) to the workers, e.g.:

```bash
helm upgrade --install bario deploy/bario --namespace bario \
  --set services.moderation.nodeSelector.node-type=heavy \
  --set services.moderation-gore.nodeSelector.node-type=heavy \
  --set postgres.nodeSelector.node-type=heavy \
  --set services.auth.nodeSelector.node-type=light \
  --set services.posts.nodeSelector.node-type=light \
  --set services.uploads.nodeSelector.node-type=light \
  --set services.follows.nodeSelector.node-type=light \
  --set frontend.nodeSelector.node-type=light
```

If the master is tainted with `node-role.kubernetes.io/control-plane:NoSchedule`,
also add a matching `tolerations` entry to the heavy workloads.

The chart deploys the two moderation sidecars as internal-only ClusterIP
services (`moderation` on 8085/9095 and `moderation-gore` on 8086/9096). Set
their images and resources under `services.moderation` and
`services.moderation-gore`; the uploads thresholds and fail-open behavior come
from `uploads.moderation.*`. Neither sidecar nor its metrics port is exposed
through the Ingress.

`make docker-build` builds the Go services and frontend only. Build the
sidecars separately with the commands in the quick start, and tag them with
the same `REGISTRY`/`TAG` when overriding images.

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

Run the Go services on separate ports:

```bash
cd backend

(cd services/auth && HTTP_PORT=8081 METRICS_PORT=9091 go run .) &
(cd services/follows && HTTP_PORT=8083 METRICS_PORT=9093 \
  AUTH_URL=http://localhost:8081 go run .) &
(cd services/posts && HTTP_PORT=8082 METRICS_PORT=9092 \
  AUTH_URL=http://localhost:8081 FOLLOWS_URL=http://localhost:8083 go run .) &
(cd services/uploads && HTTP_PORT=8084 METRICS_PORT=9094 \
  UPLOAD_DIR=/tmp/bario-uploads go run .) &
(cd services/ads && HTTP_PORT=8087 METRICS_PORT=9097 go run .) &
```

The optional moderation sidecars are separate services: NSFWJS on `:8085`
(metrics `:9095`) and CLIP gore on `:8086` (metrics `:9096`). The uploads
service calls both for every image; start them before uploading, otherwise
uploads fail closed with `503`. See
`backend/services/moderation/README.md`,
`backend/services/moderation-gore/README.md`, and
`backend/services/uploads/README.md` to run them.

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

### Seed data

With the four services running, `scripts/seed.sh` registers the reserved admin
account plus nine demo users, wires an organic follow graph, and creates 3–8
posts per user through the public HTTP APIs. It never writes to PostgreSQL
directly.

```bash
scripts/seed.sh
```

The script is safe to rerun: existing accounts are logged in, follows are
idempotent, and posts are only added up to each user's target. Override the
endpoints or credentials with `AUTH_URL`, `POSTS_URL`, `FOLLOWS_URL`,
`ADMIN_USERNAME`, and `SEED_PASSWORD`; the defaults match the ports and admin
username above. Requires `bash`, `curl`, and `jq`.

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

Uploads are screened by the moderation sidecar. Images whose risky score meets
`MODERATION_REJECT_THRESHOLD` return `451` and are not stored; classifier
failures fail closed unless `MODERATION_FAIL_OPEN=true`. See
`backend/services/uploads/README.md`.

### CSAM and abuse detection

Bario's image moderation does **not** detect child sexual abuse material
(CSAM). NSFWJS classifies pornographic and hentai imagery, and the CLIP
zero-shot classifier covers graphic violence and gore; neither model can
identify CSAM. No free or open-source ML detector for CSAM exists that we can
self-host today. The uploads service exposes a hash-matcher seam, but it is a
no-op and ships no CSAM detection.

Production CSAM detection requires a known-abuse hash corpus and a legal
reporting relationship. The supported paths are Microsoft's PhotoDNA Cloud
Service (free for qualifying non-profit/NGO and some commercial organizations;
hash matching against a known-abuse-image database, via Microsoft/NCMEC) and
direct integration with the NCMEC CyberTipline reporting API. Both require
registration or an agreement with the relevant organization; Bario does not
ship either client. See
[`backend/services/uploads/README.md`](backend/services/uploads/README.md) for
the quarantine seam.

Protected requests use `Authorization: Bearer <token>`.

Example:

```bash
curl -s -X POST http://bario.local/api/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"ada","password":"<password>","email":"ada@example.com"}'

TOKEN=$(curl -s -X POST http://bario.local/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"ada","password":"<password>"}' | jq -r .token)

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
| `make guard`     | Deterministic secret scan of staged changes                   |
| `make docker-build` | Build the four service images and the frontend image       |
| `make docker-buildx` | Build and push multi-arch images (`linux/amd64,linux/arm64`) |
| `make helm-install` | `helm upgrade --install` into `NAMESPACE` (`bario`)        |
| `make helm-upgrade` | `helm upgrade` an existing release                         |
| `make helm-template` | Render the chart locally                                  |
| `make helm-lint` | Lint the chart                                                |

Override variables: `REGISTRY`, `TAG`, `PLATFORMS`, `BUILDX_BUILDER`,
`NAMESPACE`, `RELEASE`, `HELM`.

## Notes

- Uploads live on a `ReadWriteOnce` PVC mounted at `/data/uploads`. Keep the
  uploads deployment at one replica while using that access mode.
- Uploaded images are limited to 10 MiB and must sniff as an `image/*` type.
- The feed intentionally contains posts from accounts you follow. Follow
  someone before expecting the home feed to fill up; a profile shows its own
  posts regardless.
- Passwords are bcrypt hashes; JWTs are HS256 with a 24-hour lifetime.

## Security

Secret scanning is deterministic and fully local: no LLM, no network. The
scanner reports `path:line [rule]` and never prints the secret value itself.

- `make guard` (or `sh scripts/secret_scan.sh --staged`) scans the staged
  changes and exits non-zero when it finds provider API keys, PEM/OpenSSH
  private keys, non-empty `.env` values, credential-style assignments, or long
  high-entropy tokens in assignment or bearer contexts.
- Pass file paths to scan files in full instead:
  `sh scripts/secret_scan.sh path/to/file`.
- Install the pre-commit hook if you want every commit checked:

  ```bash
  cp scripts/pre-commit .git/hooks/pre-commit
  chmod +x .git/hooks/pre-commit
  ```

- Mark a deliberate fixture with a `secret-scan:allow` comment on that line.
  If a finding is ambiguous, escalate the flagged lines to Nano
  (`maestri ask "Nano" "<flagged lines>"`) before deciding. Remove and rotate
  real credentials; never commit them.
