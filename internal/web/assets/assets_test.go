package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// vendorRows reads the `| file | source | version | sha256 |` rows of VENDOR.md.
func vendorRows(t *testing.T) map[string]string {
	t.Helper()
	b, err := FS.ReadFile("VENDOR.md")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	re := regexp.MustCompile(`^\|\s*([\w.\-]+)\s*\|.*\|\s*([0-9a-f]{64})\s*\|$`)
	for _, l := range strings.Split(string(b), "\n") {
		if m := re.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

// P1 AC5 / security AC9: VENDOR.md pins the sha256 of every vendored file.
func TestVendorHashesMatch(t *testing.T) {
	rows := vendorRows(t)
	if len(rows) != 4 {
		t.Fatalf("VENDOR.md should pin tabler, htmx, sortable and slai.js, got %v", rows)
	}
	for name, want := range rows {
		b, err := FS.ReadFile(name)
		if err != nil {
			t.Fatalf("%s is pinned but not embedded: %v", name, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s drifted: sha256 %s, VENDOR.md says %s", name, got, want)
		}
	}
}

// Security AC9 (amended: the principal allowed one first-party script on conditions): exactly
// htmx.min.js, sortable.min.js and slai.js are embedded, and all three are pinned. A fourth fails.
func TestExactlyThreeJavaScriptFiles(t *testing.T) {
	var js []string
	_ = fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".js") {
			js = append(js, p)
		}
		return nil
	})
	sort.Strings(js)
	if got := strings.Join(js, " "); got != "htmx.min.js slai.js sortable.min.js" {
		t.Fatalf("want exactly htmx.min.js, slai.js and sortable.min.js embedded, got %v", js)
	}
	rows := vendorRows(t)
	for _, p := range js {
		if rows[p] == "" {
			t.Errorf("%s is embedded but not pinned in VENDOR.md", p)
		}
	}
}

// P1 AC: under 3 MB in total, and no file reaches out to a CDN.
func TestAssetsAreSmallAndLocal(t *testing.T) {
	total := 0
	_ = fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := FS.ReadFile(p)
		total += len(b)
		if strings.HasSuffix(p, ".css") && regexp.MustCompile(`@import|url\(\s*["']?https?:`).Match(b) {
			t.Errorf("%s loads something from the network", p)
		}
		return nil
	})
	if total > 3<<20 {
		t.Fatalf("embedded assets are %d bytes, over the 3 MB budget", total)
	}
}
