// Command frankenphp-tus is FrankenPHP with the tus handler.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/dunglas/caddy-tus"
	_ "github.com/dunglas/frankenphp/caddy"
)

func main() {
	caddycmd.Main()
}
