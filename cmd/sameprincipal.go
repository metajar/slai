package cmd

import (
	"fmt"
	"io"

	"github.com/oorbt/slai/internal/config"
)

// sameReviewerLine is the informational line `slai init` and `slai doctor` print when
// the principal runs the same model and harness as every engineer ("" otherwise). It is
// information, not a warning: nothing is wrong, but a reviewer of the same model shares
// the author's blind spots, which is worth knowing when the team is chosen.
func sameReviewerLine(t *config.Team) string {
	model, harness, ok := t.PrincipalSharesEngineers()
	if !ok {
		return ""
	}
	return fmt.Sprintf("- info: the principal runs the same model and harness as every engineer (%s on %s), so reviews are the same model checking its own kind of work. A different model or harness for the principal gives a more independent review (`slai init --edit`).", model, harness)
}

func printSameReviewerLine(w io.Writer, t *config.Team) {
	if l := sameReviewerLine(t); l != "" {
		fmt.Fprintln(w, l)
	}
}
