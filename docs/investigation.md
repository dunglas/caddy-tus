# Investigation: resumable uploads in FrankenPHP with tusd

Written on 2026-11-12 while evaluating how FrankenPHP could offer resumable uploads.

## Recommendation

Ship a plain Caddy HTTP handler module (shape 2), maintained in its own repository like the Mercure and Vulcain modules, and optionally compiled into the official FrankenPHP builds behind a build tag.
The module mounts tusd on a path prefix, stores uploads itself, and calls PHP only through tusd hooks replayed as in-process sub-requests to the next handler (`php_server`).
It is pure Go with no dependency on FrankenPHP, so it builds against stock Caddy.

A Go extension (shape 1) cannot serve HTTP on its own, so it would be this same Caddy module plus PHP functions and an extension worker.
Add that layer later only if users ask for asynchronous post-finish processing or for reading S3-stored uploads without a PHP S3 SDK.

Two caveats come from tusd and apply to both shapes:

1. tusd implements the IETF draft only up to interop version 6 (draft -05).
   The current draft is -15 (2 November 2026), interop version 10.
   Enabling `EnableExperimentalProtocol` today interoperates with clients pinned to drafts -01 to -05 only; a client speaking interop 10 gets `412 Precondition Failed`.
   "IETF resumable uploads support" should not be advertised until tusd catches up (or we contribute it upstream).
