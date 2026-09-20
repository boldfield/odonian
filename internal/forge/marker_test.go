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

func TestCommentRole(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantRole string
		wantOK   bool
	}{
		{name: "worker", body: "haiku-worker: addressed in abc123", wantRole: "worker", wantOK: true},
		{name: "reviewer", body: "gpt-5.5-reviewer: CHANGES REQUESTED", wantRole: "reviewer", wantOK: true},
		{name: "merger", body: "fable-merger: Merging now", wantRole: "merger", wantOK: true},
		{name: "reconciler", body: "odonian-reconciler: bouncing back", wantRole: "reconciler", wantOK: true},
		{name: "leading whitespace", body: "  \n haiku-worker: Comment", wantRole: "worker", wantOK: true},
		{name: "unmarked human comment", body: "Please fix this", wantRole: "", wantOK: false},
		{name: "marker mid-body is not a prefix", body: "Some text haiku-worker: More", wantRole: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, ok := CommentRole(tt.body)
			if role != tt.wantRole || ok != tt.wantOK {
				t.Errorf("CommentRole(%q) = (%q, %v), want (%q, %v)", tt.body, role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}

func TestStripCommentMarker(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantRest string
		wantOK   bool
	}{
		{name: "worker", body: "haiku-worker: addressed in abc123", wantRest: "addressed in abc123", wantOK: true},
		{name: "reviewer approval", body: "gpt-5.5-reviewer:   APPROVED", wantRest: "APPROVED", wantOK: true},
		{name: "unmarked comment", body: "Please fix this", wantRest: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, ok := StripCommentMarker(tt.body)
			if rest != tt.wantRest || ok != tt.wantOK {
				t.Errorf("StripCommentMarker(%q) = (%q, %v), want (%q, %v)", tt.body, rest, ok, tt.wantRest, tt.wantOK)
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
