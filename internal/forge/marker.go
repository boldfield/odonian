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
// This is an authorship/role predicate only. It says nothing about whether the comment is
// actionable feedback or has been addressed — callers must apply their own classification
// (see CommentRole) rather than treating agent authorship as a completion signal.
func IsAgentAuthoredComment(body string) bool {
	trimmed := strings.TrimLeft(body, " \t\n\r")
	return agentMarkerRegex.MatchString(trimmed)
}

// CommentRole returns the marker role ("worker", "reviewer", "merger", or "reconciler") for a
// marker-prefixed comment body, and whether a marker was found at all. Like
// IsAgentAuthoredComment, this is an authorship/role parser only, not a completion rule.
func CommentRole(body string) (role string, ok bool) {
	trimmed := strings.TrimLeft(body, " \t\n\r")
	m := agentMarkerRegex.FindStringSubmatch(trimmed)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// StripCommentMarker returns the text of a marker-prefixed comment body after the marker
// (and any whitespace immediately following it), and whether the body was marker-prefixed.
func StripCommentMarker(body string) (rest string, ok bool) {
	trimmed := strings.TrimLeft(body, " \t\n\r")
	loc := agentMarkerRegex.FindStringIndex(trimmed)
	if loc == nil {
		return "", false
	}
	return strings.TrimLeft(trimmed[loc[1]:], " \t\n\r"), true
}

// MarkerPrefix composes a marker prefix from role and model strings.
// role should be one of "worker", "reviewer", or "merger".
func MarkerPrefix(model, role string) string {
	return model + "-" + role + ": "
}
