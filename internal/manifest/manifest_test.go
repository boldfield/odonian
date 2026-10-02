package manifest

import (
	"errors"
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
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1", "claim-2", "claim-3"},
				SourceStartPoints:  []string{"https://example.com/advisory"},
				FileScope:          []string{"claims.md"},
				AcceptanceCriteria: []string{"all claims verified against primary sources"},
				Dependencies:       []Dependency{},
			},
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-1", Disposition: Assigned},
			{ClaimID: "claim-2", Disposition: Assigned},
			{ClaimID: "claim-3", Disposition: Assigned},
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
	assertErrorCode(t, err, "NIL_MANIFEST")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "INVALID_VERSION")
}

func TestNegativeVersion(t *testing.T) {
	manifest := &Manifest{
		Version:      -1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file.md"}),
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "INVALID_VERSION")
}

func TestUnsupportedVersion(t *testing.T) {
	manifest := &Manifest{
		Version:      99,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file.md"}),
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "INVALID_VERSION")
}

func TestMissingParentTaskID(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "",
		Children: []Child{
			createValidChild("child-1", []string{"file.md"}),
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_PARENT_TASK_ID")
}

func TestNoChildren(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children:     []Child{},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "NO_CHILDREN")
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
	assertErrorCode(t, err, "TOO_MANY_CHILDREN")
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
	assertErrorCode(t, err, "DUPLICATE_KEY")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_KEY")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_TITLE")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_SPEC")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_TRACK")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "UNKNOWN_TRACK")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_MODEL")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "UNKNOWN_MODEL")
}

func TestInvalidReviewModelsCount(t *testing.T) {
	tests := []struct {
		name     string
		models   []string
		wantCode string
	}{
		{"no models", []string{}, "INVALID_REVIEW_MODELS"},
		{"single model", []string{"claude-sonnet-5"}, "INVALID_REVIEW_MODELS"},
		{"three models", []string{"claude-sonnet-5", "gpt-5-5", "claude-fable-5-1"}, "INVALID_REVIEW_MODELS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
						ReviewModels:       tt.models,
						AgentMerge:         ptrBool(false),
						Escalate:           ptrBool(true),
						ClaimIDs:           []string{"claim-1"},
						SourceStartPoints:  []string{"source-1"},
						FileScope:          []string{"file.md"},
						AcceptanceCriteria: []string{"criterion-1"},
						Dependencies:       []Dependency{},
					},
				},
			}

			err := manifest.Validate(validModels, validTracks)
			assertErrorCode(t, err, tt.wantCode)
		})
	}
}

func TestDuplicateReviewModels(t *testing.T) {
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
				ReviewModels:       []string{"gpt-5-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "DUPLICATE_REVIEW_MODELS")
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
				ReviewModels:       []string{"unknown-reviewer", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "UNKNOWN_REVIEW_MODEL")
}

func TestMissingAgentMerge(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         nil,
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_AGENT_MERGE")
}

func TestMissingEscalate(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           nil,
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_ESCALATE")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_CLAIMS")
}

func TestBlankClaimID(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1", ""},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "BLANK_CLAIM_ID")
}

func TestDuplicateClaimIDWithinChild(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1", "claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "DUPLICATE_CLAIM_ID")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "OVERSIZED_CLAIMS")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_SOURCES")
}

func TestBlankSourceStartPoint(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1", ""},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "BLANK_SOURCE")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"s1", "s2", "s3", "s4", "s5"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "OVERSIZED_SOURCES")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_FILE_SCOPE")
}

func TestBlankFileInFileScope(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md", ""},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "BLANK_FILE")
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISSING_ACCEPTANCE_CRITERIA")
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
	assertErrorCode(t, err, "OVERLAPPING_FILES")
}

func TestPathNormalizationInFileOverlaps(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"./file.md"}),
			createValidChild("child-2", []string{"file.md"}),
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "OVERLAPPING_FILES")
}

func TestDependencyCycle(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file-1.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies: []Dependency{
					{Kind: DependencyChild, Ref: "child-2"},
				},
			},
			{
				Key:                "child-2",
				Title:              "Test 2",
				Spec:               "Test spec 2",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-2"},
				SourceStartPoints:  []string{"source-2"},
				FileScope:          []string{"file-2.md"},
				AcceptanceCriteria: []string{"criterion-2"},
				Dependencies: []Dependency{
					{Kind: DependencyChild, Ref: "child-1"},
				},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "DEPENDENCY_CYCLE")
}

func TestUnknownChildDependency(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies: []Dependency{
					{Kind: DependencyChild, Ref: "no-such-sibling"},
				},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "UNKNOWN_CHILD_DEPENDENCY")
}

