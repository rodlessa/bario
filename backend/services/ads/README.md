# Ads service

Authenticated advertising campaign service for Bario. This scaffold implements
campaign creation and listing; serving, status changes, and click tracking are
follow-up tasks.

- **HTTP port:** `8087` (`HTTP_PORT`)
- **Metrics port:** `9097` (`METRICS_PORT`)
- **Schema:** `ads`
- **Auth:** bearer JWT, same issuer/secret as the other services

## Data model

`ads.advertisements`:

```text
id            BIGSERIAL PRIMARY KEY
advertiser_id BIGINT NOT NULL
title         TEXT NOT NULL
body          TEXT NOT NULL
target_tags   TEXT[] NOT NULL DEFAULT '{}'
budget_cents  BIGINT NOT NULL CHECK (budget_cents >= 0)
status        TEXT NOT NULL DEFAULT 'active'
created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
```

`ads.impressions` (`id`, `ad_id`, `user_id`, `served_at`) and `ads.clicks`
(`id`, `ad_id`, `user_id`, `clicked_at`) already exist for the later serving
and click tasks. `ad_id` references `ads.advertisements(id)` with
`ON DELETE CASCADE`, and indexes are present on the lookup columns.

## Endpoints implemented

### `POST /ads` (also `/api/ads`)

Requires `Authorization: Bearer <token>`. The advertiser is always the JWT
user, not a client-supplied field.

```bash
curl -s -X POST http://localhost:8087/ads \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Launch week","body":"Try the new timeline.","target_tags":["tech","go"],"budget_cents":5000}'
```

Validation:

- `title` is trimmed and must be 1–120 characters.
- `body` is trimmed and must be 1–2000 characters.
- `budget_cents` must be `>= 0`.
- `target_tags` is optional and may be empty.

Returns `201`:

```json
{
  "id": 1,
  "advertiser_id": 7,
  "title": "Launch week",
  "body": "Try the new timeline.",
  "target_tags": ["tech", "go"],
  "budget_cents": 5000,
  "status": "active",
  "created_at": "2026-11-12T00:00:00Z",
  "updated_at": "2026-11-12T00:00:00Z"
}
```

### `GET /ads` (also `/api/ads`)

Requires auth. Returns a JSON array of only the caller's advertisements,
newest first (`id DESC`). An advertiser with no campaigns gets `[]`.

```bash
curl -s http://localhost:8087/ads -H "Authorization: Bearer $TOKEN"
```

### `PATCH /ads/{id}` (also `/api/ads/{id}`)

Requires auth and ownership. Only the ad's `advertiser_id` can update it.

```bash
curl -s -X PATCH http://localhost:8087/ads/1 \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"status":"paused"}'
```

The status enum is intentionally small for now: only `active` and `paused`
are accepted; any other value returns `400`. Updating another advertiser's ad
or a nonexistent id returns `404` (the two cases are indistinguishable to
avoid leaking which ids exist). Success returns the full updated ad and
advances `updated_at`.

### `GET /healthz` (also `/api/ads/healthz`)

Returns `{"status":"ok"}`.

Not implemented yet: `POST /ads/{id}/click`, `GET /ads/serve`.

## Run locally

The service uses the same environment variables as the other Go services:

```bash
export DATABASE_URL='postgres://bario:...@localhost:5432/bario?sslmode=disable'
export JWT_SECRET="$(openssl rand -hex 32)"
cd backend/services/ads
go run .
```

Migrations run automatically on startup. Override `HTTP_PORT` and
`METRICS_PORT` if needed. Metrics are exposed separately at
`http://localhost:9097/metrics` with endpoint labels `create_ad`, `list_ads`,
and `update_ad` (plus `health`).

## Tests

```bash
cd backend/services/ads
go test ./... -v -count=1
```

Tests require Docker and Go only. `TestMain` starts an ephemeral
`postgres:16-alpine` container on a random local port, applies `adsMigrations`,
and removes it afterward; handlers are driven with `httptest`.
