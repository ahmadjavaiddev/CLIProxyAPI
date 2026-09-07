// Package static bundles the fork's accounts panel HTML into the binary so
// Docker images, which do not ship the static/ directory, still serve GET /
// and /accounts.html. A same-named file on disk takes precedence when present.
package static

import (
	_ "embed"
)

// AccountsHTML is the embedded static/accounts.html panel.
//
//go:embed accounts.html
var AccountsHTML []byte
