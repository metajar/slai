package service

import (
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/store"
)

// Per-acceptance-criterion handoff (improvements backlog #6): the engineer lists
// the spec's ACs with request_review, the reviewer answers each with approve or
// request_changes, so a disagreement about what a spec meant shows up per AC, not
// as a free-text argument.

const (
	maxACs    = 100
	maxACName = 80
)

// acClaims validates and converts the claims of a request_review.
func acClaims(in []ACClaimArg) ([]store.ACClaim, error) {
	if len(in) > maxACs {
		return nil, fmt.Errorf("ac_claims has %d entries; at most %d", len(in), maxACs)
	}
	seen := map[string]bool{}
	var out []store.ACClaim
	for _, c := range in {
		ac := strings.TrimSpace(c.AC)
		if ac == "" || len(ac) > maxACName {
			return nil, fmt.Errorf("ac_claims: every entry needs an \"ac\" label of 1-%d characters", maxACName)
		}
		if k := store.ACKey(ac); seen[k] {
			return nil, fmt.Errorf("ac_claims: %q appears twice", ac)
		} else {
			seen[k] = true
		}
		if err := checkText("ac_claims note", c.Note); err != nil {
			return nil, err
		}
		out = append(out, store.ACClaim{AC: ac, Met: c.Met, Note: c.Note})
	}
	return out, nil
}

// claimSummary is the one line a reviewer sees in the review notification.
func claimSummary(cs []store.ACClaim) string {
	var met, notMet []string
	for _, c := range cs {
		if c.Met {
			met = append(met, c.AC)
		} else {
			notMet = append(notMet, c.AC)
		}
	}
	s := fmt.Sprintf("AC claims: %d met", len(met))
	if len(notMet) > 0 {
		s += fmt.Sprintf(", %d NOT met (%s)", len(notMet), strings.Join(notMet, ", "))
	}
	return s + ". review_diff lists them; answer each with ac_verdicts."
}

// verdictOutcome is what recording a review's verdicts produced.
type verdictOutcome struct {
	verdicts   []store.ACVerdict
	unanswered []string
	warnings   []string
}

func (v verdictOutcome) addTo(out map[string]any) {
	if len(v.verdicts) > 0 {
		out["ac_verdicts"] = v.verdicts
	}
	if len(v.unanswered) > 0 {
		out["unanswered_claims"] = v.unanswered
	}
	if len(v.warnings) > 0 {
		out["warnings"] = v.warnings
	}
}

// recordVerdicts stores the reviewer's verdicts against the task's latest review
// request and reports the engineer's claims that still have no verdict. Missing
// verdicts are a warning in the result, never a refusal.
func (s *Service) recordVerdicts(t store.Task, caller string, in []ACVerdictArg) (verdictOutcome, error) {
	var out verdictOutcome
	if len(in) > maxACs {
		return out, fmt.Errorf("ac_verdicts has %d entries; at most %d", len(in), maxACs)
	}
	seen := map[string]bool{}
	var vs []store.ACVerdict
	for _, v := range in {
		ac := strings.TrimSpace(v.AC)
		if ac == "" || len(ac) > maxACName {
			return out, fmt.Errorf("ac_verdicts: every entry needs an \"ac\" label of 1-%d characters", maxACName)
		}
		if k := store.ACKey(ac); seen[k] {
			return out, fmt.Errorf("ac_verdicts: %q appears twice", ac)
		} else {
			seen[k] = true
		}
		if err := checkText("ac_verdicts note", v.Note); err != nil {
			return out, err
		}
		vs = append(vs, store.ACVerdict{AC: ac, Pass: v.Pass, Note: v.Note})
	}
	if len(vs) > 0 {
		if err := s.Store.SaveACVerdicts(t.ID, caller, vs); err != nil {
			return out, err
		}
	}
	all, err := s.Store.LatestACVerdicts(t.ID)
	if err != nil {
		return out, err
	}
	out.verdicts = all
	if out.unanswered, err = s.Store.UnansweredClaims(t.ID); err != nil {
		return out, err
	}
	if len(out.unanswered) > 0 {
		out.warnings = append(out.warnings, "the engineer's claims with no verdict from you: "+strings.Join(out.unanswered, ", ")+" (pass ac_verdicts for each; this is a warning, not a refusal)")
	}
	return out, nil
}

func failedACs(vs []store.ACVerdict) []string {
	var out []string
	for _, v := range vs {
		if !v.Pass {
			out = append(out, v.AC)
		}
	}
	return out
}

// verdictSummary is "AC verdicts: AC 1 pass; AC 2 FAIL (no test)" for the engineer's note.
func verdictSummary(vs []store.ACVerdict) string {
	if len(vs) == 0 {
		return ""
	}
	var parts []string
	for _, v := range vs {
		p := v.AC + " pass"
		if !v.Pass {
			p = v.AC + " FAIL"
			if v.Note != "" {
				p += " (" + v.Note + ")"
			}
		}
		parts = append(parts, p)
	}
	return "AC verdicts: " + strings.Join(parts, "; ")
}
