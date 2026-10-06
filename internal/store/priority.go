package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Numeric queue priority P (docs/features/urgent-work-queue.md). Higher P runs first;
// ties break on created_at then id. Manual assignment is bounded to 1..MaxManualPriority;
// Move to front generates a value above that bound, and inherited or reloaded values
// above it are valid, so only external input passes through validateManualPriority.
const (
	DefaultPriority   int64 = 500
	MaxManualPriority int64 = 1000

	PriorityActionSet   = "set"
	PriorityActionFront = "front"
)

// validateManualPriority bounds a priority supplied from outside the store. It rejects
// rather than clamps; fractional and overflowing JSON numbers never reach it because
// they fail to decode into int64.
func validateManualPriority(priority int64) error {
	if priority < 1 || priority > MaxManualPriority {
		return invalid("INVALID_PRIORITY", fmt.Sprintf("priority must be an integer between 1 and %d", MaxManualPriority))
	}
	return nil
}

// SetPriorityRequest assigns a manual priority (1..1000) to the topic of TaskID. Resetting
// to the default is Priority = DefaultPriority.
type SetPriorityRequest struct {
	ActionKey string
	TaskID    string
	Priority  int64
	Actor     string
	Reason    string
}

// FrontPriorityRequest moves the topic of TaskID to the front of the queue. It carries no
// priority: the server alone computes the value.
type FrontPriorityRequest struct {
	ActionKey string
	TaskID    string
	Actor     string
	Reason    string
}

// PriorityChange is the recorded result of a priority action. A replay returns the
// original values, not the topic's current priority.
type PriorityChange struct {
	Action           string `json:"action"`
	TaskID           string `json:"task_id"`
	TopicAnchorID    string `json:"topic_anchor_id"`
	OldPriority      int64  `json:"old_priority"`
	Priority         int64  `json:"priority"`
	QueueMaxPriority *int64 `json:"queue_max_priority,omitempty"`
	Actor            string `json:"actor"`
	Reason           string `json:"reason"`
	Replayed         bool   `json:"replayed"`
}

// priorityEventNote is the structured payload of the "priority" audit event.
type priorityEventNote struct {
	Action           string `json:"action"`
	ActionKey        string `json:"action_key"`
	TopicAnchorID    string `json:"topic_anchor_id"`
	OldPriority      int64  `json:"old_priority"`
	NewPriority      int64  `json:"new_priority"`
	QueueMaxPriority *int64 `json:"queue_max_priority,omitempty"`
	Reason           string `json:"reason"`
	Actor            string `json:"actor"`
}

func isFinishedTaskState(state string) bool {
	switch state {
	case "done", "failed", "abandoned", "superseded":
		return true
	}
	return false
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type topicNode struct {
	state    string
	archived bool
	priority int64
}

// topicGraph holds the execution-lineage relations of every task. A topic is the
// implementation task at its root plus everything reachable downward: reviews,
// adjudications and merge work (target_task_id), superseding replacements
// (superseded_by) and approved continuation children (continuation_parent links).
// Document or project membership, dependencies and finding follow-ups are not lineage.
type topicGraph struct {
	nodes    map[string]topicNode
	parent   map[string]string
	children map[string][]string
}

// lineageParentSQL selects the single lineage parent of the task aliased c, or NULL for a
// topic root. Precedence: review/adjudication/merge work (target_task_id), a superseding
// replacement (superseded_by), then an approved continuation child (continuation_parent
// link). A relation whose parent row does not exist is skipped. The graph loader and the
// per-task anchor walk below both use this one expression, so they cannot disagree.
const lineageParentSQL = `COALESCE(
	(SELECT p.id FROM task p WHERE p.id = c.target_task_id AND p.id <> c.id),
	(SELECT o.id FROM task o WHERE o.superseded_by = c.id AND o.id <> c.id ORDER BY o.id LIMIT 1),
	(SELECT p.id FROM task_link l JOIN task p ON p.id = l.value
		WHERE l.task_id = c.id AND l.kind = 'continuation_parent' AND l.tombstoned_at IS NULL AND p.id <> c.id
		ORDER BY p.id LIMIT 1))`

// topicAnchorSQL resolves the topic anchor of the outer task row by walking lineage
// upward; it requires the outer query to read FROM task unaliased. The walk is bounded by
// lineage depth and ends on a cycle (UNION dedupes), where it yields no root and the
// caller falls back to the task itself.
const topicAnchorSQL = `COALESCE((
	WITH RECURSIVE lineage_up(id, parent_id) AS (
		SELECT c.id, ` + lineageParentSQL + ` FROM task c WHERE c.id = task.id
		UNION
		SELECT c.id, ` + lineageParentSQL + ` FROM task c JOIN lineage_up u ON c.id = u.parent_id
	)
	SELECT id FROM lineage_up WHERE parent_id IS NULL LIMIT 1
), task.id)`

// taskTopicColumns are the two Task columns that expose the topic: the effective priority
// (the anchor's priority, which Set and Front write across the topic and which descendants
// born later inherit by lineage) and the anchor id. Every Task read selects them in this
// order, after the row's own columns, so all views agree.
const taskTopicColumnPriority = `COALESCE((SELECT a.priority FROM task a WHERE a.id = ` + topicAnchorSQL + `), task.priority)`

const taskTopicColumns = taskTopicColumnPriority + ` AS topic_priority,
		` + topicAnchorSQL + ` AS topic_anchor_id`

// effectiveTaskPriority reads the priority a task's topic currently carries, so work
// spawned from it inherits the topic value whether or not the task's own row was ever
// rewritten. The value is generated or inherited, never operator input, so it is not
// range-checked.
func effectiveTaskPriority(ctx context.Context, tx *sql.Tx, taskID string) (int64, error) {
	var priority int64
	err := tx.QueryRowContext(ctx, `SELECT `+taskTopicColumnPriority+` FROM task WHERE id = ?`, taskID).Scan(&priority)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultPriority, nil
	}
	return priority, err
}

