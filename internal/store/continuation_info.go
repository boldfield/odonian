package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/boldfield/odonian/internal/manifest"
)

// Proposed-child statuses on ProposedChild.Status.
const (
	proposedStatusPending    = "pending"     // the parent has not reached done; nothing has been created
	proposedStatusCreated    = "created"     // the verified merge created this child
	proposedStatusNotCreated = "not_created" // the parent is finished (done/failed) and no child exists for it
)

// Dependency statuses on CreatedChild.DependencyStatus.
const (
	dependencyStatusNone      = "none"      // the child has no dependencies
	dependencyStatusSatisfied = "satisfied" // every dependency is done
	dependencyStatusBlocked   = "blocked"   // at least one dependency is not done; see BlockedBy
)

// Action item types on ActionItem.Type.
const (
	actionItemLegacyHeldFollowUp = "legacy_held_follow_up"
	actionItemHeldDependent      = "held_dependent"
)

// ContinuationDependency is one typed dependency a proposed child lists in its manifest.
type ContinuationDependency struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// ProposedChild is one child of the continuation manifest the parent submitted for its current
// review round, as the reviewers examined it.
type ProposedChild struct {
	Key           string                   `json:"key"`
	Title         string                   `json:"title"`
	Track         string                   `json:"track"`
	Model         string                   `json:"model"`
	InitialState  string                   `json:"initial_state"` // the state the child will be created in: ready for research, backlog otherwise
	Dependencies  []ContinuationDependency `json:"dependencies"`
	Status        string                   `json:"status"` // pending, created or not_created
	CreatedTaskID string                   `json:"created_task_id,omitempty"`
}

// CreatedChild is a task the parent's verified merge created from a continuation manifest.
type CreatedChild struct {
	ID                 string   `json:"id"`
	Key                string   `json:"key,omitempty"` // the manifest key it was created from
	Title              string   `json:"title"`
	ParentTaskID       string   `json:"parent_task_id"`
	ManifestDigest     string   `json:"manifest_digest"`
	State              string   `json:"state"` // the child's current state; ready or backlog when freshly created
	Track              string   `json:"track"`
	DependencyStatus   string   `json:"dependency_status"` // none, satisfied or blocked
	DependsOn          []string `json:"depends_on"`
	BlockedBy          []string `json:"blocked_by"` // the dependencies that are not yet done
	Claimable          bool     `json:"claimable"`  // ready, not held, and no blocking dependency
	ClaimIDs           []string `json:"claim_ids,omitempty"`
	SourceStartPoints  []string `json:"source_start_points,omitempty"`
	FileScope          []string `json:"file_scope,omitempty"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
}

// DeferredClaim is a pending candidate claim the manifest carried forward to a named owner
// instead of assigning it to a child.
type DeferredClaim struct {
	ClaimID string `json:"claim_id"`
	Owner   string `json:"owner"`
}

// ExcludedClaim is a pending candidate claim the manifest explicitly put out of scope. It has no
// owner: nothing is deferred.
type ExcludedClaim struct {
	ClaimID string `json:"claim_id"`
	Reason  string `json:"reason"`
}

// ActionItem is something an operator must do by hand because continuation children do not
// replace or retarget existing tasks automatically.
type ActionItem struct {
	Type        string `json:"type"`
	TaskID      string `json:"task_id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Description string `json:"description"`
}

