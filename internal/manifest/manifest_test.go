package manifest

import (
	"strings"
	"testing"
)

var (
	validModels = map[string]bool{
		"claude-opus-5-5":  true,
		"claude-sonnet-5":  true,
		"claude-haiku-4-5": true,
		"claude-fable-5-1": true,
		"gpt-5-5":          true,
	}

	validTracks = map[string]bool{
		"research": true,
		"build":    true,
		"design":   true,
	}
)

func TestValidManifest(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Verify security claims",
				Spec:               "Verify all security-related claims in the advisory",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         false,
				Escalate:           true,
				ClaimIDs:           []string{"claim-1", "claim-2", "claim-3"},
				SourceStartPoints:  []string{"https://example.com/advisory"},
				FileScope:          []string{"claims.md"},
				AcceptanceCriteria: []string{"all claims verified against primary sources"},
				Dependencies:       []string{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err != nil {
		t.Errorf("valid manifest failed validation: %v", err)
	}
}

func TestNilManifest(t *testing.T) {
	var manifest *Manifest
	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for nil manifest")
	}
	if !strings.Contains(err.Error(), "NIL_MANIFEST") {
		t.Errorf("expected NIL_MANIFEST error, got: %v", err)
	}
}

func TestInvalidVersion(t *testing.T) {
	manifest := &Manifest{
		Version:      0,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for invalid version")
	}
	if !strings.Contains(err.Error(), "INVALID_VERSION") {
		t.Errorf("expected INVALID_VERSION error, got: %v", err)
	}
}

func TestMissingParentTaskID(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing parent task ID")
	}
	if !strings.Contains(err.Error(), "MISSING_PARENT_TASK_ID") {
		t.Errorf("expected MISSING_PARENT_TASK_ID error, got: %v", err)
	}
}

func TestNoChildren(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children:     []Child{},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for no children")
	}
	if !strings.Contains(err.Error(), "NO_CHILDREN") {
		t.Errorf("expected NO_CHILDREN error, got: %v", err)
	}
}

func TestTooManyChildren(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file-1.md"}),
			createValidChild("child-2", []string{"file-2.md"}),
			createValidChild("child-3", []string{"file-3.md"}),
			createValidChild("child-4", []string{"file-4.md"}),
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for too many children")
	}
	if !strings.Contains(err.Error(), "TOO_MANY_CHILDREN") {
		t.Errorf("expected TOO_MANY_CHILDREN error, got: %v", err)
	}
}

func TestDuplicateChildKey(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file-1.md"}),
			createValidChild("child-1", []string{"file-2.md"}),
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for duplicate key")
	}
	if !strings.Contains(err.Error(), "DUPLICATE_KEY") {
		t.Errorf("expected DUPLICATE_KEY error, got: %v", err)
	}
}

func TestMissingChildKey(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "MISSING_KEY") {
		t.Errorf("expected MISSING_KEY error, got: %v", err)
	}
}

func TestMissingChildTitle(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing title")
	}
	if !strings.Contains(err.Error(), "MISSING_TITLE") {
		t.Errorf("expected MISSING_TITLE error, got: %v", err)
	}
}

func TestMissingChildSpec(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing spec")
	}
	if !strings.Contains(err.Error(), "MISSING_SPEC") {
		t.Errorf("expected MISSING_SPEC error, got: %v", err)
	}
}

func TestMissingChildTrack(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing track")
	}
	if !strings.Contains(err.Error(), "MISSING_TRACK") {
		t.Errorf("expected MISSING_TRACK error, got: %v", err)
	}
}

func TestUnknownTrack(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "unknown",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for unknown track")
	}
	if !strings.Contains(err.Error(), "UNKNOWN_TRACK") {
		t.Errorf("expected UNKNOWN_TRACK error, got: %v", err)
	}
}

func TestMissingChildModel(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing model")
	}
	if !strings.Contains(err.Error(), "MISSING_MODEL") {
		t.Errorf("expected MISSING_MODEL error, got: %v", err)
	}
}

func TestUnknownModel(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "unknown-model",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for unknown model")
	}
	if !strings.Contains(err.Error(), "UNKNOWN_MODEL") {
		t.Errorf("expected UNKNOWN_MODEL error, got: %v", err)
	}
}

func TestMissingReviewModels(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing review models")
	}
	if !strings.Contains(err.Error(), "MISSING_REVIEW_MODELS") {
		t.Errorf("expected MISSING_REVIEW_MODELS error, got: %v", err)
	}
}

func TestTooManyReviewModels(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5", "claude-fable-5-1"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for too many review models")
	}
	if !strings.Contains(err.Error(), "TOO_MANY_REVIEW_MODELS") {
		t.Errorf("expected TOO_MANY_REVIEW_MODELS error, got: %v", err)
	}
}

func TestUnknownReviewModel(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"unknown-reviewer"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for unknown review model")
	}
	if !strings.Contains(err.Error(), "UNKNOWN_REVIEW_MODEL") {
		t.Errorf("expected UNKNOWN_REVIEW_MODEL error, got: %v", err)
	}
}

func TestMissingClaimIDs(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing claim IDs")
	}
	if !strings.Contains(err.Error(), "MISSING_CLAIMS") {
		t.Errorf("expected MISSING_CLAIMS error, got: %v", err)
	}
}

func TestOversizedClaims(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for oversized claims")
	}
	if !strings.Contains(err.Error(), "OVERSIZED_CLAIMS") {
		t.Errorf("expected OVERSIZED_CLAIMS error, got: %v", err)
	}
}

