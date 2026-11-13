# posts runbook

## What it does

`posts` owns schema `posts` and the `posts.posts` table. It creates posts,
serves single posts and profile timelines, and builds the home feed from the
accounts the caller follows. The feed calls follows, auth, and ads.

- API: `:8080`, routes `/posts`, `/feed`, `/posts/{id}`, `/users/{id}/posts`
  and the same under `/api/posts`
- Metrics: `:9090`, service label `posts`
- Database: `posts.posts(id, user_id, content, image_id, created_at)` with
  index `(user_id, id DESC)`

## Feed contract and clients

`GET /feed?limit=≤100&cursor=&pages=` (auth required) returns:

```json
{"items": [{"kind": "post", "...": "..."},
           {"kind": "ad", "id": 1, "advertiser_id": 2,
            "title": "...", "body": "..."}],
 "next_cursor": "123"}
```

- Organic post order is newest first, filtered by the followed ids.
- Ads are fetched from `GET <ADS_URL>/ads/serve?pages=&limit=` with the caller's
  Authorization header forwarded. Ads are **not load-bearing**.
- Ads are re-rolled on every request and are interleaved between organic posts;
  `next_cursor` is derived only from the last organic post.
- `pages` scales ad density; the frontend sends 0 for the first page and
  increments it per loaded batch.
- The default page size from the frontend is 30 (`limit=30`).

## Health check

```bash
curl -fsS --resolve bario.local:80:192.168.122.92 \
  http://bario.local/api/posts/healthz

kubectl -n bario get pods -l app.kubernetes.io/component=posts
kubectl -n bario logs deploy/bario-posts --tail=50
```

Feed smoke test:

```bash
TOKEN=$(curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"ada_lovelace\",\"password\":\"${SEED_PASSWORD}\"}" \
  http://bario.local/api/auth/login | jq -r .token)
curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H "Authorization: Bearer $TOKEN" \
  'http://bario.local/api/posts/feed?limit=30&pages=0' | jq '{keys, items: (.items|length), kinds: (.items|map(.kind)), next_cursor}'
```

## Failure modes and degradation

| Symptom | Cause | Action |
| --- | --- | --- |
| feed returns `502 could not load the accounts you follow` | follows service call failed | This is correct/load-bearing. Check `bario-follows` health and logs; restore follows. Do not "fix" this by returning an organic-only feed. |
| feed returns 200 but has no ads | ads service error/timeout, no active ads, or ads inventory shortfall | Expected fail-open behavior. Check `bario-ads` and its logs/metrics if ads should exist; the feed must remain 200. |
| feed response has `posts` instead of `items` | stale posts image or client is pre-3e | Confirm the pod image digest and that the image was pushed; see platform.md pull-policy notes. |
| author names show `user-<id>` | auth username lookup failed | Best-effort fallback. Check auth health; feed still works. |
| 500 `could not load feed/post/posts` | DB query/scan error | Check PostgreSQL, `posts.posts` schema, and pod logs. |
| 400 `invalid pages` / invalid limit/cursor | bad query params | Valid limits are 1–100 (default 20 server-side); cursor is an organic post id. |
| feed page repeats/skips posts | cursor derived from ads or client reused a stale cursor | Cursor must be `next_cursor` from the server, which comes only from the last organic post. |
| duplicate ads on repeated requests | expected: ads are re-rolled every request; impressions are recorded per serve | Not a cursor bug. |

## Redeploy

```bash
cd ~/chirp
docker build -f backend/services/posts/Dockerfile \
  -t 127.0.0.1:5000/bario/posts:latest backend
# push to the VM registry (platform.md)
helm upgrade bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 \
  --set ingress.className=traefik
```

Verify the new pod start time/digest, then re-run the feed smoke test and check
that `items`/`kind` and the interleaved ad positions look correct. The chart
rolls app pods on every upgrade via `bario.dev/revision`.

## Tests

```bash
cd backend && go test -race ./services/posts/... -count=1
```

The suite covers interleaving, ads fail-open, pages/auth forwarding, cursor
pagination unaffected by ads, and the existing follows-down 502 behavior.
