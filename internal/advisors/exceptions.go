package advisors

import "fmt"

// Exception is a rule of the Advisor Rules page of Studio (hosted's "notification exceptions"):
// it hides lints from the advisors of the whole project or of one member. A zero selector matches
// every lint, so a rule with only LintName set hides that one lint and a rule with none hides all.
type Exception struct {
	LintCategory string
	LintName     string
	// LintMetadata, when set, matches the lints whose metadata has every one of these keys with
	// the same value.
	LintMetadata map[string]any
	// AssignedTo is the GoTrue id of the member the rule is for; empty means the whole project.
	AssignedTo string
	// IsDisabled hides the matching lints (from AssignedTo, or from everyone). A rule that is not
	// disabled and names a member shows the lints to that member only.
	IsDisabled bool
}

// Matches reports whether the rule's selector selects l.
func (e Exception) Matches(l Lint) bool {
	if e.LintName != "" && e.LintName != l.Name {
		return false
	}
	if e.LintCategory != "" && !l.Has(e.LintCategory) {
		return false
	}
	for k, want := range e.LintMetadata {
		got, ok := l.Metadata[k]
		if !ok || fmt.Sprint(got) != fmt.Sprint(want) {
			return false
		}
	}
	return true
}

// Visible filters lints by rules for the member user (a GoTrue id; empty for a caller that is
// not a member, who then sees what every member sees). The order is kept.
func Visible(lints []Lint, rules []Exception, user string) []Lint {
	if len(rules) == 0 {
		return lints
	}
	out := make([]Lint, 0, len(lints))
next:
	for _, l := range lints {
		for _, e := range rules {
			if !e.Matches(l) {
				continue
			}
			switch {
			case e.IsDisabled && (e.AssignedTo == "" || e.AssignedTo == user):
				continue next
			case !e.IsDisabled && e.AssignedTo != "" && e.AssignedTo != user:
				continue next
			}
		}
		out = append(out, l)
	}
	return out
}
