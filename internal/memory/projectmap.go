package memory

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MapOptions are the inputs of ProjectMap besides the tree itself.
type MapOptions struct {
	TestCommand string   // team.yaml project.test_command
	Commands    []string // texts of fresh `command` facts
}

// NoMapMessage is what ProjectMap prints for a tree with nothing to map.
const NoMapMessage = "No project map: no packages, entry points or README found in this repository."

// skipDirs are never walked (vendored or generated trees).
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "testdata": true}

type mapPkg struct {
	dir, purpose string
	entry        []string
}

// ProjectMap renders the generated project map for the tree at root: each directory
// that holds Go source or a README with a one-line purpose (the first sentence of the
// package doc comment, else the README's first line), the entry points (package main),
// and the verify commands. It is generated at read time, never stored, and the same
// tree and options always give the same bytes (sorted walk, no timestamps).
func ProjectMap(root string, opt MapOptions) (string, error) {
	var pkgs []mapPkg
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()]) {
			return filepath.SkipDir
		}
		p, ok := scanDir(root, path)
		if ok {
			pkgs = append(pkgs, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(pkgs) == 0 {
		return NoMapMessage + "\n", nil
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].dir < pkgs[j].dir })
	var b strings.Builder
	b.WriteString("## Packages\n")
	for _, p := range pkgs {
		fmt.Fprintf(&b, "- %s — %s\n", p.dir, p.purpose)
	}
	var entries []string
	for _, p := range pkgs {
		for _, e := range p.entry {
			entries = append(entries, joinDir(p.dir, e))
		}
	}
	if len(entries) > 0 {
		b.WriteString("\n## Entry points\n")
		for _, e := range entries {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	var cmds []string
	if opt.TestCommand != "" {
		cmds = append(cmds, opt.TestCommand)
	}
	extra := append([]string(nil), opt.Commands...)
	sort.Strings(extra)
	for _, c := range extra {
		if c != opt.TestCommand {
			cmds = append(cmds, c)
		}
	}
	if len(cmds) > 0 {
		b.WriteString("\n## Verify\n")
		for _, c := range cmds {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	return b.String(), nil
}

func joinDir(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

// scanDir describes one directory: ok is false when it has neither Go source nor a README.
func scanDir(root, dir string) (mapPkg, bool) {
	rel, _ := filepath.Rel(root, dir)
	rel = filepath.ToSlash(rel)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return mapPkg{}, false
	}
	p := mapPkg{dir: rel}
	fset := token.NewFileSet()
	doc, readme := "", ""
	haveGo := false
	for _, e := range ents { // ReadDir is sorted by name
		name := e.Name()
		if e.IsDir() {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go"):
			haveGo = true
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.PackageClauseOnly|parser.ParseComments)
			if err != nil {
				continue
			}
			if f.Name.Name == "main" {
				p.entry = append(p.entry, name)
			}
			if doc == "" && f.Doc != nil {
				doc = firstSentence(f.Doc.Text())
			}
		case strings.EqualFold(name, "README.md") && readme == "":
			readme = readmeLine(filepath.Join(dir, name))
		}
	}
	if !haveGo && readme == "" {
		return mapPkg{}, false
	}
	p.purpose = doc
	if p.purpose == "" {
		p.purpose = readme
	}
	if p.purpose == "" {
		p.purpose = "(no description)"
	}
	// A main package's entry point is the file declaring it; keep only main.go when present.
	if len(p.entry) > 1 {
		for _, e := range p.entry {
			if e == "main.go" {
				p.entry = []string{"main.go"}
				break
			}
		}
	}
	return p, true
}

// firstSentence is the doc comment's first line, cut at the first ". ".
func firstSentence(doc string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(doc), "\n", 2)[0])
	if i := strings.Index(line, ". "); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSuffix(line, ".")
}

func readmeLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(sc.Text()), "#"))
		if line != "" {
			return line
		}
	}
	return ""
}
