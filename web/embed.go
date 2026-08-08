// Package webassets embebe el panel web en el binario: lo sirven tanto el
// modo -web como la app de escritorio (desktop/).
package webassets

import "embed"

//go:embed index.html desk
var FS embed.FS
