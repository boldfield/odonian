package tuiview

import "strings"

// FilterableStates lists the states the operator can hide or show, in design order.
// done and superseded are never listed.
var FilterableStates = []string{
	"backlog", "ready", "in_progress", "checks", "review",
	"approved", "blocked", "failed", "abandoned",
}

// StateFilter is the set of hidden task states. The zero value hides nothing.
// It is a comparable value type: the set is a bitmask indexed by FilterableStates.
type StateFilter struct {
	hidden uint16
}

func stateBit(state string) (uint16, bool) {
	for i, s := range FilterableStates {
		if s == state {
			return 1 << uint(i), true
		}
	}
	return 0, false
}

// DefaultStateFilter hides failed and abandoned.
func DefaultStateFilter() StateFilter {
	f, _ := ParseHiddenStates([]string{"failed", "abandoned"})
	return f
}

// ParseHiddenStates builds a filter hiding the named states. Names that are not
// filterable states are ignored and returned. An empty list hides nothing.
func ParseHiddenStates(names []string) (StateFilter, []string) {
	var f StateFilter
	var ignored []string
	for _, n := range names {
		bit, ok := stateBit(n)
		if !ok {
			ignored = append(ignored, n)
			continue
		}
		f.hidden |= bit
	}
	return f, ignored
}

// Visible reports whether tasks in the given state are shown.
// A state that is not filterable is always visible.
func (f StateFilter) Visible(state string) bool {
	bit, ok := stateBit(state)
	return !ok || f.hidden&bit == 0
}

// Toggle returns a filter with the state's visibility flipped.
// An unfilterable state leaves the filter unchanged.
func (f StateFilter) Toggle(state string) StateFilter {
	if bit, ok := stateBit(state); ok {
		f.hidden ^= bit
	}
	return f
}

// VisibleStates returns the shown states in design order.
func (f StateFilter) VisibleStates() []string {
	return f.states(true)
}

// HiddenStates returns the hidden states in design order.
func (f StateFilter) HiddenStates() []string {
	return f.states(false)
}

func (f StateFilter) states(visible bool) []string {
	out := make([]string, 0, len(FilterableStates))
	for _, s := range FilterableStates {
		if f.Visible(s) == visible {
			out = append(out, s)
		}
	}
	return out
}

// FilterWorkItems keeps the work items whose implement task's state is visible.
// Review and merge tasks follow their implement task.
func FilterWorkItems(items []*WorkItem, filter StateFilter) []*WorkItem {
	out := make([]*WorkItem, 0, len(items))
	for _, wi := range items {
		if filter.Visible(wi.Implement.State) {
			out = append(out, wi)
		}
	}
	return out
}

// HiddenLabel returns the line naming the hidden states, or "" when none are hidden.
func HiddenLabel(filter StateFilter) string {
	hidden := filter.HiddenStates()
	if len(hidden) == 0 {
		return ""
	}
	return "hidden: " + strings.Join(hidden, ", ") + " · F to change"
}
