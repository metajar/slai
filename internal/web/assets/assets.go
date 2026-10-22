// Package assets embeds the files `slai serve` serves under /static/: Tabler's CSS, htmx
// and SortableJS (vendored, pinned in VENDOR.md) and slai.css. Nothing is fetched from the
// network, and there are exactly four JavaScript files: the two vendored ones, slai.js
// (ours: it only wires SortableJS drops to htmx requests; see scripts_lint_test.go) and theme.js
// (ours: sets data-bs-theme from prefers-color-scheme before first paint).
package assets

import "embed"

//go:embed tabler.min.css slai.css htmx.min.js sortable.min.js slai.js theme.js VENDOR.md
var FS embed.FS