func TestBlankDependencyRef(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies: []Dependency{
					{Kind: DependencyChild, Ref: ""},
				},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "BLANK_DEPENDENCY_REF")
}

func TestInvalidTaskID(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies: []Dependency{
					{Kind: DependencyTask, Ref: "invalid"},
				},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "INVALID_TASK_ID")
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
			candidates: []PendingCandidate{{ClaimID: "claim-child-1", Disposition: Assigned}},
		},
		{
			name: "carried forward without owner",
			candidates: []PendingCandidate{
				{ClaimID: "claim-child-1", Disposition: Assigned},
				{ClaimID: "claim-other-1", Disposition: CarriedForward},
			},
			expectError: true,
			errorCode:   "MISSING_OWNER",
		},
		{
			name: "carried forward with owner",
			candidates: []PendingCandidate{
				{ClaimID: "claim-child-1", Disposition: Assigned},
				{ClaimID: "claim-other-1", Disposition: CarriedForward, Owner: ptrString("owner-1")},
			},
		},
		{
			name: "excluded without reason",
			candidates: []PendingCandidate{
				{ClaimID: "claim-child-1", Disposition: Assigned},
				{ClaimID: "claim-other-2", Disposition: Excluded},
			},
			expectError: true,
			errorCode:   "MISSING_REASON",
		},
		{
			name: "excluded with reason",
			candidates: []PendingCandidate{
				{ClaimID: "claim-child-1", Disposition: Assigned},
				{ClaimID: "claim-other-2", Disposition: Excluded, Reason: ptrString("no source available")},
			},
		},
		{
			name:        "empty claim ID",
			candidates:  []PendingCandidate{{ClaimID: "", Disposition: Assigned}},
			expectError: true,
			errorCode:   "INVALID_CANDIDATE",
		},
		{
			name: "invalid disposition",
			candidates: []PendingCandidate{
				{ClaimID: "claim-child-1", Disposition: "invalid"},
			},
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
				assertErrorCode(t, err, tt.errorCode)
			} else {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			}
		})
	}
}

func TestDuplicateCandidateID(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file.md"}),
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-child-1", Disposition: Assigned},
			{ClaimID: "claim-child-1", Disposition: Excluded, Reason: ptrString("duplicate")},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "DUPLICATE_CANDIDATE")
}

func TestUnassignedCandidate(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file.md"}),
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-999", Disposition: Assigned},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "UNASSIGNED_CANDIDATE")
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
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-child-1", Disposition: Assigned},
			{ClaimID: "claim-child-2", Disposition: Assigned},
			{ClaimID: "claim-child-3", Disposition: Assigned},
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file-1.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
			{
				Key:                "child-2",
				Title:              "Test 2",
				Spec:               "Test spec 2",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-2"},
				SourceStartPoints:  []string{"source-2"},
				FileScope:          []string{"file-2.md"},
				AcceptanceCriteria: []string{"criterion-2"},
				Dependencies: []Dependency{
					{Kind: DependencyChild, Ref: "child-1"},
				},
			},
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-1", Disposition: Assigned},
			{ClaimID: "claim-2", Disposition: Assigned},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	if err != nil {
		t.Errorf("valid dependencies failed: %v", err)
	}
}

func TestDuplicateClaimAcrossChildren(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file-1.md"}),
			{
				Key:                "child-2",
				Title:              "Test",
				Spec:               "Test spec",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-child-1"}, // Same as child-1
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file-2.md"},
				AcceptanceCriteria: []string{"criterion-1"},
				Dependencies:       []Dependency{},
			},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "DUPLICATE_CLAIM_ASSIGNMENT")
}

// Regression test for invalid task ID with non-hex characters
func TestInvalidTaskIDNonHex(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion"},
				Dependencies: []Dependency{
					{Kind: DependencyTask, Ref: "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"}, // invalid UUID
				},
			},
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-1", Disposition: Assigned},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "INVALID_TASK_ID")
}

// Regression test for contradictory dispositions
func TestContradictoryDispositionCarriedForward(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file.md"}),
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-child-1", Disposition: CarriedForward, Owner: ptrString("owner-1")},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "CONTRADICTORY_DISPOSITION")
}

// Regression test for contradictory dispositions (excluded)
func TestContradictoryDispositionExcluded(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file.md"}),
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-child-1", Disposition: Excluded, Reason: ptrString("reason")},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "CONTRADICTORY_DISPOSITION")
}

