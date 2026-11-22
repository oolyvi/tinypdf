# PDF Compressor

A small, self-hosted web app that shrinks PDFs to a size you choose. Upload a file, pick a target reduction (1–90%), and
download the result. No accounts, no database, and nothing is kept on disk after the request finishes.

- **Target-based:** you ask for "50% smaller", not for a vague quality preset.
- **Least quality loss:** searches for the gentlest settings that still hit your target.
- **Safe fallback:** if a PDF can't be shrunk, you get the original back instead of a bigger file.
- **Single container:** one Go binary plus Ghostscript; the UI is embedded (English and Azerbaijani, light/dark mode).

## Quick start

```bash
docker compose up -d --build
```

Open <http://localhost:8080>.

## How it works

1. **Upload:** the file is streamed to a private temp directory and checked for a `%PDF-` header.
2. **Binary search over quality:** Ghostscript re-renders the PDF at a "compression level" from 0 to 100, which maps to
   image DPI and JPEG quality. Up to 6 passes narrow in on the target size.
3. **Pick the best result:** the largest output that is still at or under the target wins. The search stops early once
   it lands within 2% of the original size below the target.
4. **Lossless polish:** [pdfcpu](https://github.com/pdfcpu/pdfcpu) optimizes the result further, and is only used if it
   actually makes the file smaller.
5. **Clean up:** the temp directory is deleted as soon as the response is sent.

| Level | Image DPI | JPEG quality |
|------:|----------:|-------------:|
|     0 |       300 |           95 |
|    50 |       175 |           58 |
|   100 |        50 |           20 |

Fonts are subset and compressed, and duplicate images are merged at every level.

> **Heads up:** only images are downsampled. A PDF that is mostly text or vector graphics may not reach a high target.
> In that case the app returns the smallest result it found (and reports `X-Target-Met: false`), or the original file if
> nothing smaller was possible.

## Installation

### Docker (recommended)

With Compose, configured via environment variables or a `.env` file (copy [`.env.example`](.env.example)):

```bash
docker compose up -d --build
```

Without Compose:

```bash
docker build -t pdf-compress .
docker run -d --name pdf-compress -p 8080:8080 pdf-compress
```

Cross-building (for example on an Apple Silicon Mac for an x86 server):

```bash
docker buildx build --platform linux/amd64 -t pdf-compress .
```

The image runs as a non-root user, and the Compose file drops all capabilities, sets `no-new-privileges`, and caps
memory at 2 GB so a single huge PDF can't take down the host.

### From source

Requires **Go 1.27+** and **Ghostscript** on your `PATH`.

```bash
# macOS
brew install ghostscript
# Debian / Ubuntu
sudo apt install ghostscript

go run .
```

## Configuration

All settings are environment variables.

| Variable              | Default                         | Description                                                               |
|-----------------------|---------------------------------|---------------------------------------------------------------------------|
| `PORT`                | `8080`                          | Port the server listens on (with Compose: the published host port)        |
| `MAX_UPLOAD_MB`       | `0`                             | Maximum upload size in MB (`0` = unlimited)                               |
| `MAX_CONCURRENT_JOBS` | number of CPUs (`2` in Compose) | Simultaneous compressions; extra requests wait up to 30 s, then get `503` |
| `JOB_TIMEOUT_SECONDS` | `300`                           | Per-request compression deadline                                          |
| `GS_BIN`              | auto-detected                   | Path to Ghostscript (tries `gs`, `gswin64c`, `gswin32c`)                  |

## API

### `POST /compress`

Multipart form fields:

| Field              | Type   | Description                                                          |
|--------------------|--------|----------------------------------------------------------------------|
| `pdf`              | file   | The PDF to compress (required)                                       |
| `target_reduction` | number | Desired size reduction in percent. Default `50`, clamped to `1`–`90` |

```bash
curl -X POST http://localhost:8080/compress \
  -F "pdf=@report.pdf" \
  -F "target_reduction=60" \
  -D - -o compressed_report.pdf
```

On success the response body is the PDF (named `compressed_<original name>`), with these headers:

| Header                    | Meaning                                           |
|---------------------------|---------------------------------------------------|
| `X-Original-Size`         | Input size in bytes                               |
| `X-Compressed-Size`       | Output size in bytes                              |
| `X-Compression-Reduction` | Actual reduction achieved, in percent             |
| `X-Target-Met`            | `true` if the actual reduction reached the target |

Error responses are plain text:

| Status | Cause                                                                  |
|-------:|------------------------------------------------------------------------|
|    400 | Malformed request or no PDF supplied                                   |
|    413 | File exceeds `MAX_UPLOAD_MB`                                           |
|    422 | Not a PDF, or Ghostscript couldn't process it (corrupted or encrypted) |
|    503 | Server busy; retry after the `Retry-After` interval                    |
|    504 | Compression exceeded `JOB_TIMEOUT_SECONDS`                             |

### `GET /healthz`

Returns `200 ok`. The Docker image uses it for its `HEALTHCHECK` (via `pdf_compress -healthcheck`).

### `GET /`

The web UI.

## Deployment notes

- **Use HTTPS.** Put the container behind a reverse proxy that terminates TLS (Caddy, Nginx, Traefik, or your cloud load
  balancer).
- **Raise proxy limits** to match your settings: for Nginx, `client_max_body_size` (≥ `MAX_UPLOAD_MB`) and
  `proxy_read_timeout` (≥ `JOB_TIMEOUT_SECONDS`).
- **There is no authentication.** If the service is public, add auth and rate limiting at the proxy, and set
  `MAX_UPLOAD_MB` to something sensible.
- **Size the host for Ghostscript.** Each job can use significant CPU and RAM. Tune `MAX_CONCURRENT_JOBS` and the
  Compose `mem_limit` together.
- Works out of the box on Fly.io, Railway, Render, Cloud Run, or any VPS with Docker; just route traffic to `$PORT`.

## Development

```bash
go test ./...
```

The Ghostscript end-to-end test is skipped automatically when `gs` isn't installed.

```
main.go                config, startup, graceful shutdown, -healthcheck flag
internal/compress/     Ghostscript wrapper and size-targeting search
internal/server/       HTTP handlers, upload handling, embedded single-page UI
```