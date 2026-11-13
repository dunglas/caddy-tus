// Package tus is a Caddy HTTP handler serving resumable uploads (tus v1 and
// the IETF resumable upload draft) with tusd, and calling the next handler
// (e.g. php_server) as an in-process hook endpoint.
package tus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/hooks"
	"go.uber.org/zap"
	"golang.org/x/exp/slog"
)

const (
	hookPreCreate    = string(hooks.HookPreCreate)
	hookPreFinish    = string(hooks.HookPreFinish)
	hookPreTerminate = string(hooks.HookPreTerminate)

	// HookVar is the Caddy variable set on hook sub-requests only.
	// Clients cannot set it, so PHP can trust it (map it with php_server's env).
	HookVar = "tus_hook"

	maxHookResponseSize = 1 << 20
)

var syncHooks = []string{hookPreCreate, hookPreFinish, hookPreTerminate}

func init() {
	caddy.RegisterModule(Tus{})
}

// Tus serves resumable uploads under BasePath and passes every other request to the next handler.
type Tus struct {
	// BasePath is the URL prefix of the upload endpoint, e.g. "/files/".
	BasePath string `json:"base_path,omitempty"`
	// Dir is the filesystem directory where uploads are stored.
	Dir string `json:"dir,omitempty"`
	// MaxSize is the maximum size of an upload in bytes, 0 means unlimited.
	MaxSize int64 `json:"max_size,omitempty"`
	// IETFDraft enables the IETF resumable upload draft next to tus v1.
	IETFDraft bool `json:"ietf_draft,omitempty"`
	// EnableDownload exposes uploads through GET; uploads are only protected by their random ID.
	EnableDownload bool `json:"enable_download,omitempty"`
	// DisableTermination disables DELETE.
	DisableTermination bool `json:"disable_termination,omitempty"`
	// HookURI is the path the next handler receives hook sub-requests on, e.g. "/tus-hook.php".
	HookURI string `json:"hook_uri,omitempty"`
	// Hooks lists the hooks sent to HookURI. Defaults to pre-create and pre-finish.
	Hooks []string `json:"hooks,omitempty"`

	hookURL *url.URL
	handler *tusd.Handler
	logger  *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (Tus) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.tus",
		New: func() caddy.Module { return new(Tus) },
	}
}

