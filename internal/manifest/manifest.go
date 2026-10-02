package manifest

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const CurrentVersion = 1

// Disposition represents the disposition of a candidate claim in the manifest
type Disposition string

const (
	Assigned       Disposition = "assigned"
	CarriedForward Disposition = "carried_forward"
	Excluded       Disposition = "excluded"
)

// DependencyKind represents the type of dependency
type DependencyKind string

const (
	DependencyParent DependencyKind = "parent"
	DependencyChild  DependencyKind = "child"
	DependencyTask   DependencyKind = "task"
)

// Dependency represents a typed dependency reference
type Dependency struct {
	Kind DependencyKind `json:"kind"` // parent, child, or task
	Ref  string         `json:"ref"`  // reference (key or task ID)
}

// Child represents a fully specified child task in a research continuation manifest
type Child struct {
	Key                string       `json:"key"`                 // stable parent-scoped key
	Title              string       `json:"title"`               // short title
	Spec               string       `json:"spec"`                // full prose specification
	Track              string       `json:"track"`               // "research", "build", or "design"
	Model              string       `json:"model"`               // model name
	ReviewModels       []string     `json:"review_models"`       // exactly 2 distinct reviewer models
	AgentMerge         *bool        `json:"agent_merge"`         // whether agent can merge
	Escalate           *bool        `json:"escalate"`            // whether escalation is enabled
	ClaimIDs           []string     `json:"claim_ids"`           // exact claim IDs this task will verify
	SourceStartPoints  []string     `json:"source_start_points"` // starting references for sources
	FileScope          []string     `json:"file_scope"`          // files this task touches
	AcceptanceCriteria []string     `json:"acceptance_criteria"` // acceptance criteria as strings
	Dependencies       []Dependency `json:"dependencies"`        // typed dependencies with kind
}

