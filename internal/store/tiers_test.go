package store

import "testing"

func TestNextTier(t *testing.T) {
	store := &sqliteStore{
		allowedModels:    []string{"haiku", "sonnet", "opus"},
		escalationLadder: []string{"haiku", "sonnet", "opus"},
	}

	tests := []struct {
		model    string
		wantTier string
		wantOk   bool
	}{
		{"haiku", "sonnet", true},
		{"sonnet", "opus", true},
		{"opus", "", false},
		{"unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gotTier, gotOk := store.nextTier(tt.model)
			if gotTier != tt.wantTier || gotOk != tt.wantOk {
				t.Errorf("nextTier(%q) = (%q, %v), want (%q, %v)", tt.model, gotTier, gotOk, tt.wantTier, tt.wantOk)
			}
		})
	}
}

func TestIsTopTier(t *testing.T) {
	store := &sqliteStore{
		allowedModels:    []string{"haiku", "sonnet", "opus"},
		escalationLadder: []string{"haiku", "sonnet", "opus"},
	}

	tests := []struct {
		model string
		want  bool
	}{
		{"haiku", false},
		{"sonnet", false},
		{"opus", true},
		{"unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := store.isTopTier(tt.model)
			if got != tt.want {
				t.Errorf("isTopTier(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestNextTierWithCustomLadder(t *testing.T) {
	// Test that nextTier walks a custom escalation ladder
	store := &sqliteStore{
		allowedModels:    []string{"haiku", "sonnet", "opus", "fable"},
		escalationLadder: []string{"haiku", "opus", "fable"},
	}

	tests := []struct {
		model    string
		wantTier string
		wantOk   bool
	}{
		{"haiku", "opus", true},
		{"opus", "fable", true},
		{"fable", "", false},
		{"sonnet", "", false}, // sonnet is in allowedModels but not in ladder
		{"unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gotTier, gotOk := store.nextTier(tt.model)
			if gotTier != tt.wantTier || gotOk != tt.wantOk {
				t.Errorf("nextTier(%q) = (%q, %v), want (%q, %v)", tt.model, gotTier, gotOk, tt.wantTier, tt.wantOk)
			}
		})
	}
}

func TestIsTopTierWithCustomLadder(t *testing.T) {
	// Test that isTopTier respects custom escalation ladder
	store := &sqliteStore{
		allowedModels:    []string{"haiku", "sonnet", "opus", "fable"},
		escalationLadder: []string{"haiku", "opus", "fable"},
	}

	tests := []struct {
		model string
		want  bool
	}{
		{"haiku", false},
		{"opus", false},
		{"fable", true},
		{"sonnet", false}, // sonnet is in allowedModels but not in ladder
		{"unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := store.isTopTier(tt.model)
			if got != tt.want {
				t.Errorf("isTopTier(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestDefaultLadderToAllowedModels(t *testing.T) {
	// Test that ladder defaults to allowedModels when not explicitly set
	store := &sqliteStore{
		allowedModels:    []string{"haiku", "sonnet", "opus"},
		escalationLadder: []string{"haiku", "sonnet", "opus"}, // Simulating the default set by Open()
	}

	// Both nextTier and isTopTier should behave the same as before
	tier, ok := store.nextTier("haiku")
	if tier != "sonnet" || !ok {
		t.Errorf("nextTier(haiku) with default ladder = (%q, %v), want (sonnet, true)", tier, ok)
	}

	isTop := store.isTopTier("opus")
	if !isTop {
		t.Errorf("isTopTier(opus) with default ladder = %v, want true", isTop)
	}
}

func TestResearchNextTierEmpty(t *testing.T) {
	// Test that research escalation is disabled when ladder is empty (unset)
	store := &sqliteStore{
		allowedModels:            []string{"haiku", "sonnet", "opus"},
		researchEscalationLadder: []string{}, // Empty = no research escalation
	}

	tests := []struct {
		model    string
		wantTier string
		wantOk   bool
	}{
		{"haiku", "", false},
		{"sonnet", "", false},
		{"opus", "", false},
		{"unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gotTier, gotOk := store.researchNextTier(tt.model)
			if gotTier != tt.wantTier || gotOk != tt.wantOk {
				t.Errorf("researchNextTier(%q) = (%q, %v), want (%q, %v)", tt.model, gotTier, gotOk, tt.wantTier, tt.wantOk)
			}
		})
	}
}

func TestResearchNextTierConfigured(t *testing.T) {
	// Test that research escalation works when ladder is configured
	store := &sqliteStore{
		allowedModels:            []string{"haiku", "sonnet", "opus"},
		researchEscalationLadder: []string{"haiku", "sonnet", "opus"},
	}

	tests := []struct {
		model    string
		wantTier string
		wantOk   bool
	}{
		{"haiku", "sonnet", true},
		{"sonnet", "opus", true},
		{"opus", "", false},
		{"unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gotTier, gotOk := store.researchNextTier(tt.model)
			if gotTier != tt.wantTier || gotOk != tt.wantOk {
				t.Errorf("researchNextTier(%q) = (%q, %v), want (%q, %v)", tt.model, gotTier, gotOk, tt.wantTier, tt.wantOk)
			}
		})
	}
}

func TestResearchNextTierCustomLadder(t *testing.T) {
	// Test that research escalation respects custom ladder
	store := &sqliteStore{
		allowedModels:            []string{"haiku", "sonnet", "opus", "fable"},
		researchEscalationLadder: []string{"haiku", "opus", "fable"},
	}

	tests := []struct {
		model    string
		wantTier string
		wantOk   bool
	}{
		{"haiku", "opus", true},
		{"opus", "fable", true},
		{"fable", "", false},
		{"sonnet", "", false}, // sonnet is in allowedModels but not in research ladder
		{"unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gotTier, gotOk := store.researchNextTier(tt.model)
			if gotTier != tt.wantTier || gotOk != tt.wantOk {
				t.Errorf("researchNextTier(%q) = (%q, %v), want (%q, %v)", tt.model, gotTier, gotOk, tt.wantTier, tt.wantOk)
			}
		})
	}
}

func TestIsResearchTopTierEmpty(t *testing.T) {
	// Test that isResearchTopTier returns false when ladder is empty (unset)
	store := &sqliteStore{
		allowedModels:            []string{"haiku", "sonnet", "opus"},
		researchEscalationLadder: []string{}, // Empty = no research escalation
	}

	tests := []struct {
		model string
		want  bool
	}{
		{"haiku", false},
		{"sonnet", false},
		{"opus", false},
		{"unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := store.isResearchTopTier(tt.model)
			if got != tt.want {
				t.Errorf("isResearchTopTier(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestIsResearchTopTierConfigured(t *testing.T) {
	// Test that isResearchTopTier respects configured ladder
	store := &sqliteStore{
		allowedModels:            []string{"haiku", "sonnet", "opus", "fable"},
		researchEscalationLadder: []string{"haiku", "opus", "fable"},
	}

	tests := []struct {
		model string
		want  bool
	}{
		{"haiku", false},
		{"opus", false},
		{"fable", true},
		{"sonnet", false}, // sonnet is in allowedModels but not in research ladder
		{"unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := store.isResearchTopTier(tt.model)
			if got != tt.want {
				t.Errorf("isResearchTopTier(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}