// Provision implements caddy.Provisioner.
func (t *Tus) Provision(ctx caddy.Context) error {
	t.logger = ctx.Logger()

	if t.BasePath == "" {
		t.BasePath = "/files/"
	}
	if !strings.HasSuffix(t.BasePath, "/") {
		t.BasePath += "/"
	}
	if t.Dir == "" {
		return errors.New("tus: dir is required")
	}
	// Hooks receive Storage.Path, which must not depend on the working directory of the hook handler.
	dir, err := filepath.Abs(t.Dir)
	if err != nil {
		return fmt.Errorf("tus: %w", err)
	}
	t.Dir = dir

	if t.HookURI != "" {
		u, err := url.Parse(t.HookURI)
		if err != nil || u.IsAbs() || !strings.HasPrefix(u.Path, "/") {
			return fmt.Errorf("tus: hook_uri must be an absolute path: %q", t.HookURI)
		}
		t.hookURL = u

		if len(t.Hooks) == 0 {
			t.Hooks = []string{hookPreCreate, hookPreFinish}
		}
		for _, h := range t.Hooks {
			if !slices.Contains(syncHooks, h) {
				return fmt.Errorf("tus: unsupported hook %q (supported: %s)", h, strings.Join(syncHooks, ", "))
			}
		}
	}

	composer := tusd.NewStoreComposer()
	filestore.New(t.Dir).UseIn(composer)
	filelocker.New(t.Dir).UseIn(composer)

	config := tusd.Config{
		StoreComposer:              composer,
		BasePath:                   t.BasePath,
		MaxSize:                    t.MaxSize,
		EnableExperimentalProtocol: t.IETFDraft,
		DisableDownload:            !t.EnableDownload,
		DisableTermination:         t.DisableTermination,
		RespectForwardedHeaders:    false,
		// tusd logs every request at info level; only keep warnings and errors.
		Logger: slog.New(slog.NewTextHandler(zap.NewStdLog(t.logger).Writer(), &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	if slices.Contains(t.Hooks, hookPreCreate) {
		config.PreUploadCreateCallback = t.preCreate
	}
	if slices.Contains(t.Hooks, hookPreFinish) {
		config.PreFinishResponseCallback = t.preFinish
	}
	if slices.Contains(t.Hooks, hookPreTerminate) {
		config.PreUploadTerminateCallback = t.preTerminate
	}

	h, err := tusd.NewHandler(config)
	if err != nil {
		return fmt.Errorf("tus: %w", err)
	}
	t.handler = h

	return nil
}

type hookTargetKey struct{}

// hookTarget carries what a hook needs to replay a sub-request through the rest of the route.
type hookTarget struct {
	req  *http.Request
	next caddyhttp.Handler
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (t *Tus) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	rest, ok := strings.CutPrefix(r.URL.Path, t.BasePath)
	if !ok {
		if r.URL.Path != strings.TrimSuffix(t.BasePath, "/") {
			return next.ServeHTTP(w, r)
		}
		rest = ""
	}

	ctx := context.WithValue(r.Context(), hookTargetKey{}, &hookTarget{req: r, next: next})
	r2 := r.WithContext(ctx)
	u := *r.URL
	u.Path = rest
	u.RawPath = ""
	r2.URL = &u

	t.handler.ServeHTTP(w, r2)

	return nil
}

func (t *Tus) preCreate(ev tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	res, err := t.invoke(hookPreCreate, ev)
	if err != nil {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, err
	}
	if res.RejectUpload {
		e := tusd.ErrUploadRejectedByServer
		e.HTTPResponse = e.HTTPResponse.MergeWith(res.HTTPResponse)

		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, e
	}

	return res.HTTPResponse, res.ChangeFileInfo, nil
}

func (t *Tus) preFinish(ev tusd.HookEvent) (tusd.HTTPResponse, error) {
	res, err := t.invoke(hookPreFinish, ev)
	if err != nil {
		return tusd.HTTPResponse{}, err
	}

	return res.HTTPResponse, nil
}

func (t *Tus) preTerminate(ev tusd.HookEvent) (tusd.HTTPResponse, error) {
	res, err := t.invoke(hookPreTerminate, ev)
	if err != nil {
		return tusd.HTTPResponse{}, err
	}
	if res.RejectTermination {
		e := tusd.ErrUploadTerminationRejected
		e.HTTPResponse = e.HTTPResponse.MergeWith(res.HTTPResponse)

		return tusd.HTTPResponse{}, e
	}

	return res.HTTPResponse, nil
}

// skippedHeaders describe the tus request body, not the hook sub-request.
var skippedHeaders = []string{"Content-Length", "Content-Type", "Content-Encoding", "Transfer-Encoding", "Expect", "Upgrade"}

// invoke runs the hook as an internal POST through the rest of the route, using tusd's HTTP hook JSON format.
// A non-2xx response rejects the operation and is forwarded to the client as is.
func (t *Tus) invoke(typ string, ev tusd.HookEvent) (hooks.HookResponse, error) {
	var res hooks.HookResponse

	target, ok := ev.Context.Value(hookTargetKey{}).(*hookTarget)
	if !ok {
		return res, errors.New("tus: hook invoked outside of a request")
	}
	orig := target.req

	server, ok := orig.Context().Value(caddyhttp.ServerCtxKey).(*caddyhttp.Server)
	if !ok {
		return res, errors.New("tus: no Caddy server in request context")
	}

	body, err := json.Marshal(hooks.HookRequest{Type: hooks.HookType(typ), Event: ev})
	if err != nil {
		return res, err
	}

	u := *t.hookURL
	req, err := http.NewRequestWithContext(ev.Context, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.URL = &u
	req.RequestURI = u.RequestURI()
	req.Host = orig.Host
	req.RemoteAddr = orig.RemoteAddr
	req.TLS = orig.TLS
	req.Proto, req.ProtoMajor, req.ProtoMinor = orig.Proto, orig.ProtoMajor, orig.ProtoMinor
	req.Header = orig.Header.Clone()
	for _, h := range skippedHeaders {
		req.Header.Del(h)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Tus-Hook", typ)

	rec := newRecorder()
	req = caddyhttp.PrepareRequest(req, caddy.NewReplacer(), rec, server)
	// Handlers that already ran (root, vars...) stored their state in the original vars.
	if vars, ok := orig.Context().Value(caddyhttp.VarsCtxKey).(map[string]any); ok {
		for k, v := range vars {
			caddyhttp.SetVar(req.Context(), k, v)
		}
	}
	caddyhttp.SetVar(req.Context(), HookVar, typ)

	if err := target.next.ServeHTTP(rec, req); err != nil {
		return res, fmt.Errorf("tus: %s hook: %w", typ, err)
	}

	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.status < 200 || rec.status > 299 {
		return res, tusd.Error{
			ErrorCode: "ERR_HOOK_REJECTED",
			Message:   typ + " hook rejected the request",
			HTTPResponse: tusd.HTTPResponse{
				StatusCode: rec.status,
				Body:       rec.body.String(),
				Header:     tusd.HTTPHeader{"Content-Type": rec.header.Get("Content-Type")},
			},
		}
	}

	if rec.body.Len() == 0 {
		return res, nil
	}
	if mt, _, _ := mime.ParseMediaType(rec.header.Get("Content-Type")); mt != "application/json" {
		return res, fmt.Errorf("tus: %s hook: expected an application/json response, got %q", typ, rec.header.Get("Content-Type"))
	}
	if rec.overflow {
		return res, fmt.Errorf("tus: %s hook: response exceeds %d bytes", typ, maxHookResponseSize)
	}
	if err := json.Unmarshal(rec.body.Bytes(), &res); err != nil {
		return res, fmt.Errorf("tus: %s hook: %w", typ, err)
	}

	return res, nil
}

// recorder buffers the hook response.
type recorder struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func newRecorder() *recorder {
	return &recorder{header: make(http.Header)}
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 && status >= 200 {
		r.status = status
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	if r.body.Len()+len(b) > maxHookResponseSize {
		r.overflow = true
		return len(b), nil
	}

	return r.body.Write(b)
}

func (r *recorder) Flush() {}

// Interface guards
var (
	_ caddy.Provisioner           = (*Tus)(nil)
	_ caddyhttp.MiddlewareHandler = (*Tus)(nil)
)