// PendingCandidate represents a pending candidate claim and its disposition
type PendingCandidate struct {
	ClaimID     string      `json:"claim_id"`
	Disposition Disposition `json:"disposition"`
	Owner       *string     `json:"owner,omitempty"`  // for carried_forward
	Reason      *string     `json:"reason,omitempty"` // for excluded
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

	if m.Version != CurrentVersion {
		return ValidationError{"INVALID_VERSION", fmt.Sprintf("manifest version must be %d, got %d", CurrentVersion, m.Version)}
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

	// Build index for dependency validation
	seenKeys := make(map[string]bool)
	childFileScopes := make(map[string][]string)
	allChildClaims := make(map[string]string)  // claim ID -> child key that contains it
	childClaimIDs := make(map[string][]string) // track all claim IDs per child for dedup

	// Validate each child and build indexes
	for i, child := range m.Children {
		if err := validateChild(child, allowedModels, allowedTracks, i); err != nil {
			return err
		}

		if seenKeys[child.Key] {
			return ValidationError{"DUPLICATE_KEY", fmt.Sprintf("child key %q is duplicated", child.Key)}
		}
		seenKeys[child.Key] = true

		childFileScopes[child.Key] = child.FileScope
		childClaimIDs[child.Key] = child.ClaimIDs

		// Track where each claim ID appears for cross-checking
		for _, claimID := range child.ClaimIDs {
			if existing, exists := allChildClaims[claimID]; exists {
				return ValidationError{"DUPLICATE_CLAIM_ASSIGNMENT", fmt.Sprintf("claim ID %q appears in both child %q and child %q", claimID, existing, child.Key)}
			}
			allChildClaims[claimID] = child.Key
		}

		// Check for oversized claims per child
		if len(child.ClaimIDs) > 6 {
			return ValidationError{"OVERSIZED_CLAIMS", fmt.Sprintf("child %q has %d claims, max is 6", child.Key, len(child.ClaimIDs))}
		}

		// Check for oversized sources per child (with dedup count)
		distinctSources := countDistinct(child.SourceStartPoints)
		if distinctSources > 4 {
			return ValidationError{"OVERSIZED_SOURCES", fmt.Sprintf("child %q has %d distinct primary sources, max is 4", child.Key, distinctSources)}
		}
	}

	// Check for overlapping file writes (unsupported)
	if err := checkFileOverlaps(m.Children, childFileScopes); err != nil {
		return err
	}

	// Check for dependency cycles and validate dependency references
	if err := checkDependencies(m.ParentTaskID, m.Children, seenKeys); err != nil {
		return err
	}

	// Validate pending candidates and cross-check with children
	if err := validatePendingCandidates(m.Children, m.PendingCandidates, allChildClaims, childClaimIDs); err != nil {
		return err
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

	// Reviewer pair must be exactly 2 distinct models
	if len(child.ReviewModels) != 2 {
		return ValidationError{"INVALID_REVIEW_MODELS", fmt.Sprintf("%s: review_models must have exactly 2 entries, got %d", prefix, len(child.ReviewModels))}
	}

	if child.ReviewModels[0] == child.ReviewModels[1] {
		return ValidationError{"DUPLICATE_REVIEW_MODELS", fmt.Sprintf("%s: review_models must be distinct, got duplicate %q", prefix, child.ReviewModels[0])}
	}

	for _, model := range child.ReviewModels {
		if !allowedModels[model] {
			return ValidationError{"UNKNOWN_REVIEW_MODEL", fmt.Sprintf("%s: unknown review model %q", prefix, model)}
		}
	}

	// Check agent_merge and escalate are present
	if child.AgentMerge == nil {
		return ValidationError{"MISSING_AGENT_MERGE", fmt.Sprintf("%s: agent_merge is required", prefix)}
	}

	if child.Escalate == nil {
		return ValidationError{"MISSING_ESCALATE", fmt.Sprintf("%s: escalate is required", prefix)}
	}

	// Validate claim IDs
	if len(child.ClaimIDs) == 0 {
		return ValidationError{"MISSING_CLAIMS", fmt.Sprintf("%s: claim_ids is required and must have at least one entry", prefix)}
	}

	for i, claimID := range child.ClaimIDs {
		if strings.TrimSpace(claimID) == "" {
			return ValidationError{"BLANK_CLAIM_ID", fmt.Sprintf("%s: claim_ids[%d] is blank", prefix, i)}
		}
	}

	// Check for duplicate claim IDs within a child
	claimSet := make(map[string]bool)
	for _, claimID := range child.ClaimIDs {
		if claimSet[claimID] {
			return ValidationError{"DUPLICATE_CLAIM_ID", fmt.Sprintf("%s: claim ID %q appears multiple times", prefix, claimID)}
		}
		claimSet[claimID] = true
	}

	// Validate source start points
	if len(child.SourceStartPoints) == 0 {
		return ValidationError{"MISSING_SOURCES", fmt.Sprintf("%s: source_start_points is required and must have at least one entry", prefix)}
	}

	for i, source := range child.SourceStartPoints {
		if strings.TrimSpace(source) == "" {
			return ValidationError{"BLANK_SOURCE", fmt.Sprintf("%s: source_start_points[%d] is blank", prefix, i)}
		}
	}

	// Validate file scope
	if len(child.FileScope) == 0 {
		return ValidationError{"MISSING_FILE_SCOPE", fmt.Sprintf("%s: file_scope is required and must have at least one entry", prefix)}
	}

	for i, file := range child.FileScope {
		if strings.TrimSpace(file) == "" {
			return ValidationError{"BLANK_FILE", fmt.Sprintf("%s: file_scope[%d] is blank", prefix, i)}
		}
	}

	// Validate acceptance criteria
	if len(child.AcceptanceCriteria) == 0 {
		return ValidationError{"MISSING_ACCEPTANCE_CRITERIA", fmt.Sprintf("%s: acceptance_criteria is required and must have at least one entry", prefix)}
	}

	for i, criterion := range child.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" {
			return ValidationError{"BLANK_CRITERION", fmt.Sprintf("%s: acceptance_criteria[%d] is blank", prefix, i)}
		}
	}

	return nil
}