func loadTopicGraph(ctx context.Context, q queryer) (*topicGraph, error) {
	g := &topicGraph{
		nodes:    map[string]topicNode{},
		parent:   map[string]string{},
		children: map[string][]string{},
	}

	rows, err := q.QueryContext(ctx, `SELECT c.id, c.state, c.archived_at IS NOT NULL, c.priority, `+lineageParentSQL+` FROM task c`)
	if err != nil {
		return nil, fmt.Errorf("failed to load task lineage: %w", err)
	}
	for rows.Next() {
		var id string
		var node topicNode
		var parent *string
		if err := rows.Scan(&id, &node.state, &node.archived, &node.priority, &parent); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan task lineage: %w", err)
		}
		g.nodes[id] = node
		if parent != nil {
			g.parent[id] = *parent
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("failed to iterate task lineage: %w", err)
	}
	rows.Close()

	for child, parent := range g.parent {
		g.children[parent] = append(g.children[parent], child)
	}
	for parent := range g.children {
		sort.Strings(g.children[parent])
	}
	return g, nil
}

// root walks up to the topic anchor. A corrupt cycle has no root, so the task is its own
// anchor, matching topicAnchorSQL.
func (g *topicGraph) root(id string) string {
	start := id
	seen := map[string]bool{id: true}
	for {
		parent, ok := g.parent[id]
		if !ok {
			return id
		}
		if seen[parent] {
			return start
		}
		seen[parent] = true
		id = parent
	}
}

// members returns the anchor and every task below it, anchor first.
func (g *topicGraph) members(anchor string) []string {
	out := []string{anchor}
	seen := map[string]bool{anchor: true}
	for i := 0; i < len(out); i++ {
		for _, child := range g.children[out[i]] {
			if !seen[child] {
				seen[child] = true
				out = append(out, child)
			}
		}
	}
	return out
}

// outstandingTopics maps each outstanding topic anchor to its priority. A topic is
// outstanding when any non-archived task in its lineage is not finished, so a done
// anchor with an active continuation still counts, and a topic that is wholly finished
// or archived does not. The priority is the anchor's: Set and Front write it across the
// whole topic, and the anchor row is the one source that is never missing.
func (g *topicGraph) outstandingTopics() map[string]int64 {
	out := map[string]int64{}
	for id, node := range g.nodes {
		if node.archived || isFinishedTaskState(node.state) {
			continue
		}
		anchor := g.root(id)
		out[anchor] = g.nodes[anchor].priority
	}
	return out
}

