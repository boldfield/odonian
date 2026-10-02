package manifest

import (
	"fmt"
	"strings"
)

// ChildDisposition represents the disposition of a child task in the manifest
type ChildDisposition string

const (
	ChildAssigned       ChildDisposition = "assigned"
	ChildCarriedForward ChildDisposition = "carried_forward"
	ChildExcluded       ChildDisposition = "excluded"
)

// Child represents a fully specified child task in a research continuation manifest
type Child struct {
	Key                string   `json:"key"`                 // stable parent-scoped key
	Title              string   `json:"title"`               // short title
	Spec               string   `json:"spec"`                // full prose specification
	Track              string   `json:"track"`               // "research", "build", or "design"
	Model              string   `json:"model"`               // model name
	ReviewModels       []string `json:"review_models"`       // pair of reviewer models
	AgentMerge         bool     `json:"agent_merge"`         // whether agent can merge
	Escalate           bool     `json:"escalate"`            // whether escalation is enabled
	ClaimIDs           []string `json:"claim_ids"`           // exact claim IDs this task will verify
	SourceStartPoints  []string `json:"source_start_points"` // starting references for sources
	FileScope          []string `json:"file_scope"`          // files this task touches
	AcceptanceCriteria []string `json:"acceptance_criteria"` // acceptance criteria as strings
	Dependencies       []string `json:"dependencies"`        // task IDs or keys this depends on
}

// PendingCandidate represents a pending candidate claim and its disposition
type PendingCandidate struct {
	ClaimID     string           `json:"claim_id"`
	Disposition ChildDisposition `json:"disposition"`
	Owner       *string          `json:"owner,omitempty"`  // for carried_forward
	Reason      *string          `json:"reason,omitempty"` // for excluded
}

// Manifest represents a versioned research continuation manifest
type Manifest struct {
	Version           int                `json:"version"`            // manifest version
	ParentTaskID      string             `json:"parent_task_id"`     // ID of the parent task
	Children          []Child            `json:"children"`           // up to 3 children
	PendingCandidates []PendingCandidate `json:"pending_candidates"` // dispositions of pending claims
}