func checkFileOverlaps(children []Child, fileScopes map[string][]string) error {
	// Build normalized file list from children in order for deterministic iteration
	type normalizedScope struct {
		childIdx int
		childKey string
		files    []string
	}
	var allNormalized []normalizedScope

	for i, child := range children {
		if files, exists := fileScopes[child.Key]; exists {
			var normalized []string
			for _, file := range files {
				normalized = append(normalized, filepath.Clean(file))
			}
			allNormalized = append(allNormalized, normalizedScope{i, child.Key, normalized})
		}
	}

	// Check for any overlaps (exact, prefix, or containment)
	for i := 0; i < len(allNormalized); i++ {
		for j := i + 1; j < len(allNormalized); j++ {
			scopeI := allNormalized[i]
			scopeJ := allNormalized[j]

			for _, fileI := range scopeI.files {
				for _, fileJ := range scopeJ.files {
					if hasOverlap(fileI, fileJ) {
						return ValidationError{"OVERLAPPING_FILES", fmt.Sprintf("unsupported overlapping file writes: %q (child %q) and %q (child %q)", fileI, scopeI.childKey, fileJ, scopeJ.childKey)}
					}
				}
			}
		}
	}

	return nil
}

func hasOverlap(fileA, fileB string) bool {
	// Check for exact match
	if fileA == fileB {
		return true
	}
	// Check for prefix/containment: one is a prefix of the other
	// (e.g., "docs" contains "docs/x.md" or vice versa)
	if strings.HasPrefix(fileA, fileB+"/") || strings.HasPrefix(fileB, fileA+"/") {
		return true
	}
	return false
}

func checkDependencies(parentTaskID string, children []Child, seenKeys map[string]bool) error {
	// Build key->index map for efficiency
	keyToIndex := make(map[string]int)
	for i, child := range children {
		keyToIndex[child.Key] = i
	}

	// Track parent dependencies to ensure at most one per manifest
	parentDepCount := 0

	// Validate each dependency reference
	for _, child := range children {
		for _, dep := range child.Dependencies {
			if strings.TrimSpace(dep.Ref) == "" {
				return ValidationError{"BLANK_DEPENDENCY_REF", fmt.Sprintf("child %q: dependency ref cannot be blank", child.Key)}
			}

			switch dep.Kind {
			case DependencyParent:
				// Parent dependency ref must match the manifest's parent task ID
				if dep.Ref != parentTaskID {
					return ValidationError{"MISMATCHED_PARENT_DEPENDENCY", fmt.Sprintf("child %q: parent dependency ref %q does not match parent task ID %q", child.Key, dep.Ref, parentTaskID)}
				}
				parentDepCount++
				if parentDepCount > 1 {
					return ValidationError{"MULTIPLE_PARENT_DEPENDENCIES", "at most one parent dependency per manifest"}
				}
			case DependencyChild:
				// Child dependency must reference an existing sibling key
				if !seenKeys[dep.Ref] {
					return ValidationError{"UNKNOWN_CHILD_DEPENDENCY", fmt.Sprintf("child %q: unknown child dependency %q", child.Key, dep.Ref)}
				}
			case DependencyTask:
				// External task ID must be well-formed (non-empty, valid UUID-like format)
				if !isValidTaskID(dep.Ref) {
					return ValidationError{"INVALID_TASK_ID", fmt.Sprintf("child %q: invalid task ID %q", child.Key, dep.Ref)}
				}
			default:
				return ValidationError{"UNKNOWN_DEPENDENCY_KIND", fmt.Sprintf("child %q: unknown dependency kind %q", child.Key, dep.Kind)}
			}
		}
	}

	// Check for dependency cycles (only among child dependencies)
	visited := make(map[string]int) // -1: visiting, 0: unvisited, 1: visited
	for _, child := range children {
		if visited[child.Key] == 0 {
			if hasCycleDFS(child.Key, children, keyToIndex, visited) {
				return ValidationError{"DEPENDENCY_CYCLE", fmt.Sprintf("dependency cycle detected involving child %q", child.Key)}
			}
		}
	}

	return nil
}

