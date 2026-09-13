// Package web embeds the two operator pages -- the public log viewer and the
// metrics dashboard -- so the deployed image is a single self-contained binary
// with no static-asset hosting to arrange.
package web

import _ "embed"

//go:embed logs.html
var LogsHTML []byte

//go:embed dashboard.html
var DashboardHTML []byte
