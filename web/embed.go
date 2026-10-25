// Package web holds the static UI assets that are embedded into the gabkeep
// binary (spec §3: no build step beyond `go build`, no runtime asset fetch).
package web

import "embed"

// FS contains index.html plus everything under assets/.
//
//go:embed index.html assets
var FS embed.FS
