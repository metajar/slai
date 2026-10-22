package security_test

import (
	"fmt"
	"sort"
	"testing"
)

// Not a check: the route table the rest of the suite walks.
func TestRouteTableIsNotTrivial(t *testing.T) {
	s := newSeeded(t)
	var gets, writes []string
	for _, rt := range s.Srv.Routes() {
		if rt.Write() {
			writes = append(writes, rt.Method+" "+rt.Pattern)
		} else {
			gets = append(gets, rt.Pattern)
		}
	}
	sort.Strings(gets)
	sort.Strings(writes)
	t.Logf("GET routes (%d): %v", len(gets), gets)
	t.Logf("write routes (%d): %v", len(writes), writes)
	if len(gets) < 5 || len(writes) < 5 {
		t.Fatalf("expected the real modules in the table, got %d GET and %d write routes", len(gets), len(writes))
	}
	_ = fmt.Sprint
}
