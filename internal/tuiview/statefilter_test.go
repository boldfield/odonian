package tuiview

import (
	"reflect"
	"testing"

	"github.com/boldfield/odonian/internal/tuiclient"
)

func TestDefaultStateFilter(t *testing.T) {
	f := DefaultStateFilter()
	if got, want := f.HiddenStates(), []string{"failed", "abandoned"}; !reflect.DeepEqual(got, want) {
		t.Errorf("hidden = %v, want %v", got, want)
	}
	if got, want := f.VisibleStates(), []string{"backlog", "ready", "in_progress", "checks", "review", "approved", "blocked"}; !reflect.DeepEqual(got, want) {
		t.Errorf("visible = %v, want %v", got, want)
	}
}

func TestParseHiddenStates(t *testing.T) {
	tests := []struct {
		name        string
		in          []string
		wantHidden  []string
		wantIgnored []string
	}{
		{"valid", []string{"blocked", "backlog"}, []string{"backlog", "blocked"}, nil},
		{"empty", []string{}, []string{}, nil},
		{"nil", nil, []string{}, nil},
		{"unknown and done", []string{"nope", "failed", "done"}, []string{"failed"}, []string{"nope", "done"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ignored := ParseHiddenStates(tt.in)
			if got := f.HiddenStates(); !reflect.DeepEqual(got, tt.wantHidden) {
				t.Errorf("hidden = %v, want %v", got, tt.wantHidden)
			}
			if !reflect.DeepEqual(ignored, tt.wantIgnored) {
				t.Errorf("ignored = %v, want %v", ignored, tt.wantIgnored)
			}
		})
	}
}

func TestToggle(t *testing.T) {
	for _, s := range FilterableStates {
		t.Run(s, func(t *testing.T) {
			f := StateFilter{}
			hidden := f.Toggle(s)
			if hidden.Visible(s) {
				t.Errorf("%s still visible after toggle", s)
			}
			if f.Visible(s) != true {
				t.Errorf("toggle mutated receiver")
			}
			if hidden.Toggle(s) != f {
				t.Errorf("second toggle did not restore filter")
			}
		})
	}
	f := DefaultStateFilter()
	for _, s := range []string{"done", "superseded", "bogus"} {
		if f.Toggle(s) != f {
			t.Errorf("toggling %q changed the filter", s)
		}
		if !f.Visible(s) {
			t.Errorf("%q should be visible", s)
		}
	}
}

func TestFilterWorkItems(t *testing.T) {
	failedWithReview := &WorkItem{
		Implement: &tuiclient.Task{ID: "a", State: "failed"},
		Reviews:   []*tuiclient.Task{{ID: "ar", State: "ready"}},
	}
	inReviewHiddenChild := &WorkItem{
		Implement: &tuiclient.Task{ID: "b", State: "review"},
		Reviews:   []*tuiclient.Task{{ID: "br", State: "failed"}},
	}
	got := FilterWorkItems([]*WorkItem{failedWithReview, inReviewHiddenChild}, DefaultStateFilter())
	if len(got) != 1 || got[0] != inReviewHiddenChild || len(got[0].Reviews) != 1 {
		t.Errorf("got %v, want only the in-review item kept whole", got)
	}
}

func TestHiddenLabel(t *testing.T) {
	if got, want := HiddenLabel(DefaultStateFilter()), "hidden: failed, abandoned · F to change"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := HiddenLabel(StateFilter{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestNeedsYouFailedAndBadge(t *testing.T) {
	if NeedsYou(&tuiclient.Task{State: "failed"}) {
		t.Error("failed should not need you")
	}
	if !NeedsYou(&tuiclient.Task{State: "blocked"}) {
		t.Error("blocked should need you")
	}
	if !NeedsYou(&tuiclient.Task{State: "approved", AgentMerge: false}) {
		t.Error("approved without agent_merge should need you")
	}
	g, n := Badge([]*WorkItem{{Implement: &tuiclient.Task{State: "failed"}}})
	if g != "" || n != 0 {
		t.Errorf("badge = %q,%d, want none", g, n)
	}
}
