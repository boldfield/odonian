package forge

import (
	"testing"
)

func TestIsAgentAuthoredComment(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		// Positives
		{
			name: "haiku-worker",
			body: "haiku-worker: Some feedback",
			want: true,
		},
		{
			name: "gpt-5.5-reviewer",
			body: "gpt-5.5-reviewer: This looks good",
			want: true,
		},
		{
			name: "fable-merger",
			body: "fable-merger: Merging now",
			want: true,
		},
		{
			name: "odonian-reconciler",
			body: "odonian-reconciler: changes requested — bouncing back",
			want: true,
		},
		{
			name: "leading space",
			body: " haiku-worker: Comment",
			want: true,
		},
		{
			name: "leading tab",
			body: "\thaiku-worker: Comment",
			want: true,
		},
		{
			name: "leading newline",
			body: "\nhaiku-worker: Comment",
			want: true,
		},
		{
			name: "multiple leading whitespace",
			body: "  \n\t haiku-worker: Comment",
			want: true,
		},
		// Negatives
		{
			name: "plain human text",
			body: "This is a human comment",
			want: false,
		},
		{
			name: "marker mid-body",
			body: "Some text haiku-worker: More text",
			want: false,
		},
		{
			name: "uppercase in token",
			body: "Haiku-worker: Comment",
			want: false,
		},
		{
			name: "missing colon",
			body: "haiku-worker Comment",
			want: false,
		},
		{
			name: "wrong role",
			body: "haiku-maintainer: Comment",
			want: false,
		},
		{
			name: "empty body",
			body: "",
			want: false,
		},
		{
			name: "only whitespace",
			body: "   \n\t  ",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAgentAuthoredComment(tt.body)
			if got != tt.want {
				t.Errorf("IsAgentAuthoredComment(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestAgentCommentRole(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantRole string
		wantOK   bool
	}{
		{name: "worker", body: "haiku-worker: pushed rework", wantRole: "worker", wantOK: true},
		{name: "reviewer", body: "gpt-5.5-reviewer: CHANGES REQUESTED", wantRole: "reviewer", wantOK: true},
		{name: "merger", body: "fable-merger: merged", wantRole: "merger", wantOK: true},
		{name: "reconciler", body: "odonian-reconciler: bounced", wantRole: "reconciler", wantOK: true},
		{name: "leading whitespace", body: "  \n\thaiku-worker: hi", wantRole: "worker", wantOK: true},
		{name: "unmarked human", body: "Please fix the off-by-one", wantRole: "", wantOK: false},
		{name: "marker mid-body", body: "note: haiku-worker: hi", wantRole: "", wantOK: false},
		{name: "empty", body: "", wantRole: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, ok := AgentCommentRole(tt.body)
			if role != tt.wantRole || ok != tt.wantOK {
				t.Errorf("AgentCommentRole(%q) = (%q, %v), want (%q, %v)", tt.body, role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}

func TestAgentCommentMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "reviewer changes requested", body: "gpt-5.5-reviewer: CHANGES REQUESTED - fix it", want: "CHANGES REQUESTED - fix it"},
		{name: "reviewer approved", body: "opus-reviewer: APPROVED", want: "APPROVED"},
		{name: "leading whitespace", body: "  haiku-worker:   addressed in abc123", want: "addressed in abc123"},
		{name: "unmarked returned as-is", body: "  Please fix this  ", want: "Please fix this"},
		{name: "empty", body: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AgentCommentMessage(tt.body); got != tt.want {
				t.Errorf("AgentCommentMessage(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestMarkerPrefix(t *testing.T) {
	tests := []struct {
		name  string
		model string
		role  string
		want  string
	}{
		{
			name:  "haiku worker",
			model: "haiku",
			role:  "worker",
			want:  "haiku-worker: ",
		},
		{
			name:  "gpt-5.5 reviewer",
			model: "gpt-5.5",
			role:  "reviewer",
			want:  "gpt-5.5-reviewer: ",
		},
		{
			name:  "fable merger",
			model: "fable",
			role:  "merger",
			want:  "fable-merger: ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkerPrefix(tt.model, tt.role)
			if got != tt.want {
				t.Errorf("MarkerPrefix(%q, %q) = %q, want %q", tt.model, tt.role, got, tt.want)
			}
		})
	}
}
