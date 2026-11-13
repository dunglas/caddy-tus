package tus

import (
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("tus", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("tus", httpcaddyfile.After, "forward_auth")
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	t := new(Tus)
	err := t.UnmarshalCaddyfile(h.Dispenser)

	return t, err
}

// UnmarshalCaddyfile sets up the handler from Caddyfile tokens:
//
//	tus {
//		base_path <path>
//		dir <path>
//		max_size <size>
//		ietf_draft
//		enable_download
//		disable_termination
//		hook_uri <path>
//		hooks <pre-create|pre-finish|pre-terminate...>
//	}
func (t *Tus) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "base_path":
			if !d.NextArg() {
				return d.ArgErr()
			}
			t.BasePath = d.Val()
		case "dir":
			if !d.NextArg() {
				return d.ArgErr()
			}
			t.Dir = d.Val()
		case "max_size":
			if !d.NextArg() {
				return d.ArgErr()
			}
			size, err := humanize.ParseBytes(d.Val())
			if err != nil {
				return d.Errf("invalid max_size: %v", err)
			}
			t.MaxSize = int64(size)
		case "ietf_draft":
			t.IETFDraft = true
		case "enable_download":
			t.EnableDownload = true
		case "disable_termination":
			t.DisableTermination = true
		case "hook_uri":
			if !d.NextArg() {
				return d.ArgErr()
			}
			t.HookURI = d.Val()
		case "hooks":
			t.Hooks = d.RemainingArgs()
			if len(t.Hooks) == 0 {
				return d.ArgErr()
			}
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
	}

	return nil
}

var _ caddyfile.Unmarshaler = (*Tus)(nil)
