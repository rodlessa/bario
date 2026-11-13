# uploads runbook

## What it does

`uploads` owns schema `uploads`, stores accepted image bytes on a PVC, and fans
out every upload to both moderation sidecars plus the hash-matcher seam before
deciding whether to store, reject, quarantine, or fail the request.

- API: `:8080`, routes `/uploads`, `/uploads/{id}` and under `/api/uploads`
- Metrics: `:9090`, service label `uploads`
- Database:
  - `uploads.images(id TEXT PK, user_id, filename, content_type, size,
    moderation_status, moderation_scores JSONB, created_at)`
  - `uploads.moderation_audit(...)` for rejected/failed uploads
  - `uploads.quarantine(...)` for hash-match quarantines
- Storage: PVC mounted at `/data/uploads` (5 GiB charter default); quarantine
  defaults to a sibling `/data/quarantine`, outside the public image tree
- Outbound: NSFWJS `POST :8085/classify`, CLIP gore `POST :8086/classify`
  (both with a 5-second timeout and 1 MiB response cap)
- Limits: 10 MiB max, formats JPEG/PNG/GIF/BMP/WebP

## Upload flow

```text
POST /uploads (JWT + multipart field "file")
  -> MaxBytesReader / ParseMultipartForm (413 or 400)
  -> sniff type; JPEG/PNG/GIF/BMP/WebP only (415 otherwise)
  -> generate 32-hex id
  -> concurrently call NSFWJS, gore CLIP, and hash matcher
  -> reject if risky >= threshold (451 + audit)
  -> classifier error: fail closed (503 + audit) unless MODERATION_FAIL_OPEN=true
  -> hash error: 503 always; hash match: quarantine + 451
  -> write bytes + insert uploads.images; 201 {id, url}
```

Defaults: `MODERATION_REJECT_THRESHOLD=0.7`, `GORE_REJECT_THRESHOLD=0.7`,
`MODERATION_INCLUDE_SEXY=false`, `MODERATION_FAIL_OPEN=false`. The NSFW risky
score is Porn+Hentai (plus Sexy only if enabled); the gore risky score is
graphic violence+gore+blood. Responses with probabilities that do not sum to
0.95–1.05 are treated as classifier failures.

`GET /uploads/{id}` is public, validates a 32-hex id, reads metadata from
`uploads.images`, serves the file, and returns
`Cache-Control: public, max-age=31536000, immutable` with `nosniff`.

## Health check

```bash
curl -fsS --resolve bario.local:80:192.168.122.92 \
  http://bario.local/api/uploads/healthz

kubectl -n bario get pods -l app.kubernetes.io/component=uploads
kubectl -n bario logs deploy/bario-uploads --tail=80
```

Upload smoke test:

```bash
TOKEN=$(curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"ada_lovelace\",\"password\":\"${SEED_PASSWORD}\"}" \
  http://bario.local/api/auth/login | jq -r .token)
curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H "Authorization: Bearer $TOKEN" \
  -F file=@/path/to/test.png http://bario.local/api/uploads
```

## Failure modes and response

| Symptom | Cause | Action |
| --- | --- | --- |
| `451 image rejected by content moderation` | risk score at/above threshold, or a hash match | Inspect `uploads.moderation_audit` (reason/scores) and `uploads.quarantine` for hash hits. This is a policy decision, not an infrastructure failure. |
| `503 image moderation is unavailable` | one/both sidecars failed or timed out, and `MODERATION_FAIL_OPEN=false` (default) | Check moderation pods/logs and their `/classify`; restore them, or intentionally set `MODERATION_FAIL_OPEN=true` if accepting unclassified images is acceptable for the environment. |
| Upload succeeds with `moderation_status='unavailable'` | sidecar failure while fail-open is true | Review sidecar health; fail-open is deliberate but means moderation was skipped. |
| `415 uploaded file is not a supported image` | ICO, TIFF, AVIF, or unknown bytes | Use JPEG/PNG/GIF/BMP/WebP. ICO is specifically rejected because Go may sniff it as `image/x-icon` and neither tfjs nor sharp decodes it. |
| `413 image must be at most 10 MiB` | >10 MiB body/image | Compress or resize before upload. |
| `400` missing/invalid multipart, empty file, unsupported/decode failure | bad request | Check multipart field name `file` and image bytes. |
| `500 could not store image` | filesystem/PVC write or DB insert failure | Check PVC mount/permissions and PostgreSQL; on DB failure the code removes the orphaned file. |
| Upload hangs near 5 seconds then 503 | sidecar timeout | Check moderation CPU/memory and `/classify` latency; both calls run in parallel. |
| `GET /uploads/{id}` 404 | DB row missing or file missing on the PVC | If the PVC was recreated, rows and bytes may no longer match; investigate storage/PVC recovery. |
| Images render as unsupported/blank after upload | format/decode mismatch between uploads and sidecars | Keep the three format lists identical (uploads, NSFWJS, CLIP). WebP is accepted and NSFWJS normalizes it with sharp; ICO must stay rejected. |

## Format trap (real project history)

The upload service, NSFWJS sidecar, and CLIP sidecar must agree on formats:

- JPEG, PNG, GIF, BMP, WebP are accepted by all three.
- TensorFlow.js cannot decode WebP; the Node sidecar converts WebP to raw sRGB
  with `sharp` before classification.
- Go's sniffer can report `image/x-icon` for ICO, but neither tfjs nor sharp
  decodes ICO, so uploads rejects it before any classifier call.
- TIFF/AVIF are likewise not in the allowlist and must return 415.

If you add a format, update all three format lists and the uploads allowlist in
the same change, and add a classification test.

## Redeploy

```bash
cd ~/chirp
docker build -f backend/services/uploads/Dockerfile \
  -t 127.0.0.1:5000/bario/uploads:latest backend
# push to the VM registry (platform.md)
helm upgrade bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 \
  --set ingress.className=traefik
```

Check the new pod, then upload a valid image (expect 201), an unsupported file
(expect 415), and verify both sidecars receive `/classify` traffic.

## Tests

```bash
cd backend && go test ./services/uploads/... -count=1
```