// Regression test for mismatched parent dependency
func TestMismatchedParentDependency(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion"},
				Dependencies: []Dependency{
					{Kind: DependencyParent, Ref: "parent-999"}, // doesn't match parent-123
				},
			},
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-1", Disposition: Assigned},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MISMATCHED_PARENT_DEPENDENCY")
}

// Regression test for blank acceptance criteria
func TestBlankAcceptanceCriteria(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"valid criterion", "  ", "another criterion"}, // blank in middle
				Dependencies:       []Dependency{},
			},
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-1", Disposition: Assigned},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "BLANK_CRITERION")
}

// Two children may each depend on the parent.
func TestParentDependencyPerChildAllowed(t *testing.T) {
	c1 := createValidChild("child-1", []string{"file-1.md"})
	c1.Dependencies = []Dependency{{Kind: DependencyParent, Ref: "parent-123"}}
	c2 := createValidChild("child-2", []string{"file-2.md"})
	c2.Dependencies = []Dependency{{Kind: DependencyParent, Ref: "parent-123"}}
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children:     []Child{c1, c2},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-child-1", Disposition: Assigned},
			{ClaimID: "claim-child-2", Disposition: Assigned},
		},
	}

	if err := manifest.Validate(validModels, validTracks); err != nil {
		t.Fatalf("expected two children depending on the parent to pass, got %v", err)
	}
}

// A single child may not list the parent dependency twice.
func TestDuplicateParentDependencyWithinChild(t *testing.T) {
	c1 := createValidChild("child-1", []string{"file-1.md"})
	c1.Dependencies = []Dependency{
		{Kind: DependencyParent, Ref: "parent-123"},
		{Kind: DependencyParent, Ref: "parent-123"},
	}
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children:     []Child{c1},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-child-1", Disposition: Assigned},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "MULTIPLE_PARENT_DEPENDENCIES")
	if err != nil && !strings.Contains(err.Error(), `child "child-1"`) {
		t.Errorf("expected message to name the child, got %v", err)
	}
}

func TestDuplicateChildAndTaskDependencyWithinChild(t *testing.T) {
	taskID := "12345678-1234-1234-1234-123456789abc"
	cases := map[string]Dependency{
		"child": {Kind: DependencyChild, Ref: "child-1"},
		"task":  {Kind: DependencyTask, Ref: taskID},
	}
	for name, dep := range cases {
		t.Run(name, func(t *testing.T) {
			c1 := createValidChild("child-1", []string{"file-1.md"})
			c2 := createValidChild("child-2", []string{"file-2.md"})
			c2.Dependencies = []Dependency{dep, dep}
			manifest := &Manifest{
				Version:      1,
				ParentTaskID: "parent-123",
				Children:     []Child{c1, c2},
				PendingCandidates: []PendingCandidate{
					{ClaimID: "claim-child-1", Disposition: Assigned},
					{ClaimID: "claim-child-2", Disposition: Assigned},
				},
			}
			err := manifest.Validate(validModels, validTracks)
			assertErrorCode(t, err, "DUPLICATE_DEPENDENCY")
		})
	}
}

func TestInvalidFileScope(t *testing.T) {
	cases := map[string]string{
		"dot":            ".",
		"dot-slash":      "./",
		"absolute":       "/repo/x.md",
		"parent-escape":  "../../etc/passwd",
		"bare-parent":    "..",
		"cleaned-escape": "a/../../x",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := &Manifest{
				Version:      1,
				ParentTaskID: "parent-123",
				Children: []Child{
					createValidChild("child-1", []string{path}),
					createValidChild("child-2", []string{"x.md"}),
				},
				PendingCandidates: []PendingCandidate{
					{ClaimID: "claim-child-1", Disposition: Assigned},
					{ClaimID: "claim-child-2", Disposition: Assigned},
				},
			}
			err := manifest.Validate(validModels, validTracks)
			assertErrorCode(t, err, "INVALID_FILE_SCOPE")
		})
	}
}

// Regression test for unmatched child claim (no assigned candidate entry)
func TestUnmatchedChildClaim(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			createValidChild("child-1", []string{"file.md"}),
		},
		PendingCandidates: []PendingCandidate{
			// claim-child-1 is in the child's claim_ids but has no candidate entry
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "UNMATCHED_CHILD_CLAIM")
}

// Regression test for unknown dependency kind
func TestUnknownDependencyKind(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"file.md"},
				AcceptanceCriteria: []string{"criterion"},
				Dependencies: []Dependency{
					{Kind: DependencyKind("invalid-kind"), Ref: "some-ref"},
				},
			},
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-1", Disposition: Assigned},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "UNKNOWN_DEPENDENCY_KIND")
}

