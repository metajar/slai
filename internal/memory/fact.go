// Package memory is the team's shared, grounded fact cache.
//
// Facts are Markdown files, one per fact, under <project>/.slai/memory/<id>.md; they are
// the source of truth and are tracked in git. SQLite (table facts_index) is a derived
// index that Reindex rebuilds from the files; on any disagreement the file wins. Every
// fact cites repository paths with the blob id each had on slai/integration when the
// fact was recorded, and is "stale" as soon as one of those blobs changed or vanished.
//
// API in short:
//
//	m := memory.New(dir, db, repo, rev)  // rev defaults to "slai/integration"
//	f, existed, err := m.Put(ctx, Fact{Kind, Text, Author, Paths: []Path{{Path: "a.go"}}})
//	f, err := m.Get(id); facts, invalid, err := m.List()
//	res, err := m.Check(ctx, f)          // one git ls-tree call
//	err := m.Reindex(ctx)                // rebuild facts_index from the files
//	err := m.Delete(id)
//
// Put and Delete are the only functions that write or remove fact files, and they hold
// one lock, so concurrent writers of different facts never collide.
package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kinds a fact can have.
const (
	KindArchitecture    = "architecture"
	KindConvention      = "convention"
	KindGotcha          = "gotcha"
	KindCommand         = "command"
	KindDecisionPointer = "decision-pointer"
)

// Stored statuses. "stale" is never stored: it is computed against slai/integration.
const (
	StatusProposed = "proposed"
	StatusVerified = "verified"
)

// MaxTextLen is the longest allowed fact text, in characters.
const MaxTextLen = 300

var kinds = map[string]bool{KindArchitecture: true, KindConvention: true, KindGotcha: true, KindCommand: true, KindDecisionPointer: true}

// Path is a repository path a fact cites and its blob id on integration when recorded.
type Path struct {
	Path string `yaml:"path" json:"path"`
	Blob string `yaml:"blob" json:"blob"`
}

// Fact is one parsed memory file.
type Fact struct {
	ID             string `yaml:"id"`
	Kind           string `yaml:"kind"`
	Status         string `yaml:"status"`
	Author         string `yaml:"author"`
	VerifiedBy     string `yaml:"verified_by,omitempty"`
	IntegrationSHA string `yaml:"integration_sha"`
	Created        string `yaml:"created"` // YYYY-MM-DD
	Paths          []Path `yaml:"paths"`
	Decision       string `yaml:"decision"` // heading in decisions.md (kind decision-pointer)
	Check          string `yaml:"check"`    // optional re-proving command; only a human runs it
	Text           string `yaml:"-"`        // the file body
}

// Errors returned for facts that cannot be stored.
var (
	ErrBadKind = errors.New("memory: unknown kind")
	ErrBadText = errors.New("memory: text must be 1-300 characters on one line without backticks")
)

// Normalize is the text form ids are derived from: lower case, whitespace collapsed.
func Normalize(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// ID derives the deterministic id from kind, normalised text and the sorted cited paths.
func ID(kind, text string, paths []Path) string {
	ps := make([]string, 0, len(paths))
	for _, p := range paths {
		ps = append(ps, p.Path)
	}
	sort.Strings(ps)
	sum := sha256.Sum256([]byte(kind + "\n" + Normalize(text) + "\n" + strings.Join(ps, "\n")))
	return "f-" + hex.EncodeToString(sum[:])[:4*2]
}

// Validate checks the fields Put needs; callers (the service) sanitise before this.
func (f Fact) Validate() error {
	if !kinds[f.Kind] {
		return fmt.Errorf("%w %q", ErrBadKind, f.Kind)
	}
	if t := strings.TrimSpace(f.Text); t == "" || len([]rune(t)) > MaxTextLen || strings.ContainsAny(t, "\n\r`") {
		return ErrBadText
	}
	return nil
}

func scalar(s string) string {
	b, _ := yaml.Marshal(s)
	return strings.TrimRight(string(b), "\n")
}

// Marshal renders the file: YAML frontmatter, then the text.
func (f Fact) Marshal() []byte {
	var b bytes.Buffer
	w := func(k, v string) { fmt.Fprintf(&b, "%s: %s\n", k, scalar(v)) }
	b.WriteString("---\n")
	w("id", f.ID)
	w("kind", f.Kind)
	w("status", f.Status)
	w("author", f.Author)
	if f.VerifiedBy != "" {
		w("verified_by", f.VerifiedBy)
	}
	w("integration_sha", f.IntegrationSHA)
	w("created", f.Created)
	if len(f.Paths) == 0 {
		b.WriteString("paths: []\n")
	} else {
		b.WriteString("paths:\n")
		for _, p := range f.Paths {
			fmt.Fprintf(&b, "  - path: %s\n    blob: %s\n", scalar(p.Path), scalar(p.Blob))
		}
	}
	w("decision", f.Decision)
	w("check", f.Check)
	b.WriteString("---\n")
	b.WriteString(strings.TrimSpace(f.Text))
	b.WriteString("\n")
	return b.Bytes()
}

// Parse reads a memory file. It rejects missing or malformed frontmatter and an unknown kind or
// status. The id is not re-derived: a hand edit may change the text and keeps the file's id.
func Parse(data []byte) (Fact, error) {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return Fact{}, errors.New("memory: no frontmatter")
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		if head, ok = strings.CutSuffix(rest, "\n---"); !ok {
			return Fact{}, errors.New("memory: unterminated frontmatter")
		}
	}
	var f Fact
	dec := yaml.NewDecoder(strings.NewReader(head))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return Fact{}, fmt.Errorf("memory: frontmatter: %w", err)
	}
	f.Text = strings.TrimSpace(body)
	if err := f.Validate(); err != nil {
		return Fact{}, err
	}
	if f.Status != StatusProposed && f.Status != StatusVerified {
		return Fact{}, fmt.Errorf("memory: unknown status %q", f.Status)
	}
	return f, nil
}
