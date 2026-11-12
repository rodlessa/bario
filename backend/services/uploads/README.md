# Uploads service

Stores post images on disk and serves them back with their original content
type. Images are screened by the NSFWJS moderation sidecar before they are
written, and the moderation outcome is stored with the image metadata.

## Moderation

Every upload that passes the image type check is POSTed as raw bytes to the
sidecar's `/classify` endpoint with a 5-second timeout. Uploads are rejected
when:

```
Porn + Hentai >= MODERATION_REJECT_THRESHOLD
```

Setting `MODERATION_INCLUDE_SEXY=true` adds `Sexy` to that sum.

| Env var | Default | Meaning |
| --- | --- | --- |
| `MODERATION_URL` | `http://localhost:8085` | Sidecar base URL |
| `MODERATION_REJECT_THRESHOLD` | `0.7` | Reject at or above this risky sum (0–1) |
| `MODERATION_INCLUDE_SEXY` | `false` | Include `Sexy` in the risky sum |
| `MODERATION_FAIL_OPEN` | `false` | Accept the upload if the classifier itself fails |

The 5-second limit is enforced with both an HTTP client timeout and a context
deadline, so a hung sidecar cannot hang uploads.

### Outcomes

| Case | HTTP | `uploads.images` | `uploads.moderation_audit` | File written |
| --- | --- | --- | --- | --- |
| Risky sum below threshold | `201` | `moderation_status='approved'`, scores stored | no | yes |
| Risky sum at/above threshold | `451` | no row | `rejected`, scores stored | no |
| Classifier error, fail closed (default) | `503` | no row | `error`, scores null | no |
| Classifier error, `MODERATION_FAIL_OPEN=true` | `201` | `moderation_status='unavailable'`, scores null | no | yes |

A rejection returns:

```json
{"error":"image rejected by content moderation"}
```

A classifier failure with fail-closed returns:

```json
{"error":"image moderation is unavailable; try again later"}
```

Rejected and failed uploads are recorded in `uploads.moderation_audit` rather
than `uploads.images`, so the images table only contains rows whose file exists
and can be served. The audit row keeps the upload id, user, filename, content
type, size, status, raw scores, reason, and timestamp. Audit writes use a
detached context so a client disconnect cannot erase the trail.

The schema is migrated automatically on startup: `moderation_status` and
`moderation_scores` are added to `uploads.images`, and
`uploads.moderation_audit` is created if missing. Existing images default to
`moderation_status='approved'`.

## Supported formats

JPEG, PNG, GIF, BMP, and WebP. The list is identical to the moderation
sidecar's `src/images.js`: ICO (which Go sniffs as `image/x-icon`), TIFF, and
AVIF are rejected with `415` before moderation, so a format cannot pass
uploads and then fail inside the classifier. WebP is decoded by the sidecar
with `sharp`.

## API

`POST /uploads` (also `/api/uploads`) takes a multipart field named `file`,
authenticated with a bearer token:

```bash
curl -F file=@test.jpg \
  -H "Authorization: Bearer $TOKEN" \
  http://localhost:8084/uploads
```

Success returns the existing shape:

```json
{"id":"<32-hex-id>","url":"/api/uploads/<32-hex-id>"}
```

`GET /uploads/{id}` serves the stored bytes with the original content type.

## Kubernetes note

Helm wiring for the moderation sidecar is a follow-up. In a cluster, set
`MODERATION_URL` to the sidecar Service (for example
`http://bario-moderation:8080`) before enabling uploads; until then uploads
fail closed with `503` when the classifier is unreachable.
