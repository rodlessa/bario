# ads runbook

## What it does

`ads` owns schema `ads` and serves advertising campaigns. It supports campaign
create/list/owner-only status updates, scroll-scaled serving for the posts feed,
impression recording, and click recording. It reads caller interests from auth.

- API: `:8087`, routes `/ads`, `/ads/{id}`, `/ads/serve`, `/ads/{id}/click`
  and the same under `/api/ads`
- Metrics: `:9097`, service label `ads`
- Database:
  - `ads.advertisements(id, advertiser_id, title, body, target_tags TEXT[],
    budget_cents, status, created_at, updated_at)`
  - `ads.impressions(id, ad_id FK, user_id, served_at)`
  - `ads.clicks(id, ad_id FK, user_id, clicked_at)`
- Outbound: auth `GET /users/{id}` for caller interests; best-effort

## Serving algorithm

`GET /ads/serve?pages=&limit=` (auth required):

```text
ratio    = min(0.30, 0.05 + 0.02 * pages)
ad_count = round(ratio * limit)
```

1. Active ads whose `target_tags` overlap caller interests, newest first.
2. Active ads with empty `target_tags`, filling the remaining slots.
3. Fewer active ads than requested is normal; return what exists.
4. Insert one `impressions` row per served ad (best-effort; insert failures are
   logged and do not fail the response).

Posts calls this endpoint and forwards the caller's bearer token. Ads are
fail-open from posts' perspective: an ads error/timeout must never turn the feed
into a 5xx.

## Health check

```bash
curl -fsS --resolve bario.local:80:192.168.122.92 \
  http://bario.local/api/ads/healthz

kubectl -n bario get pods -l app.kubernetes.io/component=ads
kubectl -n bario logs deploy/bario-ads --tail=50
```

Authenticated smoke test:

```bash
TOKEN=$(curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"ada_lovelace\",\"password\":\"${SEED_PASSWORD}\"}" \
  http://bario.local/api/auth/login | jq -r .token)
curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H "Authorization: Bearer $TOKEN" \
  'http://bario.local/api/ads/serve?pages=0&limit=30' | jq
```

## Failure modes and response

| Symptom | Cause | Action |
| --- | --- | --- |
| feed has no ads but posts returns 200 | ads error/timeout, no active inventory, or auth interests unavailable | Expected fail-open. Check ads logs/metrics; the feed is intentionally still 200. |
| `/ads/serve` returns 500 `could not load ads` | DB query error | Check PostgreSQL and ads logs. |
| `/ads/serve` returns 400 `limit is required` / `invalid pages` | missing/non-numeric params | Send numeric `limit`; `pages` defaults to 0 and negatives clamp to 0. |
| authenticated `/ads/serve` gets only general ads | auth interests lookup failed or user has no interests | Targeted fallback is expected; check auth only if interests should exist. |
| click returns 404 | ad id does not exist | Expected; the insert is guarded by `WHERE EXISTS`. |
| click returns 500 | DB error | Check PostgreSQL and ads logs. |
| create returns 400 | title/body/budget validation | Title 1–120, body 1–2000, budget ≥0. |
| update returns 404 | ad missing or caller is not the advertiser | Correct privacy behavior; not-found and not-owned are intentionally indistinguishable. |
| ads panels in Grafana empty but health is OK | Prometheus `bario-ads` scrape job missing/stale, or no ads traffic | Check Grafana/Prometheus; the chart includes `bario-ads` at `:9097`. A config change requires the Prometheus checksum rollout; see platform.md. |

## Redeploy

```bash
cd ~/chirp
docker build -f backend/services/ads/Dockerfile \
  -t 127.0.0.1:5000/bario/ads:latest backend
# push to the VM registry (platform.md)
helm upgrade bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 \
  --set ingress.className=traefik
```

After rollout, verify ads health, `/ads/serve` with a real token, and the posts
feed contains `kind:"ad"` items when inventory exists.

## Tests

```bash
cd backend && go test ./services/ads/... -count=1
```