2. `pre-finish` is at most once.
   If PHP fails during `pre-finish`, the client gets the error, the upload stays complete in the store, and a retried `PATCH` returns `204` without calling the hook again (reproduced below, upstream issue https://github.com/tus/tusd/issues/1299, open since 2025).
   Applications must reconcile completed uploads that were never processed, or the module must persist a "processed" marker and re-run the hook.

This repository is the prototype. It passed an end-to-end test with `curl` (tus v1 and IETF interop 6, including an interrupted and resumed upload).

## Prior art

No Caddy v2 module for tus or tusd was found.
Searches run on 2026-11-12:

- Caddy module registry (`https://caddyserver.com/api/packages`), filtered on "tus" and "upload": only `github.com/git001/caddyv2-upload` (multipart uploads, last update 2022, not tus) and an unrelated certificate uploader.
- GitHub repository search for `caddy tusd` and `caddy-tus`: no results.
- GitHub code search for Go files importing both `github.com/tus/tusd` and `github.com/caddyserver/caddy`: no Caddy module.

Caddy v1 plugins were not checked.

## tusd facts (v2.10.1, latest, 16 September 2026)

- `handler.Handler` is an `http.Handler` that expects the base path already stripped (tusd's own examples use `http.StripPrefix`); `Config.BasePath` is only used to build `Location`.
- IETF draft: `Config.EnableExperimentalProtocol`. Interop versions 3 to 6 are recognized (`pkg/handler/unrouted_handler.go`), mapping to drafts -01 to -05. The `main` branch still stops at interop 6.
- Upstream tracking: searching tusd issues and PRs (open and closed) for `IETF`, `RUFH`, `interop`, `resumable-upload` and `Upload-Complete` finds the work up to draft -05 (https://github.com/tus/tusd/pull/1213, merged) and later fixes on that code path (https://github.com/tus/tusd/pull/1397), but no issue, PR or branch for interop 7 or later.
- Hooks available as Go callbacks: `PreUploadCreateCallback` (can reject, change ID and metadata), `PreFinishResponseCallback` (can add headers/body to the final response), `PreUploadTerminateCallback`. Post-create, post-receive, post-finish and post-terminate are channels (`Notify*` flags) that the embedder must drain.
- There is no pre-access hook: PHP can gate upload creation, but `HEAD`, `PATCH`, `GET` and `DELETE` on an existing upload are protected only by the unguessable 128-bit ID and whatever Caddy matchers or auth directives run before the handler.
- Lockers: tusd ships `filelocker` and `memorylocker`. Neither is distributed.
- Expiration of abandoned uploads is not implemented (https://github.com/tus/tusd/issues/435, open).
- `pkg/hooks.NewHandlerWithHooks` was not used: it starts a goroutine that never exits (leaks on every Caddy config reload) and invokes post hooks asynchronously after the request context is gone.
- `Config.Logger` is a `golang.org/x/exp/slog` logger, a different type from `log/slog`, so it does not plug into Caddy's zap logger directly.

IETF interop versions per draft revision (from each revision's text on ietf.org):

| Drafts | Interop version | tusd |
| --- | --- | --- |
| -01 | 3 | yes |
| -02 | 4 | yes |
| -03 | 5 | yes |
| -04, -05 | 6 | yes |
| -06 to -08 | 7 | no |
| -09 to -11 | 8 | no |
| -12 | 9 | no |
| -13 to -15 | 10 | no |

Notable changes since -05 that tusd would need: `expires` limit renamed `max-age`, `Upload-Complete` required on append responses, structured field types for the new headers, `Upload-Limit` mandatory, a problem type for inconsistent lengths, and 104 interaction rules (draft -15, Appendix A).

## Shape 2: Caddy module (prototype)

### Layout

- Repository root (`github.com/dunglas/caddy-tus`): the module. Depends on Caddy, tusd, go-humanize, zap and x/exp only.
- `cmd/frankenphp-tus/`: a separate Go module building FrankenPHP plus the handler.
- `example/`: Caddyfile, PHP hook script, and `e2e.sh`.

### Caddyfile

```caddyfile
{
	frankenphp
}

example.com {
	root public

	tus {
		base_path /files/
		dir /var/lib/uploads
		max_size 1GB
		ietf_draft
		hook_uri /tus-hook.php
		hooks pre-create pre-finish
	}

	php_server {
		# Set only on hook sub-requests, so clients cannot forge it.
		env TUS_HOOK {http.vars.tus_hook}
	}
}
```

`tus` is ordered after `forward_auth`, so `basic_auth` and `forward_auth` protect uploads when configured.
The base path is a subdirective because a leading `/` argument is parsed by Caddy as a path matcher (exact match), which silently sent `PATCH /files/<id>` to PHP during testing.

### Upload store

- Filesystem: `dir` (tusd `filestore` + `filelocker`). Implemented. Adds only `github.com/tus/tusd/v2` and `github.com/tus/lockfile` to FrankenPHP's module graph; the binary grows by 443 KB (79,850,226 to 80,293,842 bytes, macOS arm64).
- S3 (not implemented): would add an `s3 { bucket, prefix, endpoint, region }` block mapped to `s3store.New(bucket, s3.NewFromConfig(...))`. This pulls `aws-sdk-go-v2` (about 20 modules, none currently in FrankenPHP's dependency graph). With `memorylocker` it is correct on a single instance only. Make S3 a separate build tag or a sibling module.
- Not handled: purging abandoned incomplete uploads.

### How PHP learns about uploads

Hooks are replayed as internal `POST` requests to `hook_uri`, served by the handlers after `tus` in the route (normally `php_server`):

- The body is tusd's HTTP hook JSON (`{"Type": "pre-finish", "Event": {"Upload": {...}, "HTTPRequest": {...}}}`), so existing tusd hook handlers and documentation apply.
- The sub-request copies the original request's headers (cookies, `Authorization`), `Host`, `RemoteAddr` and TLS state, so PHP sessions and auth work as in a normal request.
- It runs `caddyhttp.PrepareRequest` with a fresh replacer and copies the original Caddy vars (needed: `root` stores the document root there).
- It sets the Caddy var `tus_hook` to the hook type. Mapped to an env var in `php_server`, it tells PHP the request is a genuine hook. A client posting to `/tus-hook.php` directly gets no `TUS_HOOK` and the example script answers 404.
- Response mapping: non-2xx is forwarded to the client as the rejection (a plain `http_response_code(401)` works); empty 2xx accepts; a JSON body is decoded as tusd's `HookResponse` (`RejectUpload`, `ChangeFileInfo`, `HTTPResponse`).
- `pre-finish` runs after the data is stored and before the final response, so PHP opens `Upload.Storage.Path`, processes the file, and its response headers reach the client (the test returned `X-File-Sha256` computed by PHP). It is at most once, see the caveat above.

Only synchronous hooks are supported (`pre-create`, `pre-finish`, `pre-terminate`).
Post hooks fire after the client request ended, when the route and vars are gone; replaying them would need a request rebuilt from scratch through `Server.ServeHTTP`, or an extension worker (see shape 1).

The same sub-request mechanism should work in front of `php_fastcgi` (the var would be mapped with the FastCGI transport's `env`), but this was not tested.

Caddy placeholders do not help on their own: no PHP request runs during the `PATCH`, only the hook sub-request. Upload fields could additionally be exposed as vars (`{http.vars.tus_upload_path}`) for scripts that prefer `$_SERVER` over JSON.

### Auth

`pre-create` in PHP with the client's cookies and headers, as in the example.
Everything after creation relies on the random upload ID; pair with `forward_auth`/`basic_auth` or a matcher if that is not enough.
`GET` (download) is disabled by default (`enable_download` turns it on).

### Reproduce

Build `frankenphp-tus` as described in the README, then:

```console
cd example
mkdir -p uploads
../frankenphp-tus run --config Caddyfile &
head -c 3000000 /dev/urandom > /tmp/blob.bin
sh e2e.sh /tmp/blob.bin uploads/finished.log
```

### End-to-end results (curl, macOS arm64, PHP 8.5 ZTS)

| Step | Result |
| --- | --- |
| `POST /tus-hook.php` from outside | 404 (no `TUS_HOOK`) |
| tus create without `Authorization` | 401 with PHP's JSON body |
| tus create with auth | 201, metadata extended by PHP (`owner: alice`) |
| `PATCH` 3 MB rate-limited, killed after 2 s | `HEAD` reports offset 655360 |
| Resume `PATCH` from 655360 | 204, `X-File-Sha256` from PHP equals local sha256, PHP saw the session cookie |
| `GET` on the upload | 405 (download disabled) |
| `POST /files` (no trailing slash) | 201 |
| `pre-finish` returns 500, then `PATCH` retried at offset = length | first 500, `HEAD` offset 5 of 5, retry 204, hook not called again |
| IETF `POST` with interop 6, `Upload-Complete: ?1` | `104` then `201` through Caddy, PHP pre-finish ran |
| IETF create with `?0`, `HEAD`, `PATCH` `application/partial-upload` | completed, sha256 matches |
| IETF `POST` with interop 10 | 412 (falls back to tus v1, missing `Tus-Resumable`) |

The IETF rows were run by hand with `curl -v` (`Upload-Draft-Interop-Version: 6` or `10`), not by `e2e.sh`.
No `SetReadDeadline` errors through Caddy's response writer wrappers.
HTTP/2, HTTP/3, browsers (tus-js-client), `php_fastcgi` and Windows were not tested.

## Shape 1: Go extension

An extension ([extensions docs](https://frankenphp.dev/docs/extensions/)) is a Caddy module that also registers PHP functions; it cannot accept HTTP traffic outside Caddy's routes.
Running tusd from a PHP function (`tus_serve()` in a PHP script) would block a PHP thread for the whole upload, the problem this work avoids.
So shape 1 is shape 2 plus:

- PHP functions such as `frankenphp_tus_info(string $id): array` or `frankenphp_tus_open(string $id)` reading through tusd's store (useful for S3 without a PHP SDK).
- Asynchronous post-finish handling: drain `CompleteUploads` and call `Workers.SendMessage` ([extension workers docs](https://frankenphp.dev/docs/extension-workers/)) on a dedicated worker pool, so heavy processing (transcoding, virus scan) does not delay the client's final response and does not occupy regular request threads. It does not fix the at-most-once problem: tusd emits the event once.
- Auth would still be `pre-create` through a sub-request; an extension worker has no access to the user's session unless the worker uses `SendRequest` with the original headers, which is what the sub-request already does.

Costs: cgo and a PHP build for every user (no `xcaddy build` against stock Caddy), coupling to FrankenPHP's extension and worker APIs, unusable with `php_fastcgi`, and more code (C bridge, arginfo, stubs).

## Maintenance cost

- Shape 2: about 400 lines of Go, two new dependencies (filesystem store), follows tusd releases. Main ongoing work is the IETF draft gap and the pre-finish retry gap, both in tusd.
- Shape 1: shape 2 plus the extension layer, tests requiring a PHP build in CI, and tracking FrankenPHP extension API changes.

## Open questions

1. Who closes the IETF gap? Contribute interop 7 to 10 to tusd, wait for upstream, or document the feature as "tus v1, IETF draft -05".
2. Pre-finish retries: fix https://github.com/tus/tusd/issues/1299 upstream, or have the module store a "processed" flag (upload metadata or a sidecar file) and re-run `pre-finish` when a completed but unprocessed upload is retried?
3. Home: keep this repository, or move it under the `php` or `tus` organizations? Include it in the official FrankenPHP image (+443 KB without S3)?
4. Hook trust marker: the Caddy var plus `env` mapping needs one Caddyfile line. A FrankenPHP request option that always exposes `$_SERVER['TUS_HOOK']` would remove that line at the cost of coupling.
5. S3: ship in the main module behind a build tag or as a sibling module, and which locker for multi-instance deployments.
6. Post hooks: worth an extension worker integration, or are `pre-finish` plus an application-side queue enough?
7. Expiration of incomplete uploads (config option, or document a cron).
8. Logging: tusd logs through `x/exp/slog`; the prototype pipes warnings and errors through `zap.NewStdLog`, so they appear at info level. A proper bridge is about 50 lines.
9. Interaction with the new Go form parser: none needed. Uploads go to tusd's store and never through PHP's body parsing.

## Sources

- tusd repository and releases: https://github.com/tus/tusd, https://github.com/tus/tusd/releases
- tusd v2 Go API: https://pkg.go.dev/github.com/tus/tusd/v2/pkg/handler
- tusd IETF draft PRs: https://github.com/tus/tusd/pull/1213 (draft -04/-05), https://github.com/tus/tusd/pull/1097 (draft -03), https://github.com/tus/tusd/pull/1066, https://github.com/tus/tusd/pull/1397
- tusd issues: https://github.com/tus/tusd/issues/1299 (pre-finish on retry), https://github.com/tus/tusd/issues/435 (expiration)
- IETF draft: https://datatracker.ietf.org/doc/draft-ietf-httpbis-resumable-upload/, https://www.ietf.org/archive/id/draft-ietf-httpbis-resumable-upload-15.html, https://www.ietf.org/archive/id/draft-ietf-httpbis-resumable-upload-05.html
- tus v1 protocol: https://tus.io/protocols/resumable-upload
- Caddy module registry: https://caddyserver.com/api/packages
- Caddy `PrepareRequest`: https://pkg.go.dev/github.com/caddyserver/caddy/v2/modules/caddyhttp#PrepareRequest
- FrankenPHP extension docs: https://frankenphp.dev/docs/extensions/, https://frankenphp.dev/docs/extension-workers/