// ParentInfo is the child-side provenance of a task created from a continuation manifest.
type ParentInfo struct {
	ID             string `json:"id"`
	ChildKey       string `json:"child_key,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
}

// ContinuationInfo is the planned-continuation view of a task: what its manifest proposes, which
// children the merge created, and what needs manual attention. It is deliberately separate from
// FindingFollowUps (tasks born from non-blocking review findings), which are not continuations.
// It carries no coverage claim: the pending-candidate lists are reported exactly as the manifest
// states them.
type ContinuationInfo struct {
	ManifestDigest   string          `json:"manifest_digest,omitempty"` // digest of the manifest for the current review round
	ProposedChildren []ProposedChild `json:"proposed_children,omitempty"`
	CreatedChildren  []CreatedChild  `json:"created_children,omitempty"`
	DeferredClaims   []DeferredClaim `json:"deferred_claims,omitempty"`
	ExcludedClaims   []ExcludedClaim `json:"excluded_claims,omitempty"`
	ActionItems      []ActionItem    `json:"action_items,omitempty"`
	ParentInfo       *ParentInfo     `json:"parent_info,omitempty"`
}

// FindingFollowUp is a non-blocking research review-finding follow-up task linked to this task
// by research_parent. It is not a planned continuation.
type FindingFollowUp struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
	Track string `json:"track"`
	Held  bool   `json:"held"`
}

// continuationDedupKey is the value of a continuation_child_dedup link, written by
// InsertManifestChildren.
type continuationDedupKey struct {
	ParentID       string `json:"parent_id"`
	ChildKey       string `json:"child_key"`
	ManifestDigest string `json:"manifest_digest"`
}

// continuationChildInitialState is the state a manifest child is created in: research children
// are immediately claimable, generated build and design children wait in backlog.
func continuationChildInitialState(track string) string {
	if track == "research" {
		return "ready"
	}
	return "backlog"
}

// loadContinuationView builds the continuation and finding-follow-up views of the task t.
// links are t's own links. Any query or decode failure is returned rather than yielding a
// partial operator view.
func loadContinuationView(ctx context.Context, q eventQuerier, t Task, links []TaskLink, manifests []SubmissionManifest) (*ContinuationInfo, []FindingFollowUp, error) {
	info := &ContinuationInfo{}

	for _, l := range links {
		if l.Kind != "continuation_parent" || l.TombstonedAt != nil {
			continue
		}
		info.ParentInfo = &ParentInfo{ID: l.Value}
		for _, d := range links {
			if d.Kind != "continuation_child_dedup" || d.TombstonedAt != nil {
				continue
			}
			var key continuationDedupKey
			if err := json.Unmarshal([]byte(d.Value), &key); err != nil {
				return nil, nil, fmt.Errorf("failed to decode continuation dedup link %s: %w", d.ID, err)
			}
			info.ParentInfo.ChildKey = key.ChildKey
			info.ParentInfo.ManifestDigest = key.ManifestDigest
			break
		}
		break
	}

	if t.Kind != "implement" {
		return nil, nil, nil
	}

	followUps, err := loadFindingFollowUps(ctx, q, t.ID)
	if err != nil {
		return nil, nil, err
	}

	var planned *manifest.Manifest
	for i := range manifests {
		if manifests[i].ReviewRound != t.ReviewRound {
			continue
		}
		planned = &manifest.Manifest{}
		if err := json.Unmarshal(manifests[i].ManifestJSON, planned); err != nil {
			return nil, nil, fmt.Errorf("failed to decode stored continuation manifest for round %d: %w", manifests[i].ReviewRound, err)
		}
		info.ManifestDigest = manifests[i].ManifestDigest
		break
	}

	created, err := loadCreatedChildren(ctx, q, t.ID)
	if err != nil {
		return nil, nil, err
	}

	if planned != nil {
		order := make(map[string]int, len(planned.Children))
		for i, c := range planned.Children {
			order[c.Key] = i
		}
		sort.SliceStable(created, func(i, j int) bool {
			oi, iok := order[created[i].Key]
			oj, jok := order[created[j].Key]
			if iok != jok {
				return iok
			}
			return iok && oi < oj
		})

		createdByKey := make(map[string]string, len(created))
		for _, c := range created {
			if c.ManifestDigest == info.ManifestDigest {
				createdByKey[c.Key] = c.ID
			}
		}
		for _, c := range planned.Children {
			p := ProposedChild{
				Key:          c.Key,
				Title:        c.Title,
				Track:        c.Track,
				Model:        c.Model,
				InitialState: continuationChildInitialState(c.Track),
				Dependencies: make([]ContinuationDependency, 0, len(c.Dependencies)),
			}
			for _, d := range c.Dependencies {
				p.Dependencies = append(p.Dependencies, ContinuationDependency{Kind: string(d.Kind), Ref: d.Ref})
			}
			if id, ok := createdByKey[c.Key]; ok {
				p.Status = proposedStatusCreated
				p.CreatedTaskID = id
			} else if t.State == "done" || t.State == "failed" {
				p.Status = proposedStatusNotCreated
			} else {
				p.Status = proposedStatusPending
			}
			info.ProposedChildren = append(info.ProposedChildren, p)
		}

		for _, pc := range planned.PendingCandidates {
			switch pc.Disposition {
			case manifest.CarriedForward:
				d := DeferredClaim{ClaimID: pc.ClaimID}
				if pc.Owner != nil {
					d.Owner = *pc.Owner
				}
				info.DeferredClaims = append(info.DeferredClaims, d)
			case manifest.Excluded:
				e := ExcludedClaim{ClaimID: pc.ClaimID}
				if pc.Reason != nil {
					e.Reason = *pc.Reason
				}
				info.ExcludedClaims = append(info.ExcludedClaims, e)
			}
		}
	}
	info.CreatedChildren = created

	if planned != nil || len(created) > 0 {
		items, err := loadContinuationActionItems(ctx, q, t.ID)
		if err != nil {
			return nil, nil, err
		}
		info.ActionItems = items
	}

	if info.ParentInfo == nil && planned == nil && len(created) == 0 {
		info = nil
	}
	return info, followUps, nil
}

// loadFindingFollowUps returns the research review-finding follow-up tasks of parentID.
func loadFindingFollowUps(ctx context.Context, q eventQuerier, parentID string) ([]FindingFollowUp, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT c.id, c.title, c.state, c.track, c.held
		FROM task_link l
		JOIN task c ON c.id = l.task_id
		WHERE l.kind = 'research_parent' AND l.value = ? AND l.tombstoned_at IS NULL
		ORDER BY c.created_at, c.id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to query finding follow-ups: %w", err)
	}
	defer rows.Close()
	var out []FindingFollowUp
	for rows.Next() {
		var f FindingFollowUp
		if err := rows.Scan(&f.ID, &f.Title, &f.State, &f.Track, &f.Held); err != nil {
			return nil, fmt.Errorf("failed to scan finding follow-up: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating finding follow-ups: %w", err)
	}
	return out, nil
}

// loadCreatedChildren returns the tasks created from parentID's continuation manifests, with
// their provenance links and dependency status, in creation order.
func loadCreatedChildren(ctx context.Context, q eventQuerier, parentID string) ([]CreatedChild, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT c.id, c.title, c.state, c.track, c.held
		FROM task_link lp
		JOIN task c ON c.id = lp.task_id
		WHERE lp.kind = 'continuation_parent' AND lp.value = ? AND lp.tombstoned_at IS NULL
		ORDER BY c.created_at, c.id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to query created children: %w", err)
	}
	var children []CreatedChild
	held := map[string]bool{}
	byID := map[string]int{}
	for rows.Next() {
		c := CreatedChild{ParentTaskID: parentID, DependsOn: []string{}, BlockedBy: []string{}}
		var isHeld bool
		if err := rows.Scan(&c.ID, &c.Title, &c.State, &c.Track, &isHeld); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan created child: %w", err)
		}
		held[c.ID] = isHeld
		byID[c.ID] = len(children)
		children = append(children, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("error iterating created children: %w", err)
	}
	rows.Close()
	if len(children) == 0 {
		return nil, nil
	}

	linkRows, err := q.QueryContext(ctx, `
		SELECT l.task_id, l.kind, l.value
		FROM task_link lp
		JOIN task_link l ON l.task_id = lp.task_id
		WHERE lp.kind = 'continuation_parent' AND lp.value = ? AND lp.tombstoned_at IS NULL
		  AND l.tombstoned_at IS NULL
		  AND l.kind IN ('continuation_child_dedup', 'continuation_child_claim_ids', 'continuation_child_source_start_points', 'continuation_child_file_scope', 'continuation_child_acceptance_criteria')
		ORDER BY l.task_id, l.kind, l.id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to query created child links: %w", err)
	}
	for linkRows.Next() {
		var taskID, kind, value string
		if err := linkRows.Scan(&taskID, &kind, &value); err != nil {
			linkRows.Close()
			return nil, fmt.Errorf("failed to scan created child link: %w", err)
		}
		c := &children[byID[taskID]]
		var decodeErr error
		switch kind {
		case "continuation_child_dedup":
			var key continuationDedupKey
			if decodeErr = json.Unmarshal([]byte(value), &key); decodeErr == nil {
				c.Key = key.ChildKey
				c.ManifestDigest = key.ManifestDigest
			}
		case "continuation_child_claim_ids":
			decodeErr = json.Unmarshal([]byte(value), &c.ClaimIDs)
		case "continuation_child_source_start_points":
			decodeErr = json.Unmarshal([]byte(value), &c.SourceStartPoints)
		case "continuation_child_file_scope":
			decodeErr = json.Unmarshal([]byte(value), &c.FileScope)
		case "continuation_child_acceptance_criteria":
			decodeErr = json.Unmarshal([]byte(value), &c.AcceptanceCriteria)
		}
		if decodeErr != nil {
			linkRows.Close()
			return nil, fmt.Errorf("failed to decode %s link of task %s: %w", kind, taskID, decodeErr)
		}
	}
	if err := linkRows.Err(); err != nil {
		linkRows.Close()
		return nil, fmt.Errorf("error iterating created child links: %w", err)
	}
	linkRows.Close()

	depRows, err := q.QueryContext(ctx, `
		SELECT d.task_id, d.depends_on_id, dt.state
		FROM task_link lp
		JOIN task_dep d ON d.task_id = lp.task_id
		JOIN task dt ON dt.id = d.depends_on_id
		WHERE lp.kind = 'continuation_parent' AND lp.value = ? AND lp.tombstoned_at IS NULL
		ORDER BY d.task_id, d.depends_on_id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to query created child dependencies: %w", err)
	}
	for depRows.Next() {
		var taskID, depID, depState string
		if err := depRows.Scan(&taskID, &depID, &depState); err != nil {
			depRows.Close()
			return nil, fmt.Errorf("failed to scan created child dependency: %w", err)
		}
		c := &children[byID[taskID]]
		c.DependsOn = append(c.DependsOn, depID)
		if depState != "done" {
			c.BlockedBy = append(c.BlockedBy, depID)
		}
	}
	if err := depRows.Err(); err != nil {
		depRows.Close()
		return nil, fmt.Errorf("error iterating created child dependencies: %w", err)
	}
	depRows.Close()

	for i := range children {
		c := &children[i]
		switch {
		case len(c.DependsOn) == 0:
			c.DependencyStatus = dependencyStatusNone
		case len(c.BlockedBy) == 0:
			c.DependencyStatus = dependencyStatusSatisfied
		default:
			c.DependencyStatus = dependencyStatusBlocked
		}
		c.Claimable = c.State == "ready" && !held[c.ID] && len(c.BlockedBy) == 0
	}
	return children, nil
}

// loadContinuationActionItems lists the legacy held tasks and dependents of a continuation
// parent that an operator must replace or retarget by hand; the server never repoints them.
func loadContinuationActionItems(ctx context.Context, q eventQuerier, parentID string) ([]ActionItem, error) {
	var items []ActionItem

	followRows, err := q.QueryContext(ctx, `
		SELECT c.id, c.title, c.state
		FROM task_link l
		JOIN task c ON c.id = l.task_id
		WHERE l.kind = 'research_parent' AND l.value = ? AND l.tombstoned_at IS NULL
		  AND (c.held = 1 OR c.state = 'backlog')
		  AND c.state NOT IN ('done', 'failed')
		  AND c.archived_at IS NULL AND c.superseded_by IS NULL
		ORDER BY c.created_at, c.id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to query held follow-ups: %w", err)
	}
	for followRows.Next() {
		var a ActionItem
		if err := followRows.Scan(&a.TaskID, &a.Title, &a.State); err != nil {
			followRows.Close()
			return nil, fmt.Errorf("failed to scan held follow-up: %w", err)
		}
		a.Type = actionItemLegacyHeldFollowUp
		a.Description = "Legacy review-finding follow-up is held and is not replaced or retargeted by the continuation children; replace, retarget or close it manually."
		items = append(items, a)
	}
	if err := followRows.Err(); err != nil {
		followRows.Close()
		return nil, fmt.Errorf("error iterating held follow-ups: %w", err)
	}
	followRows.Close()

	rows, err := q.QueryContext(ctx, `
		SELECT c.id, c.title, c.state
		FROM task_dep d
		JOIN task c ON c.id = d.task_id
		WHERE d.depends_on_id = ?
		  AND c.kind = 'implement' AND c.held = 1
		  AND c.state NOT IN ('done', 'failed')
		  AND c.archived_at IS NULL AND c.superseded_by IS NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM task_link cp
		    WHERE cp.task_id = c.id AND cp.kind = 'continuation_parent' AND cp.value = d.depends_on_id AND cp.tombstoned_at IS NULL
		  )
		ORDER BY c.created_at, c.id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to query held dependents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a ActionItem
		if err := rows.Scan(&a.TaskID, &a.Title, &a.State); err != nil {
			return nil, fmt.Errorf("failed to scan held dependent: %w", err)
		}
		a.Type = actionItemHeldDependent
		a.Description = "Held task depends on this continuation parent; its work may have moved into the continuation children, and its dependencies are not repointed automatically. Replace or retarget it manually."
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating held dependents: %w", err)
	}
	return items, nil
}
