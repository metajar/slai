# Vendored web assets

Everything `slai serve` ships is embedded with `go:embed`; nothing is loaded from a CDN.
`TestVendorHashesMatch` fails when a file here drifts from the hash below, and
`TestExactlyFourJavaScriptFiles` fails if a fifth script is embedded. To upgrade a file,
download the new version, update the row, and say why in the commit.

| file | source | version | sha256 |
| --- | --- | --- | --- |
| tabler.min.css | https://cdn.jsdelivr.net/npm/@tabler/core@1.0.0/dist/css/tabler.min.css | @tabler/core 1.0.0 (MIT) | 68efb8668c8d3d91c27b16dba6b0d853d2c2c6c86ab33400d1220d1587e8b373 |
| htmx.min.js | https://cdn.jsdelivr.net/npm/htmx.org@2.0.4/dist/htmx.min.js | htmx 2.0.4 (BSD 0-clause) | e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447 |
| sortable.min.js | https://cdn.jsdelivr.net/npm/sortablejs@1.15.6/Sortable.min.js | SortableJS 1.15.6 (MIT) | 6d0a831fc19b4bae851797ad3393157e861afb7862459c11226359b27e2c4337 |
| slai.js | ours (first-party) | slai | 2eab8ea6944be5cae0deb375060ceda06237990ad5eba005e41b77ad43b9863c |
| theme.js | ours (first-party) | slai | 1220d0118b9eca8a1369929e9583c527ea1393481c4aa4bd53a74f3381151327 |

`slai.js` is ours, not vendored, but pinned like the others (the principal's condition for allowing a
third script): any change to it shows up as a hash change in review. It is under 150 lines, opens one same-origin stream (`/events`, notices only) for the live regions, builds no
HTML, evaluates nothing and handles no token; `scripts_lint_test.go` lints that for both slai.js and theme.js. `theme.js` (under 20 lines) only sets `data-bs-theme` from `prefers-color-scheme` before first paint; it is loaded synchronously in `<head>` because the CSP forbids inline scripts. `slai.css` is ours and not pinned.

Tabler's icon font and its own JavaScript are deliberately not included: icons are inline
SVG and the only scripts are htmx and SortableJS.
