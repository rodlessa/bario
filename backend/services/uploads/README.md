# Uploads service

Stores post images on disk and serves them back with their original content
type. Images are screened by two independent classifiers before they are
written: the NSFWJS sidecar and the CLIP gore/violence sidecar. The moderation
outcome and both score sets are stored with the image metadata.

## Moderation

Every upload that passes the image type check is POSTed as raw bytes to both
sidecars' `/classify` endpoints concurrently, each with a 5-second timeout.
Uploads are rejected when either rule fires:

```
NSFW: Porn + Hentai >= MODERATION_REJECT_THRESHOLD
Gore: graphic violence + gore + blood >= GORE_REJECT_THRESHOLD
```

`MODERATION_INCLUDE_SEXY=true` adds `Sexy` to the NSFW sum. The gore sum
deliberately excludes the neutral control prompts (`a normal photo`,
`everyday scene`).

| Env var | Default | Meaning |
| --- | --- | --- |
| `MODERATION_URL` | `http://localhost:8085` | NSFWJS sidecar base URL |
| `MODERATION_REJECT_THRESHOLD` | `0.7` | NSFW risky sum threshold (0–1) |
| `MODERATION_INCLUDE_SEXY` | `false` | Add `Sexy` to the NSFW risky sum |
| `GORE_MODERATION_URL` | `http://localhost:8086` | CLIP gore sidecar base URL |
| `GORE_REJECT_THRESHOLD` | `0.7` | Gore risky sum threshold (0–1) |
| `MODERATION_FAIL_OPEN` | `false` | Shared policy when either classifier fails |

There is one fail-open switch for both classifiers: `MODERATION_FAIL_OPEN`.
Leaving it `false` (the default) means an upload is rejected with `503` if
either sidecar errors, times out, or returns malformed probabilities. A
successful rejection from the other classifier still takes precedence and
returns `451`.

Each client enforces both an HTTP client timeout and a context deadline, so a
hung sidecar cannot hang uploads. The two calls run in parallel, so the normal
latency is the slower sidecar rather than the sum of both.

### Stored scores

`uploads.images.moderation_scores` and
`uploads.moderation_audit.moderation_scores` now use a combined shape:

```json
{
  "nsfw": {
    "Neutral": 0.895,
    "Drawing": 0.051,
    "Sexy": 0.003,
    "Hentai": 0.007,
    "Porn": 0.043
  },
  "gore": {
    "graphic violence": 0.029,
    "gore": 0.135,
    "blood": 0.005,
    "a normal photo": 0.751,
    "everyday scene": 0.080
  }
}
```

Rows written before this change may still contain the old flat NSFW-only
object; the column remains JSONB and needs no migration. If a classifier fails
while fail-open accepts the upload, its sub-object is `null` and the image is
stored with `moderation_status='unavailable'`.

### Outcomes

| Case | HTTP | `uploads.images` | `uploads.moderation_audit` | File written |
| --- | --- | --- | --- | --- |
| Both sums below thresholds | `201` | `approved`, both score sets | no | yes |
| Either sum at/above threshold | `451` | no row | `rejected`, both score sets, reason(s) | no |
| Any classifier error, fail closed (default) | `503` | no row | `error`, available scores, reason(s) | no |
| Any classifier error, `MODERATION_FAIL_OPEN=true` | `201` | `unavailable`, available scores (`null` for failed side) | no | yes |

A rejection returns:

```json
{"error":"image rejected by content moderation"}
```

A classifier failure with fail-closed returns:

```json
{"error":"image moderation is unavailable; try again later"}
```

Audit reasons are prefixed with the classifier name. If both classifiers
exceed their thresholds, both reasons are joined with `; `, for example:

```text
nsfw: porn+hentai=0.9123 >= threshold 0.7; gore: graphic_violence+gore+blood=0.8450 >= threshold 0.7
```

If one classifier rejects and the other errors, the reason includes both the
rejection and the error.

Rejected and failed uploads are recorded in `uploads.moderation_audit` rather
than `uploads.images`, so the images table only contains rows whose file exists
and can be served. Audit writes use a detached context so a client disconnect
cannot erase the trail.

The schema is migrated automatically on startup: `moderation_status` and
`moderation_scores` are added to `uploads.images`, and
`uploads.moderation_audit` is created if missing. Existing images default to
`moderation_status='approved'`.

## Supported formats

JPEG, PNG, GIF, BMP, and WebP. The uploads allowlist, the NSFWJS sidecar's
`src/images.js` (tfjs plus `sharp` for WebP), and the CLIP gore sidecar's
Pillow decoder all accept the same five formats. ICO (which Go sniffs as
`image/x-icon`), TIFF, and AVIF are rejected with `415` before moderation, so a
format cannot pass uploads and then fail inside either classifier.

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

Helm wiring for the sidecars is a follow-up. In a cluster, set
`MODERATION_URL` and `GORE_MODERATION_URL` to the sidecar Services (for
example `http://bario-moderation:8080` and
`http://bario-moderation-gore:8086`) before enabling uploads; until then
uploads fail closed with `503` when either classifier is unreachable.