// ValidationError represents a validation error with a specific code
type ValidationError struct {
	Code    string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Validate validates the manifest according to research continuation rules
func (m *Manifest) Validate(allowedModels, allowedTracks map[string]bool) error {
	if m == nil {
		return ValidationError{"NIL_MANIFEST", "manifest cannot be nil"}
	}

	if m.Version == 0 {
		return ValidationError{"INVALID_VERSION", "manifest version must be > 0"}
	}

	if strings.TrimSpace(m.ParentTaskID) == "" {
		return ValidationError{"MISSING_PARENT_TASK_ID", "parent_task_id is required"}
	}

	if len(m.Children) == 0 {
		return ValidationError{"NO_CHILDREN", "manifest must contain at least one child"}
	}

	if len(m.Children) > 3 {
		return ValidationError{"TOO_MANY_CHILDREN", "manifest cannot have more than 3 children"}
	}

	// Check for duplicate keys and validate each child
	seenKeys := make(map[string]bool)
	childFileScopes := make(map[string][]string) // track file scopes per child
	claimCounts := make(map[string]int)          // track claim count per child
	primarySourceCounts := make(map[string]int)  // track primary source count per child

	for i, child := range m.Children {
		if err := validateChild(child, allowedModels, allowedTracks, i); err != nil {
			return err
		}

		if seenKeys[child.Key] {
			return ValidationError{"DUPLICATE_KEY", fmt.Sprintf("child key %q is duplicated", child.Key)}
		}
		seenKeys[child.Key] = true

		childFileScopes[child.Key] = child.FileScope
		claimCounts[child.Key] = len(child.ClaimIDs)
		primarySourceCounts[child.Key] = len(child.SourceStartPoints)
	}

	// Check for six-claim ceiling per child
	for key, count := range claimCounts {
		if count > 6 {
			return ValidationError{"OVERSIZED_CLAIMS", fmt.Sprintf("child %q has %d claims, max is 6", key, count)}
		}
	}

	// Check for four-primary-source ceiling per child
	for key, count := range primarySourceCounts {
		if count > 4 {
			return ValidationError{"OVERSIZED_SOURCES", fmt.Sprintf("child %q has %d primary sources, max is 4", key, count)}
		}
	}

	// Check for overlapping file writes (unsupported)
	if err := checkFileOverlaps(childFileScopes); err != nil {
		return err
	}

	// Check for dependency cycles
	if err := checkDependencyCycles(m.Children, seenKeys); err != nil {
		return err
	}

	// Validate pending candidates
	for _, candidate := range m.PendingCandidates {
		if strings.TrimSpace(candidate.ClaimID) == "" {
			return ValidationError{"INVALID_CANDIDATE", "claim_id is required for all candidates"}
		}

		switch candidate.Disposition {
		case ChildAssigned:
			// Valid, no additional checks
		case ChildCarriedForward:
			if candidate.Owner == nil || strings.TrimSpace(*candidate.Owner) == "" {
				return ValidationError{"MISSING_OWNER", fmt.Sprintf("claim %q marked carried_forward must have an owner", candidate.ClaimID)}
			}
		case ChildExcluded:
			if candidate.Reason == nil || strings.TrimSpace(*candidate.Reason) == "" {
				return ValidationError{"MISSING_REASON", fmt.Sprintf("claim %q marked excluded must have a reason", candidate.ClaimID)}
			}
		default:
			return ValidationError{"INVALID_DISPOSITION", fmt.Sprintf("unknown disposition %q for claim %q", candidate.Disposition, candidate.ClaimID)}
		}
	}

	return nil
}

func validateChild(child Child, allowedModels, allowedTracks map[string]bool, index int) error {
	prefix := fmt.Sprintf("child[%d]", index)

	if strings.TrimSpace(child.Key) == "" {
		return ValidationError{"MISSING_KEY", fmt.Sprintf("%s: key is required", prefix)}
	}

	if strings.TrimSpace(child.Title) == "" {
		return ValidationError{"MISSING_TITLE", fmt.Sprintf("%s: title is required", prefix)}
	}

	if strings.TrimSpace(child.Spec) == "" {
		return ValidationError{"MISSING_SPEC", fmt.Sprintf("%s: spec is required", prefix)}
	}

	if strings.TrimSpace(child.Track) == "" {
		return ValidationError{"MISSING_TRACK", fmt.Sprintf("%s: track is required", prefix)}
	}

	if !allowedTracks[child.Track] {
		return ValidationError{"UNKNOWN_TRACK", fmt.Sprintf("%s: unknown track %q", prefix, child.Track)}
	}

	if strings.TrimSpace(child.Model) == "" {
		return ValidationError{"MISSING_MODEL", fmt.Sprintf("%s: model is required", prefix)}
	}

	if !allowedModels[child.Model] {
		return ValidationError{"UNKNOWN_MODEL", fmt.Sprintf("%s: unknown model %q", prefix, child.Model)}
	}

	if len(child.ReviewModels) == 0 {
		return ValidationError{"MISSING_REVIEW_MODELS", fmt.Sprintf("%s: review_models is required and must have at least one entry", prefix)}
	}

	if len(child.ReviewModels) > 2 {
		return ValidationError{"TOO_MANY_REVIEW_MODELS", fmt.Sprintf("%s: review_models cannot have more than 2 entries", prefix)}
	}

	for _, model := range child.ReviewModels {
		if !allowedModels[model] {
			return ValidationError{"UNKNOWN_REVIEW_MODEL", fmt.Sprintf("%s: unknown review model %q", prefix, model)}
		}
	}

	if len(child.ClaimIDs) == 0 {
		return ValidationError{"MISSING_CLAIMS", fmt.Sprintf("%s: claim_ids is required and must have at least one entry", prefix)}
	}

	if len(child.SourceStartPoints) == 0 {
		return ValidationError{"MISSING_SOURCES", fmt.Sprintf("%s: source_start_points is required and must have at least one entry", prefix)}
	}

	if len(child.FileScope) == 0 {
		return ValidationError{"MISSING_FILE_SCOPE", fmt.Sprintf("%s: file_scope is required and must have at least one entry", prefix)}
	}

	if len(child.AcceptanceCriteria) == 0 {
		return ValidationError{"MISSING_ACCEPTANCE_CRITERIA", fmt.Sprintf("%s: acceptance_criteria is required and must have at least one entry", prefix)}
	}

	return nil
}

func checkFileOverlaps(fileScopes map[string][]string) error {
	fileToChild := make(map[string][]string)

	for child, files := range fileScopes {
		for _, file := range files {
			fileToChild[file] = append(fileToChild[file], child)
		}
	}

	for file, children := range fileToChild {
		if len(children) > 1 {
			return ValidationError{"OVERLAPPING_FILES", fmt.Sprintf("file %q is in multiple children: %v", file, children)}
		}
	}

	return nil
}

func checkDependencyCycles(children []Child, seenKeys map[string]bool) error {
	for _, child := range children {
		visited := make(map[string]bool)
		if hasCycle(child.Key, children, visited, seenKeys) {
			return ValidationError{"DEPENDENCY_CYCLE", fmt.Sprintf("dependency cycle detected starting at child %q", child.Key)}
		}
	}
	return nil
}

func hasCycle(nodeKey string, children []Child, visited, seenKeys map[string]bool) bool {
	if visited[nodeKey] {
		return true
	}

	visited[nodeKey] = true

	var node *Child
	for i := range children {
		if children[i].Key == nodeKey {
			node = &children[i]
			break
		}
	}

	if node == nil {
		return false
	}

	for _, dep := range node.Dependencies {
		// Only check dependencies within this child set
		if seenKeys[dep] && hasCycle(dep, children, visited, seenKeys) {
			return true
		}
	}

	delete(visited, nodeKey)
	return false
}
