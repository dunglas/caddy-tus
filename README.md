# Caddy tus: resumable uploads for Caddy and FrankenPHP

> [!WARNING]
>
> **Experimental.** This is a prototype built to evaluate resumable uploads for [FrankenPHP](https://frankenphp.dev).
> The configuration, the hook format and the module path may change or disappear without notice.
> Do not use it in production.

A [Caddy](https://caddyserver.com) HTTP handler that serves resumable uploads with [tusd](https://github.com/tus/tusd), the reference implementation of the [tus protocol](https://tus.io/protocols/resumable-upload).
It also accepts the [IETF Resumable Uploads draft](https://datatracker.ietf.org/doc/draft-ietf-httpbis-resumable-upload/), up to draft -05 (see [Limitations](#limitations)).

Uploads are written to disk by Caddy, so slow clients never hold a PHP thread.
PHP only runs for short hooks: authorizing an upload before it starts, and processing the stored file once it is complete.
Hooks are internal sub-requests sent to the next handler (typically `php_server`), with the client's cookies and headers, so sessions and auth work as usual.

## Install

With [xcaddy](https://github.com/caddyserver/xcaddy) and stock Caddy:

```console
xcaddy build --with github.com/dunglas/caddy-tus
```

With FrankenPHP, add `--with github.com/dunglas/caddy-tus` to the [FrankenPHP xcaddy command](https://frankenphp.dev/docs/compile/#using-xcaddy).
[`cmd/frankenphp-tus`](cmd/frankenphp-tus) is a ready-made FrankenPHP build including this module:

```console
cd cmd/frankenphp-tus
CGO_ENABLED=1 \
CGO_CFLAGS="$(php-config --includes)" \
CGO_LDFLAGS="$(php-config --ldflags) $(php-config --libs)" \
go build -tags=nobadger,nomysql,nopgx -o ../../frankenphp-tus .
```

## Configure

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
		# Set only on hook sub-requests, clients cannot forge it.
		env TUS_HOOK {http.vars.tus_hook}
	}
}
```

| Subdirective | Description |
| --- | --- |
| `base_path` | URL prefix of the upload endpoint. Default: `/files/` |
| `dir` | Directory where uploads are stored (required) |
| `max_size` | Maximum upload size, e.g. `1GB`. Default: unlimited |
| `ietf_draft` | Also accept the IETF Resumable Uploads draft |
| `enable_download` | Serve uploads with `GET`. Off by default: upload IDs are the only protection |
| `disable_termination` | Refuse `DELETE` |
| `hook_uri` | Path receiving hook sub-requests |
| `hooks` | Hooks sent to `hook_uri`: `pre-create`, `pre-finish`, `pre-terminate`. Default: `pre-create pre-finish` |

`tus` runs after `basic_auth` and `forward_auth`, so they also protect uploads.

## Hooks

Each hook is a `POST` to `hook_uri` with a JSON body in [tusd's HTTP hook format](https://tus.github.io/tusd/advanced-topics/hooks/):

```json
{"Type": "pre-finish", "Event": {"Upload": {"ID": "...", "Size": 3000000, "MetaData": {}, "Storage": {"Path": "/var/lib/uploads/..."}}, "HTTPRequest": {}}}
```

The Caddy variable `tus_hook` holds the hook type on hook sub-requests only.
Expose it to PHP with `env TUS_HOOK {http.vars.tus_hook}` and reject requests where it is empty: the hook script is otherwise reachable by anyone.

Responses:

- A non-2xx response rejects the operation and is sent to the client as is: `http_response_code(401)` is enough to refuse an upload.
- An empty 2xx response accepts it.
- A JSON body is read as tusd's hook response: `RejectUpload`, `ChangeFileInfo` (e.g. rewrite metadata), `HTTPResponse` (headers and body added to the client response).

```php
<?php
if (empty($_SERVER['TUS_HOOK'])) {
    http_response_code(404);
    exit;
}

$hook = json_decode(file_get_contents('php://input'), true, flags: JSON_THROW_ON_ERROR);
$upload = $hook['Event']['Upload'];

switch ($_SERVER['TUS_HOOK']) {
    case 'pre-create':
        session_start();
        if (!isset($_SESSION['user'])) {
            http_response_code(401);
        }
        break;

    case 'pre-finish':
        // The file is complete: move it, scan it, store a reference...
        rename($upload['Storage']['Path'], '/srv/files/'.$upload['ID']);
        break;
}
```

A complete example with an end-to-end `curl` test lives in [`example`](example).

## Limitations

- **IETF draft:** tusd supports interop versions 3 to 6 (drafts -01 to -05). The current draft (-15) uses interop version 10; such clients get `412 Precondition Failed`.
- **`pre-finish` runs at most once:** if the hook fails, the upload stays complete and a retried `PATCH` does not call it again ([tus/tusd#1299](https://github.com/tus/tusd/issues/1299)).
- **No access hook:** PHP authorizes upload creation only. Later requests on an upload are protected by its random ID and by any Caddy auth in front.
- **Synchronous hooks only:** post-finish and other asynchronous hooks are not supported.
- **Filesystem storage only:** no S3, no distributed locking, no expiration of abandoned uploads.

The full investigation, including the comparison with a FrankenPHP extension, is in [`docs/investigation.md`](docs/investigation.md).

## License

MIT
