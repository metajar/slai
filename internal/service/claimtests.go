package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/oorbt/slai/internal/store"
)

// A claim note that cites a test ("TestFoo covers it") is only worth something if that
// test exists. At request_review and review_diff the branch tree is searched for each
// cited name; names that are not there come back as a warning (never a refusal: the
// reviewer decides). Review-loop audit D.

// citedTest matches Go-style test names: "Test" followed by a non-lowercase character,
// so ordinary words like "Tests" or "Testing" are not mistaken for test names.
var citedTest = regexp.MustCompile(`\bTest[A-Z0-9_][A-Za-z0-9_]*`)

// missingTestWarning returns one warning line naming the cited tests absent from rev,
// or nil.
func (s *Service) missingTestWarning(ctx context.Context, rev string, claims []store.ACClaim) []string {
	if rev == "" {
		return nil
	}
	seen := map[string]bool{}
	var missing []string
	for _, c := range claims {
		for _, name := range citedTest.FindAllString(c.Note, -1) {
			if seen[name] {
				continue
			}
			seen[name] = true
			if found, err := s.Git.TreeMentions(ctx, rev, name); err == nil && !found {
				missing = append(missing, fmt.Sprintf("%s (cited for %s)", name, c.AC))
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []string{"claim notes name tests that do not exist in the branch: " + strings.Join(missing, ", ") + ". A claim should cite a test that is in the diff; this is a warning, not a refusal"}
}
