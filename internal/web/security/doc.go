// Package security holds the cross-cutting security tests of `slai serve` (spec W8). They run
// over the real route table (every page module in pages.All) rather than over one page, so a
// route that merges later is covered without editing a test. This package has no code of
// its own.
package security