func priorityRequestHash(action, taskID string, priority int64, actor, reason string) string {
	payload, _ := json.Marshal(struct {
		Action   string `json:"action"`
		TaskID   string `json:"task_id"`
		Priority int64  `json:"priority"`
		Actor    string `json:"actor"`
		Reason   string `json:"reason"`
	}{action, taskID, priority, actor, reason})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// SetTaskPriority assigns a manual priority to the whole topic of the selected task.
func (s *sqliteStore) SetTaskPriority(ctx context.Context, req SetPriorityRequest) (PriorityChange, error) {
	if err := validateManualPriority(req.Priority); err != nil {
		return PriorityChange{}, err
	}
	return s.applyPriorityAction(ctx, PriorityActionSet, req.ActionKey, req.TaskID, req.Priority, req.Actor, req.Reason)
}

// MoveTaskToFront gives the whole topic of the selected task the priority
// max(1000, max(P_queued)) + 1, where P_queued ranges over every outstanding topic on
// the server.
func (s *sqliteStore) MoveTaskToFront(ctx context.Context, req FrontPriorityRequest) (PriorityChange, error) {
	return s.applyPriorityAction(ctx, PriorityActionFront, req.ActionKey, req.TaskID, 0, req.Actor, req.Reason)
}

// applyPriorityAction runs one priority action as a single transaction: the idempotency
// lookup, the queue maximum, the topic-wide write, the action record and the audit event.
// The write pool holds one connection, so concurrent actions queue on BeginTx and each
// reads the maximum left by the one before it.
func (s *sqliteStore) applyPriorityAction(ctx context.Context, action, actionKey, taskID string, manualPriority int64, actor, reason string) (PriorityChange, error) {
	actionKey = strings.TrimSpace(actionKey)
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(sanitizeFreeText(reason))
	if actionKey == "" || len(actionKey) > 200 {
		return PriorityChange{}, invalid("INVALID_ACTION_KEY", "an idempotency key of 1-200 characters is required")
	}
	if actor == "" {
		return PriorityChange{}, invalid("ACTOR_REQUIRED", "an actor is required")
	}
	if reason == "" {
		return PriorityChange{}, invalid("REASON_REQUIRED", "an operator reason is required")
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return PriorityChange{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	requestHash := priorityRequestHash(action, taskID, manualPriority, actor, reason)
	var prior PriorityChange
	var priorHash string
	var priorQueueMax sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT request_hash, action, task_id, topic_anchor_id, old_priority, new_priority, queue_max_priority, actor, reason
		FROM priority_action WHERE action_key = ?
	`, actionKey).Scan(&priorHash, &prior.Action, &prior.TaskID, &prior.TopicAnchorID, &prior.OldPriority, &prior.Priority, &priorQueueMax, &prior.Actor, &prior.Reason)
	switch {
	case err == nil:
		if priorHash != requestHash {
			return PriorityChange{}, conflict("IDEMPOTENCY_MISMATCH", fmt.Sprintf("action key %q was already used for a different priority request", actionKey))
		}
		if priorQueueMax.Valid {
			value := priorQueueMax.Int64
			prior.QueueMaxPriority = &value
		}
		prior.Replayed = true
		return prior, nil
	case !errors.Is(err, sql.ErrNoRows):
		return PriorityChange{}, fmt.Errorf("failed to look up priority action: %w", err)
	}

	var selectedArchived bool
	err = tx.QueryRowContext(ctx, `SELECT archived_at IS NOT NULL FROM task WHERE id = ?`, taskID).Scan(&selectedArchived)
	if errors.Is(err, sql.ErrNoRows) {
		return PriorityChange{}, ErrNotFound
	}
	if err != nil {
		return PriorityChange{}, fmt.Errorf("failed to load task: %w", err)
	}
	if selectedArchived {
		return PriorityChange{}, conflict("ARCHIVED", "cannot change the priority of an archived task")
	}

	graph, err := loadTopicGraph(ctx, tx)
	if err != nil {
		return PriorityChange{}, err
	}
	anchorID := graph.root(taskID)
	oldPriority := graph.nodes[anchorID].priority

	newPriority := manualPriority
	var queueMax *int64
	if action == PriorityActionFront {
		outstanding := graph.outstandingTopics()
		if _, ok := outstanding[anchorID]; !ok {
			return PriorityChange{}, conflict("TOPIC_NOT_OUTSTANDING", "the topic has no outstanding task; a finished or archived topic cannot be moved to the front")
		}
		var highest int64
		for _, topicPriority := range outstanding {
			if topicPriority > highest {
				highest = topicPriority
			}
		}
		if highest == math.MaxInt64 {
			return PriorityChange{}, conflict("PRIORITY_OVERFLOW", "move to front would overflow the priority representation")
		}
		queueMax = &highest
		newPriority = max(MaxManualPriority, highest) + 1
	}

	for _, memberID := range graph.members(anchorID) {
		if _, err := tx.ExecContext(ctx, `UPDATE task SET priority = ? WHERE id = ?`, newPriority, memberID); err != nil {
			return PriorityChange{}, fmt.Errorf("failed to update topic priority: %w", err)
		}
	}

	now := nowTimestamp()
	var queueMaxArg any
	if queueMax != nil {
		queueMaxArg = *queueMax
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO priority_action (action_key, action, task_id, topic_anchor_id, request_hash, old_priority, new_priority, queue_max_priority, actor, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, actionKey, action, taskID, anchorID, requestHash, oldPriority, newPriority, queueMaxArg, actor, reason, now); err != nil {
		return PriorityChange{}, fmt.Errorf("failed to record priority action: %w", err)
	}

	note, err := json.Marshal(priorityEventNote{
		Action: action, ActionKey: actionKey, TopicAnchorID: anchorID,
		OldPriority: oldPriority, NewPriority: newPriority, QueueMaxPriority: queueMax,
		Reason: reason, Actor: actor,
	})
	if err != nil {
		return PriorityChange{}, fmt.Errorf("failed to encode priority event: %w", err)
	}
	noteText := string(note)
	if _, err := s.AppendEvent(ctx, tx, taskID, actor, "priority", nil, &noteText); err != nil {
		return PriorityChange{}, err
	}

	if err := tx.Commit(); err != nil {
		return PriorityChange{}, fmt.Errorf("failed to commit priority action: %w", err)
	}
	return PriorityChange{
		Action: action, TaskID: taskID, TopicAnchorID: anchorID,
		OldPriority: oldPriority, Priority: newPriority, QueueMaxPriority: queueMax,
		Actor: actor, Reason: reason,
	}, nil
}
