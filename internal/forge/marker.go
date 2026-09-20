package forge

import (
	"regexp"
	"strings"
)

var agentMarkerRegex = regexp.MustCompile(`^[a-z0-9.-]+-(worker|reviewer|merger|reconciler):`)

// IsAgentAuthoredComment reports whether a PR comment body is agent-authored.
// A comment is agent-authored iff, after leading whitespace, it matches:
// <token>-(worker|reviewer|merger|reconciler): where <token> is one or more of [a-z0-9.-]
//
// This is an authorship/role predicate only. It reports who wrote a comment, never
// whether the feedback in a comment has been addressed. Callers deciding whether a
// comment is actionable feedback or an acknowledgment must inspect the role and the
// message (see AgentCommentRole / AgentCommentMessage), not merely this predicate.
func IsAgentAuthoredComment(body string) bool {
	trimmed := strings.TrimLeft(body, " \t\n\r")
	return agentMarkerRegex.MatchString(trimmed)
}

// AgentCommentRole returns the role encoded in a comment's agent marker prefix —
// one of "worker", "reviewer", "merger", or "reconciler" — and whether the comment
// is agent-authored at all. It is an authorship/role parser; a false ok means the
// comment is unmarked (e.g. plain human feedback).
func AgentCommentRole(body string) (string, bool) {
	trimmed := strings.TrimLeft(body, " \t\n\r")
	m := agentMarkerRegex.FindStringSubmatch(trimmed)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// AgentCommentMessage returns the comment body with its leading agent marker prefix
// removed (and surrounding whitespace trimmed). If the comment is not agent-authored,
// the trimmed original body is returned unchanged.
func AgentCommentMessage(body string) string {
	trimmed := strings.TrimLeft(body, " \t\n\r")
	loc := agentMarkerRegex.FindStringIndex(trimmed)
	if loc == nil {
		return strings.TrimSpace(body)
	}
	return strings.TrimSpace(trimmed[loc[1]:])
}

// MarkerPrefix composes a marker prefix from role and model strings.
// role should be one of "worker", "reviewer", or "merger".
func MarkerPrefix(model, role string) string {
	return model + "-" + role + ": "
}
