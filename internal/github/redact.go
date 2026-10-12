package github

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/supavise/supavise/internal/registry"
)

// What a report may say. The check run and the comment of a pull request are read by everyone who
// can read the repository, which may be everyone. A run's log is behind the project's roles in the
// dashboard; what leaves for GitHub is a short failure with the secrets taken out, and for a branch
// that may hold data (a copy of production's, or a persistent one) not even the failure, because a
// database error can quote a value ("invalid input syntax for type integer: \"jane@example.com\"").

const (
	// maxPublicDetail bounds the failure shown on GitHub.
	maxPublicDetail = 300
	// maxPublicLog bounds the excerpt of the failed step's log in the check run.
	maxPublicLog = 3000
)

// dashboardPointer replaces a failure that must not leave the dashboard.
const dashboardPointer = "The error and the log of the run are in the dashboard (Branches > this branch > Workflow logs)."

var (
	// A password in a connection string, a bearer token, a JWT, and "password = value" in a message.
	reURLPassword = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@]*):[^\s@/]+@`)
	reBearer      = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`)
	reJWT         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)
	reAssigned    = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:pass(?:word|wd)?|secret|token|api[_-]?key|private[_-]?key)[a-z0-9_.-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)
)

// redact removes from text the values in secrets (the branch's secrets that config.toml read) and
// what looks like a credential.
func redact(text string, secrets []string) string {
	// Longest first, so that a secret that contains another is taken out whole.
	known := append([]string(nil), secrets...)
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	for _, v := range known {
		if len(v) >= 4 {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
	}
	text = reURLPassword.ReplaceAllString(text, "$1:[redacted]@")
	text = reJWT.ReplaceAllString(text, "[redacted token]")
	text = reBearer.ReplaceAllString(text, "$1 [redacted]")
	text = reAssigned.ReplaceAllString(text, "$1$2[redacted]")
	return text
}

// clip shortens s to at most n bytes, at a rune boundary, with an ellipsis.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// failedStepLog is the tail of the log of the first step of the run that failed, redacted: the lines
// a reader needs to see where the run stopped. A run that did not fail has no excerpt.
func failedStepLog(run *registry.BranchRun, secrets []string) string {
	if run == nil {
		return ""
	}
	for _, st := range run.Steps {
		if st.Status != registry.StepDead || st.Log == "" {
			continue
		}
		log := redact(strings.ReplaceAll(st.Log, "```", "'''"), secrets)
		if len(log) > maxPublicLog {
			log = "..." + log[len(log)-maxPublicLog+3:]
			// start at a whole line, and a whole rune
			if i := strings.IndexByte(log, '\n'); i >= 0 && i < 200 {
				log = "..." + log[i:]
			}
			for !utf8.ValidString(log) {
				log = log[:len(log)-1]
			}
		}
		return "```\n" + log + "```"
	}
	return ""
}

// public is the outcome as GitHub may show it, and the text of the check run. A failure of a branch
// that may hold data is replaced by a pointer to the dashboard; any other is cut, its secrets taken
// out, and the check run gets the end of the log of the step that failed and nothing of a run that
// went well.
func (o outcome) public() (outcome, string) {
	failed := !o.Success && !o.Neutral
	switch {
	case failed && o.Private:
		o.Detail = dashboardPointer
		return o, ""
	case failed:
		o.Detail = clip(redact(o.Detail, o.secrets), maxPublicDetail)
		o.Headline = redact(o.Headline, o.secrets)
		return o, failedStepLog(o.Run, o.secrets)
	}
	o.Detail = redact(o.Detail, o.secrets)
	return o, ""
}