// Regression test for file overlap with correct child attribution
func TestFileOverlapAttributionDeterministic(t *testing.T) {
	manifest := &Manifest{
		Version:      1,
		ParentTaskID: "parent-123",
		Children: []Child{
			{
				Key:                "child-a",
				Title:              "Test A",
				Spec:               "Test spec A",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-a"},
				SourceStartPoints:  []string{"source-a"},
				FileScope:          []string{"x/a.md"},
				AcceptanceCriteria: []string{"criterion-a"},
				Dependencies:       []Dependency{},
			},
			{
				Key:                "child-b",
				Title:              "Test B",
				Spec:               "Test spec B",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-b"},
				SourceStartPoints:  []string{"source-b"},
				FileScope:          []string{"x"},
				AcceptanceCriteria: []string{"criterion-b"},
				Dependencies:       []Dependency{},
			},
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-a", Disposition: Assigned},
			{ClaimID: "claim-b", Disposition: Assigned},
		},
	}

	// Run multiple times to check for nondeterminism
	for i := 0; i < 10; i++ {
		err := manifest.Validate(validModels, validTracks)
		if err == nil {
			t.Errorf("iteration %d: expected overlap error, got nil", i)
			continue
		}

		var valErr ValidationError
		if !errors.As(err, &valErr) {
			t.Errorf("iteration %d: expected ValidationError, got %T", i, err)
			continue
		}

		if valErr.Code != "OVERLAPPING_FILES" {
			t.Errorf("iteration %d: expected OVERLAPPING_FILES, got %s", i, valErr.Code)
			continue
		}

		// Check that the error message correctly attributes x/a.md to child-a and x to child-b
		if !strings.Contains(valErr.Message, "\"x/a.md\" (child \"child-a\")") || !strings.Contains(valErr.Message, "\"x\" (child \"child-b\")") {
			t.Errorf("iteration %d: error message missing expected child attributions: %s", i, valErr.Message)
		}
	}
}

// Regression test for file overlap with prefix
func TestFileOverlapWithPrefix(t *testing.T) {
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
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-1"},
				SourceStartPoints:  []string{"source-1"},
				FileScope:          []string{"docs"}, // Directory
				AcceptanceCriteria: []string{"criterion"},
				Dependencies:       []Dependency{},
			},
			{
				Key:                "child-2",
				Title:              "Test 2",
				Spec:               "Test spec 2",
				Track:              "research",
				Model:              "claude-opus-5-5",
				ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
				AgentMerge:         ptrBool(false),
				Escalate:           ptrBool(true),
				ClaimIDs:           []string{"claim-2"},
				SourceStartPoints:  []string{"source-2"},
				FileScope:          []string{"docs/readme.md"}, // File inside docs directory - overlaps!
				AcceptanceCriteria: []string{"criterion"},
				Dependencies:       []Dependency{},
			},
		},
		PendingCandidates: []PendingCandidate{
			{ClaimID: "claim-1", Disposition: Assigned},
			{ClaimID: "claim-2", Disposition: Assigned},
		},
	}

	err := manifest.Validate(validModels, validTracks)
	assertErrorCode(t, err, "OVERLAPPING_FILES")
}

// Helper function to create a valid child with custom file scope and unique claim ID
func createValidChild(key string, fileScope []string) Child {
	return Child{
		Key:                key,
		Title:              "Test Title",
		Spec:               "Test specification",
		Track:              "research",
		Model:              "claude-opus-5-5",
		ReviewModels:       []string{"claude-sonnet-5", "gpt-5-5"},
		AgentMerge:         ptrBool(false),
		Escalate:           ptrBool(true),
		ClaimIDs:           []string{"claim-" + key}, // Use key to make claim ID unique
		SourceStartPoints:  []string{"source-1"},
		FileScope:          fileScope,
		AcceptanceCriteria: []string{"acceptance criterion"},
		Dependencies:       []Dependency{},
	}
}

func ptrBool(b bool) *bool {
	return &b
}

func ptrString(s string) *string {
	return &s
}

// Helper to check ValidationError code using errors.As
func assertErrorCode(t *testing.T, err error, expectedCode string) {
	t.Helper()
	if err == nil {
		t.Errorf("expected error with code %s, got nil", expectedCode)
		return
	}
	var valErr ValidationError
	if !errors.As(err, &valErr) {
		t.Errorf("expected ValidationError, got %T: %v", err, err)
		return
	}
	if valErr.Code != expectedCode {
		t.Errorf("expected error code %s, got %s", expectedCode, valErr.Code)
	}
}
