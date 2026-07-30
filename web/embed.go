// Package web holds the dashboard's static files.
//
// A package of its own because go:embed cannot reach outside the directory of
// the file that declares it, and these belong at the top of the repository where
// anyone looking for the user interface will find them.
package web

import "embed"

// Files are the dashboard's assets. No build step produces them: what is in the
// repository is what is served.
//
//go:embed index.html app.js style.css
var Files embed.FS
