// Package assets embeds the files `slai serve` serves under /static/: Tabler's CSS, htmx
// and SortableJS (vendored, pinned in VENDOR.md) and slai.css. Nothing is fetched from the
// network, and there are exactly three JavaScript files: the two vendored ones and slai.js
// (ours: it only wires SortableJS drops to htmx requests; see slai_js_test.go).
package assets

import "embed"

//go:embed tabler.min.css slai.css htmx.min.js sortable.min.js slai.js VENDOR.md
var FS embed.FS
