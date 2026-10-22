package store

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestListMigrationsRejectsDuplicateVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0005_a.sql":      {Data: []byte("")},
		"migrations/0006_usage.sql":  {Data: []byte("")},
		"migrations/0006_mem.sql":    {Data: []byte("")},
		"migrations/down/0006_x.sql": {Data: []byte("")},
	}
	_, err := listMigrations(fsys)
	if err == nil || !strings.Contains(err.Error(), "0006_usage.sql") || !strings.Contains(err.Error(), "0006_mem.sql") {
		t.Fatalf("err = %v, want one naming both 0006 files", err)
	}
}

func TestListMigrationsOrdersValidSet(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0010_c.sql":      {Data: []byte("")},
		"migrations/0002_b.sql":      {Data: []byte("")},
		"migrations/down/0002_b.sql": {Data: []byte("")},
	}
	fs, err := listMigrations(fsys)
	if err != nil || len(fs) != 2 || fs[0].ver != 2 || fs[1].ver != 10 {
		t.Fatalf("%+v %v", fs, err)
	}
}

// The real embedded migrations: a duplicate fails `go test` before it can merge.
func TestEmbeddedMigrationVersionsAreUnique(t *testing.T) {
	if _, err := listMigrations(migrationsFS); err != nil {
		t.Fatal(err)
	}
}
