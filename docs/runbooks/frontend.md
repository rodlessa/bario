# frontend runbook

## What it does

The frontend is a Vue 3 + TypeScript SPA built by Vite and served by
`nginx-unprivileged` on port 8080. Traefik routes `/` to it; the SPA calls the
services through `/api/...` on the same origin.

- Container: nginx on `:8080`; ClusterIP service `bario-frontend:80`
- No database; no direct service-to-service calls
- Layouts: classic/modern toggle stored in browser localStorage
- Feed: infinite scroll, 30-item batches, one batch prefetched ahead; ads are
  rendered by `AdItem`

## Health check

```bash
curl -fsS --resolve bario.local:80:192.168.122.92 \
  http://bario.local/healthz
# ok

curl -sSI --resolve bario.local:80:192.168.122.92 \
  http://bario.local/ | grep -Ei 'HTTP/|cache-control'

kubectl -n bario get pods -l app.kubernetes.io/component=frontend
kubectl -n bario logs deploy/bario-frontend --tail=50
```

Expected cache headers:

- `/` and SPA fallback routes: `Cache-Control: no-cache`
- `/assets/<hashed>.js|css`: `Cache-Control: public, immutable` with
  `max-age=31536000`

## Failure modes and response

| Symptom | Cause | Action |
| --- | --- | --- |
| Blank feed with two empty Vue placeholders and no error | old frontend bundle expects `{posts}` while the backend returns `{items}`; the render throws and the production build swallows it | Confirm the deployed asset hash and pod digest. Rebuild/push the frontend and run a Helm upgrade. This exact incident happened before the `items/kind` contract landed. |
| Browser shows an old app shell after a deploy | cached `index.html` or stale `:latest` pod | Hard-reload once; verify `Cache-Control: no-cache` on `/`. Confirm the pod digest and `bario.dev/revision` annotation. |
| Hashed asset 404 after deploy | HTML and `/assets` came from different builds (old pod/cache) | Reload `index.html`; confirm the new pod is serving both and that the asset filename matches the HTML. |
| Login form fails but API health is fine | auth/database or credentials | Check `/api/auth/healthz` and login directly with curl; see auth runbook. Re-seed if the database is empty. |
| Infinite scroll stops loading | end of feed (`next_cursor:null`) is expected; otherwise inspect network/ads/follows | Check `/api/posts/feed` manually and the inline "Having trouble loading more" state; see posts runbook. |
| Layout toggle has no effect | localStorage disabled or old bundle | Test in a private window; confirm the current bundle hash. |
| Page loads but images 404 | uploads PVC/data missing or image id wrong | Check uploads health and `/api/uploads/{id}`; see uploads runbook. |

## Rebuild / redeploy

```bash
cd ~/chirp
npm --prefix frontend run typecheck
npm --prefix frontend run build
docker build -t 127.0.0.1:5000/bario/frontend:latest frontend
# push to the VM registry (see platform.md)
helm upgrade bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 \
  --set ingress.className=traefik
```

Verify the new pod start time and digest, fetch `/`, and confirm the asset hash
in the HTML changed when the bundle changed. The chart uses `pullPolicy: Always`
plus a per-release `bario.dev/revision` annotation, so no manual image-tag dance
is required.

## Tests

There is currently no frontend unit/component test framework. The gate is:

```bash
npm --prefix frontend run typecheck
npm --prefix frontend run build
```

Manual browser checks: log in, feed renders posts + sponsored items, infinite
scroll loads batches, both classic and modern layouts work.
