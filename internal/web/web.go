// Package web holds the dashboard: static files served by the Trainyard server.
// No build step and no external requests — it works on an offline server.
package web

import "embed"

//go:embed static
var Files embed.FS
