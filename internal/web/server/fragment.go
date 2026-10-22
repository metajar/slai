package server

import (
	"bytes"
	"html/template"
	"net/http"
)

// Fragment executes one named template of a page's template set (no layout) and writes it:
// the body an htmx refetch swaps in. data is the same value the page's content block uses.
func (d *Deps) Fragment(w http.ResponseWriter, r *http.Request, tpl *template.Template, name string, data any) {
	var b bytes.Buffer
	if err := tpl.ExecuteTemplate(&b, name, data); err != nil {
		d.Fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}