func TestMissingSourceStartPoints(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing source start points")
	}
	if !strings.Contains(err.Error(), "MISSING_SOURCES") {
		t.Errorf("expected MISSING_SOURCES error, got: %v", err)
	}
}

func TestOversizedSources(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"s1", "s2", "s3", "s4", "s5"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for oversized sources")
	}
	if !strings.Contains(err.Error(), "OVERSIZED_SOURCES") {
		t.Errorf("expected OVERSIZED_SOURCES error, got: %v", err)
	}
}

func TestMissingFileScope(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{},
				AcceptanceCriteria: []string{"criterion-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing file scope")
	}
	if !strings.Contains(err.Error(), "MISSING_FILE_SCOPE") {
		t.Errorf("expected MISSING_FILE_SCOPE error, got: %v", err)
	}
}

func TestMissingAcceptanceCriteria(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for missing acceptance criteria")
	}
	if !strings.Contains(err.Error(), "MISSING_ACCEPTANCE_CRITERIA") {
		t.Errorf("expected MISSING_ACCEPTANCE_CRITERIA error, got: %v", err)
	}
}

func TestOverlappingFiles(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file.md", "other.md"}),
			createValidChild("child-2", []string{"file.md"}),
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for overlapping files")
	}
	if !strings.Contains(err.Error(), "OVERLAPPING_FILES") {
		t.Errorf("expected OVERLAPPING_FILES error, got: %v", err)
	}
}

func TestDependencyCycle(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file-1.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []string{"child-2"},
			},
			{
				Key:                "child-2",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-2"},
				SourceStartPoints:  []string{"source-2"},
				FileScope:          []string{"file-2.md"},
				AcceptanceCriteria: []string{"criterion-2"},
				Dependencies:       []string{"child-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err == nil {
		t.Error("expected error for dependency cycle")
	}
	if !strings.Contains(err.Error(), "DEPENDENCY_CYCLE") {
		t.Errorf("expected DEPENDENCY_CYCLE error, got: %v", err)
	}
}

func TestPendingCandidates(t *testing.T) {
	tests := []struct {
		name        string
		candidates  []PendingCandidate
		expectError bool
		errorCode   string
	}{
		{
			name:       "valid assigned",
			candidates: []PendingCandidate{{ClaimID: "claim-1", Disposition: ChildAssigned}},
		},
		{
			name:        "carried forward without owner",
			candidates:  []PendingCandidate{{ClaimID: "claim-1", Disposition: ChildCarriedForward}},
			expectError: true,
			errorCode:   "MISSING_OWNER",
		},
		{
			name: "carried forward with owner",
			candidates: []PendingCandidate{
				{ClaimID: "claim-1", Disposition: ChildCarriedForward, Owner: ptrString("owner-1")},
			},
		},
		{
			name:        "excluded without reason",
			candidates:  []PendingCandidate{{ClaimID: "claim-1", Disposition: ChildExcluded}},
			expectError: true,
			errorCode:   "MISSING_REASON",
		},
		{
			name: "excluded with reason",
			candidates: []PendingCandidate{
				{ClaimID: "claim-1", Disposition: ChildExcluded, Reason: ptrString("no source available")},
			},
		},
		{
			name:        "empty claim ID",
			candidates:  []PendingCandidate{{ClaimID: "", Disposition: ChildAssigned}},
			expectError: true,
			errorCode:   "INVALID_CANDIDATE",
		},
		{
			name:        "invalid disposition",
			candidates:  []PendingCandidate{{ClaimID: "claim-1", Disposition: "invalid"}},
			expectError: true,
			errorCode:   "INVALID_DISPOSITION",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := &Manifest{
				Version:           1,
				ParentTaskID:      "parent-123",
				Children:          []Child{createValidChild("child-1", []string{"file.md"})},
				PendingCandidates: tt.candidates,
			}

			err := manifest.Validate(validModels, validTracks)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error with code %s, got nil", tt.errorCode)
				} else if !strings.Contains(err.Error(), tt.errorCode) {
					t.Errorf("expected error code %s, got: %v", tt.errorCode, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			}
		})
	}
}

func TestThreeChildrenWithNoDeps(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file-1.md"}),
			createValidChild("child-2", []string{"file-2.md"}),
			createValidChild("child-3", []string{"file-3.md"}),
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err != nil {
		t.Errorf("valid three-child manifest failed: %v", err)
	}
}

func TestValidDependencies(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-1",
				Title:              "Test 1",
				Spec:               "Test spec 1",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file-1.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []string{},
			},
			{
				Key:                "child-2",
				Title:              "Test 2",
				Spec:               "Test spec 2",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5"},
				ClaimIDs:           []string{"claim-2"},
				SourceStartPoints:  []string{"source-2"},
				FileScope:          []string{"file-2.md"},
				AcceptanceCriteria: []string{"criterion-2"},
				Dependencies:       []string{"child-1"},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err != nil {
		t.Errorf("valid dependencies failed: %v", err)
	}
}

// Helper function to create a valid child with custom file scope
func createValidChild(key string, fileScope []string) Child {
	return Child{
		Key:                key,
		Title:              "Test Title",
		Spec:               "Test specification",
		Track:              "research",
		Model:              "claude-opus-5-5",
		ReviewModels:       []string{"claude-sonnet-5"},
		AgentMerge:         false,
		Escalate:           true,
		ClaimIDs:           []string{"claim-1"},
		SourceStartPoints:  []string{"source-1"},
		FileScope:          fileScope,
		AcceptanceCriteria: []string{"acceptance criterion"},
		Dependencies:       []string{},
	}
}

func ptrString(s string) *string {
	return &s
}
