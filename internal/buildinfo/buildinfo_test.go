package buildinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIDStableAndNonEmpty(t *testing.T) {
	if ID() == "" || ID() != ID() {
		t.Fatalf("ID = %q", ID())
	}
	if Version() == "" || !strings.HasPrefix(ID(), Version()) {
		t.Fatalf("Version %q is not a prefix of ID %q", Version(), ID())
	}
}

func TestHashFileTracksContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bin")
	_ = os.WriteFile(p, []byte("one"), 0o755)
	a := HashFile(p)
	_ = os.WriteFile(p, []byte("two"), 0o755)
	if b := HashFile(p); a == b || len(a) != 12 {
		t.Fatalf("hashes %q %q", a, b)
	}
	if HashFile(p+".missing") != "unknown" {
		t.Fatal("missing file")
	}
}