func hasCycleDFS(nodeKey string, children []Child, keyToIndex map[string]int, visited map[string]int) bool {
	visited[nodeKey] = -1 // mark as visiting

	nodeIdx := keyToIndex[nodeKey]
	node := children[nodeIdx]

	for _, dep := range node.Dependencies {
		// Only check child dependencies for cycles
		if dep.Kind != DependencyChild {
			continue
		}

		if visited[dep.Ref] == -1 {
			return true // back edge, cycle detected
		}

		if visited[dep.Ref] == 0 {
			if hasCycleDFS(dep.Ref, children, keyToIndex, visited) {
				return true
			}
		}
	}

	visited[nodeKey] = 1 // mark as visited
	return false
}

func isValidTaskID(taskID string) bool {
	if strings.TrimSpace(taskID) == "" {
		return false
	}
	_, err := uuid.Parse(taskID)
	return err == nil
}

func validatePendingCandidates(children []Child, candidates []PendingCandidate, allChildClaims map[string]string, childClaimIDs map[string][]string) error {
	// Check for unique candidate IDs
	seenCandidates := make(map[string]bool)
	assignedCandidates := make(map[string]bool)

	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ClaimID) == "" {
			return ValidationError{"INVALID_CANDIDATE", "claim_id is required for all candidates"}
		}

		// Check for duplicate candidates
		if seenCandidates[candidate.ClaimID] {
			return ValidationError{"DUPLICATE_CANDIDATE", fmt.Sprintf("claim %q appears multiple times in pending_candidates", candidate.ClaimID)}
		}
		seenCandidates[candidate.ClaimID] = true

		switch candidate.Disposition {
		case Assigned:
			// Assigned candidate must appear in exactly one child
			_, exists := allChildClaims[candidate.ClaimID]
			if !exists {
				return ValidationError{"UNASSIGNED_CANDIDATE", fmt.Sprintf("claim %q marked assigned but not found in any child", candidate.ClaimID)}
			}
			assignedCandidates[candidate.ClaimID] = true
		case CarriedForward:
			// Non-assigned candidates must not appear in any child
			if _, inChild := allChildClaims[candidate.ClaimID]; inChild {
				return ValidationError{"CONTRADICTORY_DISPOSITION", fmt.Sprintf("claim %q marked carried_forward but appears in a child", candidate.ClaimID)}
			}
			if candidate.Owner == nil || strings.TrimSpace(*candidate.Owner) == "" {
				return ValidationError{"MISSING_OWNER", fmt.Sprintf("claim %q marked carried_forward must have an owner", candidate.ClaimID)}
			}
		case Excluded:
			// Non-assigned candidates must not appear in any child
			if _, inChild := allChildClaims[candidate.ClaimID]; inChild {
				return ValidationError{"CONTRADICTORY_DISPOSITION", fmt.Sprintf("claim %q marked excluded but appears in a child", candidate.ClaimID)}
			}
			if candidate.Reason == nil || strings.TrimSpace(*candidate.Reason) == "" {
				return ValidationError{"MISSING_REASON", fmt.Sprintf("claim %q marked excluded must have a reason", candidate.ClaimID)}
			}
		default:
			return ValidationError{"INVALID_DISPOSITION", fmt.Sprintf("unknown disposition %q for claim %q", candidate.Disposition, candidate.ClaimID)}
		}
	}

	// Ensure every child claim has an assigned candidate (iterate children in order for determinism)
	for _, child := range children {
		claimIDs := childClaimIDs[child.Key]
		for _, claimID := range claimIDs {
			if !assignedCandidates[claimID] && !seenCandidates[claimID] {
				return ValidationError{"UNMATCHED_CHILD_CLAIM", fmt.Sprintf("child %q claim %q has no assigned candidate", child.Key, claimID)}
			}
		}
	}

	return nil
}

func countDistinct(items []string) int {
	seen := make(map[string]bool)
	for _, item := range items {
		seen[item] = true
	}
	return len(seen)
}
