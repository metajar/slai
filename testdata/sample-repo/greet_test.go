package samplerepo

import "testing"

func TestGreet(t *testing.T) {
	if got := Greet("slai"); got != "Hello, slai!" {
		t.Fatalf("got %q", got)
	}
}
