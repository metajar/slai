package cmd

import (
	"strings"
	"testing"
)

func TestVersionStringShowsBuildIdentity(t *testing.T) {
	got := versionString()
	for _, want := range []string{"slai ", "\nbuild ", "\nrevision ", "\nbuilt ", "\nlinux/"} {
		if !strings.Contains(got, want) {
			t.Errorf("version output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "commit none") {
		t.Errorf("old placeholder text: %s", got)
	}
}
