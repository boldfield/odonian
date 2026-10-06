package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/manifest"
	"github.com/boldfield/odonian/internal/policy"
)

//go:embed migrations
var migrationsFS embed.FS

// validTracks is the set of task tracks. Task creation and continuation-manifest
// validation both read it, so the two cannot disagree about which tracks exist.
var validTracks = map[string]bool{"build": true, "design": true, "research": true}

// validBranch is the shape of a task's optional shared branch name: exactly what
// localcommit.Slugify produces, so wi/<branch> is always a safe git ref.
var validBranch = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Store is the interface for database operations.
// Concrete implementations (sqliteStore) satisfy this interface.
type Store interface {
	ResearchPermitStore
	Close() error
	Conn() *sql.DB
	Now() time.Time
	AppendEvent(ctx context.Context, tx *sql.Tx, taskID, actor, kind string, verdict, note *string, findings ...json.RawMessage) (Event, error)
	ListEvents(ctx context.Context, taskID string) ([]Event, error)
	PruneEvents(ctx context.Context, terminalRetentionDays int) (int64, error)
	CreateProject(ctx context.Context, name, repo string) (Project, error)
	GetProject(ctx context.Context, id string) (Project, error)
	ListProjects(ctx context.Context, filter ProjectListFilter) ([]Project, error)
	CreateDocument(ctx context.Context, projectID, kind, title, ref string, commit *string) (Document, error)
	ListDocuments(ctx context.Context, projectID string, kind *string) ([]Document, error)
	CreateTasks(ctx context.Context, projectID string, tasks []TaskInput) ([]Task, error)
	ResolveTaskID(ctx context.Context, id string) (string, error)
	GetTask(ctx context.Context, id string) (TaskWithDepsAndLinks, error)
	ListTasks(ctx context.Context, projectID string, filter TaskListFilter) ([]Task, error)
	ListDependents(ctx context.Context, taskID string) ([]string, error)
	UpdateTaskDependsOn(ctx context.Context, taskID string, depIDs []string) (Task, error)
	ClaimTask(ctx context.Context, taskID, agentID, model string, leaseTTL time.Duration) (Task, error)
	ClaimResearchTask(ctx context.Context, req ResearchClaim) (ResearchClaimResult, error)
	SetResearchPolicy(ctx context.Context, now time.Time, cfg policy.Config) error
	GetResearchPolicyMode(ctx context.Context) (policy.Mode, error)
	GetResearchAdmissionDiagnostic(ctx context.Context, taskID string) (ResearchAdmissionDiagnostic, error)
	HeartbeatTask(ctx context.Context, taskID, agentID string, leaseTTL time.Duration) (Task, error)
	PromoteTask(ctx context.Context, taskID string) (Task, error)
	SubmitTask(ctx context.Context, taskID, agentID, result string, verdict *string, links []LinkInput, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int, findings ...json.RawMessage) (TaskWithDepsAndLinks, error)
	SubmitTaskWithDisputes(ctx context.Context, taskID, agentID, result string, verdict *string, links []LinkInput, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int, findings json.RawMessage, disputes json.RawMessage) (TaskWithDepsAndLinks, error)
	SubmitTaskWithManifest(ctx context.Context, taskID, agentID, result string, verdict *string, links []LinkInput, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int, findings json.RawMessage, disputes json.RawMessage, manifest json.RawMessage) (TaskWithDepsAndLinks, error)
	AddReview(ctx context.Context, taskID, actor, verdict string, note *string) (Event, error)
	TransitionTask(ctx context.Context, taskID, to string, note *string) (Task, error)
	SupersedeTask(ctx context.Context, taskID string, modelOverride *string) (Task, error)
	UpdateTaskEscalate(ctx context.Context, taskID string, escalate bool) (Task, error)
	SetTaskPriority(ctx context.Context, req SetPriorityRequest) (PriorityChange, error)
	MoveTaskToFront(ctx context.Context, req FrontPriorityRequest) (PriorityChange, error)
	HoldTask(ctx context.Context, taskID string) (Task, error)
	BeginLanding(ctx context.Context, taskID string, reviewRound int, commit, attempt string) error
	CancelLanding(ctx context.Context, taskID, attempt string) error
	CompleteLanding(ctx context.Context, taskID, attempt string, note *string) (Task, error)
	ReleaseTask(ctx context.Context, taskID string, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int) (Task, error)
	ArchiveTask(ctx context.Context, taskID string) (Task, error)
	UnarchiveTask(ctx context.Context, taskID string) (Task, error)
	ArchiveProject(ctx context.Context, projectID string) (Project, error)
	UnarchiveProject(ctx context.Context, projectID string) (Project, error)
	GetResearchReviewerScorecards(ctx context.Context, projectID string) (ReviewerScorecards, error)
	TombstoneLink(ctx context.Context, taskID, linkID string) error
	CreateEvaluationCampaign(ctx context.Context, campaign EvaluationCampaign) (EvaluationCampaign, error)
	CreateEvaluationCandidate(ctx context.Context, candidate EvaluationCandidate) (EvaluationCandidate, error)
	CreateEvaluationSample(ctx context.Context, sample EvaluationSample) (EvaluationSample, error)
	ConfigureEvaluationPool(ctx context.Context, cfg EvaluationPoolConfig) (EvaluationPoolState, error)
	GetEvaluationPool(ctx context.Context, id string) (EvaluationPoolState, error)
	ClaimEvaluationJob(ctx context.Context, req EvaluationJobClaim) (EvaluationJobClaimResult, error)
	RenewEvaluationAttempt(ctx context.Context, attemptID string, expiresAt time.Time) error
	FinalizeEvaluationAttempt(ctx context.Context, res EvaluationAttemptResult) error
	ListEvaluationFindings(ctx context.Context, attemptID string) ([]evaluation.Finding, error)
	GetEvaluationAttemptDetail(ctx context.Context, attemptID string) (*EvaluationAttemptDetail, error)
	ExpireEvaluationAttempts(ctx context.Context, now time.Time) (int, error)
	GetEvaluationJob(ctx context.Context, jobID string) (EvaluationJob, error)
	GetEvaluationAttempt(ctx context.Context, attemptID string) (EvaluationAttempt, error)
	GetEvaluationSample(ctx context.Context, sampleID string) (EvaluationSample, error)
	GetEvaluationCandidate(ctx context.Context, candidateID string) (EvaluationCandidate, error)
	GetEvaluationCampaign(ctx context.Context, campaignID string) (EvaluationCampaign, error)
	PauseEvaluationCampaign(ctx context.Context, campaignID string) error
	ListEvaluationCandidates(ctx context.Context, campaignID string) ([]EvaluationCandidate, error)
	GetEvaluationCampaignStatus(ctx context.Context, campaignID string) (EvaluationCampaignStatus, error)
	FirstRoundCensus(ctx context.Context, projectIDs []string) (evaluation.FirstRoundCensus, error)
	GetFirstRoundRecord(ctx context.Context, taskID string) (evaluation.FirstRoundRecord, error)
	ListEvaluationSamples(ctx context.Context, campaignID string) ([]EvaluationSample, error)
	RecordEvaluationStaging(ctx context.Context, staging EvaluationStaging) (EvaluationStaging, error)
	GetEvaluationStaging(ctx context.Context, campaignID, sampleID, candidateID string) (EvaluationStaging, error)
	ListEvaluationStagings(ctx context.Context, campaignID string) ([]EvaluationStaging, error)
	RecordEvaluationDisposition(ctx context.Context, in EvaluationDispositionInput) (evaluation.Disposition, error)
	ListEvaluationDispositions(ctx context.Context, campaignID string) ([]evaluation.Disposition, error)
	GetEvaluationReport(ctx context.Context, campaignID string) (evaluation.Report, error)
}

// sqliteStore wraps a SQLite database connection and provides migration functionality.
type sqliteStore struct {
	conn                     *sql.DB
	readConn                 *sql.DB
	allowedModels            []string
	allowedModelsM           map[string]bool
	escalationLadder         []string
	researchDefaultModel     string
	researchEscalationLadder []string
	researchAdjudicator      string

	// clock is the server time source for claim, heartbeat and research
	// admission; nil means time.Now. Tests inject a fake clock.
	clock func() time.Time
	// research holds the research admission policy (see SetResearchPolicy).
	research researchPolicyState

	// supersedeCloseHook, when set, is invoked after each background
	// closeSupersededPR attempt finishes. It exists solely so tests can
	// deterministically wait for the async close instead of racing it; it is
	// never set in production.
	supersedeCloseHook func()
}

// StoreOption is a functional option for configuring a Store.
type StoreOption func(*sqliteStore)

// WithClock sets the server time source used by task claim and heartbeat and by
// research admission. The default is time.Now.
func WithClock(now func() time.Time) StoreOption {
	return func(s *sqliteStore) {
		s.clock = now
	}
}

// WithEscalationLadder sets the escalation ladder for the store.
// The ladder defines the order of models for escalation.
// If not provided, the ladder defaults to allowedModels.
func WithEscalationLadder(ladder []string) StoreOption {
	return func(s *sqliteStore) {
		s.escalationLadder = append([]string{}, ladder...) // Copy to avoid external mutation
	}
}

// WithResearchDefaultModel sets the default model for research tasks.
// If not provided, research tasks without an explicit model use getDefaultModel.
func WithResearchDefaultModel(model string) StoreOption {
	return func(s *sqliteStore) {
		s.researchDefaultModel = model
	}
}

// WithResearchEscalationLadder sets the escalation ladder for research tasks.
// An empty ladder means no model escalation for research tasks.
func WithResearchEscalationLadder(ladder []string) StoreOption {
	return func(s *sqliteStore) {
		s.researchEscalationLadder = append([]string{}, ladder...) // Copy to avoid external mutation
	}
}

// WithResearchAdjudicator sets ODONIAN_RESEARCH_ADJUDICATOR, the model spawnAdjudicationTask
// assigns to adjudicate a worker-disputed research finding that its raising reviewer
// maintained (docs/features/research-track.md section 5). An empty value (the
// default) means adjudication never runs: a maintained dispute stays blocking and the
// server records why on the parent task.
func WithResearchAdjudicator(model string) StoreOption {
	return func(s *sqliteStore) {
		s.researchAdjudicator = model
	}
}

// Open opens a database connection and applies all pending migrations.
// The dbPath should be a file path (e.g., "odonian.db") or "file::memory:?cache=shared"
// for an in-memory database.
// It configures WAL mode, foreign keys, and busy timeout via DSN pragmas.
// allowedModels is the list of valid model identifiers for task creation.
// opts are optional configuration functions (e.g., WithEscalationLadder).
func Open(dbPath string, allowedModels []string, opts ...StoreOption) (Store, error) {
	// Build the DSN with pragmas for WAL, foreign_keys, and busy_timeout.
	// This ensures every connection from the pool has these pragmas applied.
	dsn := buildDSN(dbPath)

	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set MaxOpenConns to 1 for single-writer SQLite (as per DESIGN.md §7)
	conn.SetMaxOpenConns(1)

	// Build a map of allowed models for O(1) lookup
	allowedModelsM := make(map[string]bool)
	for _, m := range allowedModels {
		allowedModelsM[m] = true
	}

	store := &sqliteStore{
		conn:             conn,
		allowedModels:    allowedModels,
		allowedModelsM:   allowedModelsM,
		escalationLadder: append([]string{}, allowedModels...), // Default to allowedModels
	}

	// Apply functional options
	for _, opt := range opts {
		opt(store)
	}

	// If no escalation ladder was set (or if it's empty after options), default to allowedModels
	if len(store.escalationLadder) == 0 {
		store.escalationLadder = append([]string{}, allowedModels...)
	}

	// Apply migrations in a transaction
	if err := store.migrate(migrationsFS); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	// Open a second connection for reads with SetMaxOpenConns(4)
	readConn, err := sql.Open("sqlite", dsn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to open read connection: %w", err)
	}
	readConn.SetMaxOpenConns(4)
	store.readConn = readConn

	return store, nil
}

// buildDSN constructs a SQLite DSN with pragmas.
func buildDSN(dbPath string) string {
	// If the path is already a DSN (contains scheme), use it directly but append pragmas.
	if strings.Contains(dbPath, "://") || strings.Contains(dbPath, ":memory:") {
		dsn := dbPath
		// For in-memory databases, ensure cache=shared so multiple connections share the same database
		if strings.Contains(dsn, ":memory:") && !strings.Contains(dsn, "cache=shared") {
			dsn = "file:" + dsn + "?cache=shared"
		}
		return dsn_addPragmas(dsn)
	}

	// Otherwise, treat it as a file path.
	// Escape the path and add pragmas.
	escaped := url.QueryEscape(dbPath)
	dsn := "file:" + escaped
	return dsn_addPragmas(dsn)
}

// dsn_addPragmas appends pragma query parameters to a DSN.
func dsn_addPragmas(dsn string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
}

// migrate applies all pending migrations from the given filesystem.
// It disables foreign key enforcement before the migration transaction begins (since PRAGMA inside
// a transaction is a no-op) and restores it afterward. Before commit, it runs an integrity check
// to ensure no foreign key violations occurred, failing the migration if any are found.
func (s *sqliteStore) migrate(fsys fs.FS) error {
	// Disable foreign keys on the connection before starting the transaction
	// SQLite requires PRAGMA foreign_keys to be set outside a transaction
	if _, err := s.conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("failed to disable foreign keys before migration: %w", err)
	}
	defer func() {
		if _, err := s.conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
			panic(fmt.Sprintf("failed to restore foreign keys after migration: %v", err))
		}
	}()

	tx, err := s.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Create schema_migrations table if it doesn't exist
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("failed to create schema_migrations table: %w", err)
	}

	// Get list of all migration files
	migrations, err := listMigrations(fsys)
	if err != nil {
		return err
	}

	// Apply each migration that hasn't been applied yet
	for _, migration := range migrations {
		var applied int
		err := tx.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", migration.version).Scan(&applied)
		if err != nil {
			return fmt.Errorf("failed to query schema_migrations: %w", err)
		}

		if applied > 0 {
			// Migration already applied, skip it
			continue
		}

		// Read the migration file
		data, err := fs.ReadFile(fsys, migration.path)
		if err != nil {
			return fmt.Errorf("failed to read migration file %s: %w", migration.path, err)
		}

		// Execute the migration (split by semicolon to handle multiple statements)
		statements := splitStatements(string(data))
		for _, stmt := range statements {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("failed to execute migration %s: %w", migration.version, err)
			}
		}

		// Record that the migration was applied
		now := nowTimestamp()
		if _, err := tx.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			migration.version, now); err != nil {
			return fmt.Errorf("failed to record migration %s: %w", migration.version, err)
		}
	}

	// Before commit, run a foreign key integrity check
	// This catches any migrations that would leave dangling foreign key references.
	// PRAGMA foreign_key_check returns 4 columns (table, rowid, parent, fkid) per violation.
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("failed to run foreign key integrity check: %w", err)
	}
	defer rows.Close()

	var violationCount int
	for rows.Next() {
		violationCount++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read foreign key check results: %w", err)
	}
	if violationCount > 0 {
		return fmt.Errorf("migration integrity check failed: found %d foreign key violations", violationCount)
	}

	return tx.Commit()
}

type migration struct {
	version string
	path    string
}

// listMigrations returns all migration files sorted by version from the given filesystem.
func listMigrations(fsys fs.FS) ([]migration, error) {
	var migrations []migration

	// Read all files in the migrations directory
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".sql" {
			// Extract version from filename (e.g., "0001_init.sql" -> "0001")
			version := entry.Name()[:len(entry.Name())-4] // Remove .sql extension
			migrations = append(migrations, migration{
				version: version,
				path:    filepath.Join("migrations", entry.Name()),
			})
		}
	}

	// Sort migrations by version
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	return migrations, nil
}

// Close closes the database connection.
func (s *sqliteStore) Close() error {
	var errs []error
	if s.readConn != nil {
		if err := s.readConn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.conn.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// Conn returns the underlying database connection for direct access.
func (s *sqliteStore) Conn() *sql.DB {
	return s.conn
}

// Now returns the current time using the store's clock (which may be mocked in tests).
func (s *sqliteStore) Now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// AppendEvent inserts a new event into the event table within an existing transaction.
// It must be called within a transaction so that a state change and its event can be
// committed atomically.
func (s *sqliteStore) AppendEvent(ctx context.Context, tx *sql.Tx, taskID, actor, kind string, verdict, note *string, findings ...json.RawMessage) (Event, error) {
	return s.appendEvent(ctx, tx, taskID, actor, kind, verdict, note, nil, findings...)
}

// appendEvent is the shared implementation behind AppendEvent. sourceTaskID optionally
// records which review task produced this event (set only for the review event a
// review task appends on its parent), so research aggregation can identify the
// current round's own submissions among all review events on the parent.
func (s *sqliteStore) appendEvent(ctx context.Context, tx *sql.Tx, taskID, actor, kind string, verdict, note, sourceTaskID *string, findings ...json.RawMessage) (Event, error) {
	eventID := GenerateID()
	now := nowTimestamp()

	var findingsText *string
	var findingsRaw *json.RawMessage
	if len(findings) > 0 && findings[0] != nil {
		text := string(findings[0])
		findingsText = &text
		findingsRaw = &findings[0]
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO event (id, task_id, actor, kind, verdict, note, findings, source_task_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, eventID, taskID, actor, kind, verdict, note, findingsText, sourceTaskID, now)
	if err != nil {
		return Event{}, fmt.Errorf("failed to append event: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Event{}, fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected != 1 {
		return Event{}, fmt.Errorf("expected 1 row affected, got %d", rowsAffected)
	}

	return Event{
		ID:           eventID,
		TaskID:       taskID,
		Actor:        actor,
		Kind:         kind,
		Verdict:      verdict,
		Note:         note,
		Findings:     findingsRaw,
		SourceTaskID: sourceTaskID,
		CreatedAt:    now,
	}, nil
}

// setTaskDepends replaces a task's outgoing dependencies within a transaction.
// It deletes all existing dependencies for the task and inserts new ones.
func (s *sqliteStore) setTaskDepends(ctx context.Context, tx *sql.Tx, taskID string, depIDs []string) error {
	// Delete existing dependencies
	_, err := tx.ExecContext(ctx, `
		DELETE FROM task_dep WHERE task_id = ?
	`, taskID)
	if err != nil {
		return fmt.Errorf("failed to delete existing dependencies: %w", err)
	}

	// Insert new dependencies
	for _, depID := range depIDs {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO task_dep (task_id, depends_on_id)
			VALUES (?, ?)
		`, taskID, depID)
		if err != nil {
			return fmt.Errorf("failed to insert dependency: %w", err)
		}
	}

	return nil
}

// ListEvents retrieves all events for a given task, ordered by created_at and id.
func (s *sqliteStore) ListEvents(ctx context.Context, taskID string) ([]Event, error) {
	return listEvents(ctx, s.readConn, taskID)
}

// eventQuerier is the subset of *sql.DB and *sql.Tx that listEvents needs.
type eventQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// listEvents is ListEvents against an explicit querier, so a caller already holding a
// read transaction can list events inside it instead of taking a second pooled
// connection from readConn.
func listEvents(ctx context.Context, q eventQuerier, taskID string) ([]Event, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, task_id, actor, kind, verdict, note, findings, source_task_id, disputes, created_at
		FROM event
		WHERE task_id = ?
		ORDER BY created_at, id
	`, taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to query events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var findingsText sql.NullString
		var sourceTaskID sql.NullString
		var disputesText sql.NullString
		err := rows.Scan(&e.ID, &e.TaskID, &e.Actor, &e.Kind, &e.Verdict, &e.Note, &findingsText, &sourceTaskID, &disputesText, &e.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}
		if findingsText.Valid {
			raw := json.RawMessage(findingsText.String)
			e.Findings = &raw
		}
		if sourceTaskID.Valid {
			e.SourceTaskID = &sourceTaskID.String
		}
		if disputesText.Valid {
			raw := json.RawMessage(disputesText.String)
			e.Disputes = &raw
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating events: %w", err)
	}

	return events, nil
}

// splitStatements splits SQL statements by semicolon, handling comments.
func splitStatements(sql string) []string {
	var statements []string
	var current strings.Builder
	lines := strings.Split(sql, "\n")

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Skip empty lines and comments
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")

		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSpace(current.String())
			if stmt != "" {
				// Remove trailing semicolon
				stmt = strings.TrimSuffix(stmt, ";")
				statements = append(statements, stmt)
			}
			current.Reset()
		}
	}

	// Add any remaining statement
	if current.Len() > 0 {
		stmt := strings.TrimSpace(current.String())
		if stmt != "" {
			stmt = strings.TrimSuffix(stmt, ";")
			statements = append(statements, stmt)
		}
	}

	return statements
}

// GenerateID generates a new unique ID using UUID v4.
// This is the reusable ID pattern that all tasks (T06+) will use.
func GenerateID() string {
	return uuid.NewString()
}

// timestampLayout is a fixed-width, nanosecond-precision RFC3339 layout. Unlike
// time.RFC3339Nano (which drops trailing-zero fractions and so yields variable-width
// strings that do not sort lexically), every timestamp here is the same width, so a
// lexical ORDER BY on the stored TEXT equals chronological order. All persisted
// timestamps use this single format to keep ordering consistent across tables — the
// event log (DESIGN §2/§5) depends on it for chronological audit ordering.
const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// nowTimestamp returns the current UTC time formatted with timestampLayout.
func nowTimestamp() string {
	return time.Now().UTC().Format(timestampLayout)
}

// leaseExpiryTimestamp returns the time at the given future time (now + ttl) formatted with timestampLayout.
// This ensures the lease expires_at timestamp is in the same fixed-width format so string comparison
// in claimableSQL works correctly.
func leaseExpiryTimestamp(ttl time.Duration) string {
	return time.Now().UTC().Add(ttl).Format(timestampLayout)
}

// Domain structs mirroring the schema (DESIGN.md §2)

// Project represents a code project.
type Project struct {
	ID         string  `db:"id" json:"id"`
	Name       string  `db:"name" json:"name"`
	Repo       string  `db:"repo" json:"repo"`
	CreatedAt  string  `db:"created_at" json:"created_at"`
	ArchivedAt *string `db:"archived_at" json:"archived_at"` // nullable
}

// Document represents a design or feature spec document.
type Document struct {
	ID        string  `db:"id" json:"id"`
	ProjectID string  `db:"project_id" json:"project_id"`
	Kind      string  `db:"kind" json:"kind"` // 'design' or 'feature_spec'
	Title     string  `db:"title" json:"title"`
	Ref       string  `db:"ref" json:"ref"`
	Commit    *string `db:"commit" json:"commit"` // nullable
	CreatedAt string  `db:"created_at" json:"created_at"`
	UpdatedAt string  `db:"updated_at" json:"updated_at"`
}

// Task represents a task on the board.
type Task struct {
	ID             string   `db:"id" json:"id"`
	ProjectID      string   `db:"project_id" json:"project_id"`
	DocumentID     string   `db:"document_id" json:"document_id"`
	Title          string   `db:"title" json:"title"`
	Spec           string   `db:"spec" json:"spec"`
	State          string   `db:"state" json:"state"`
	Assignee       *string  `db:"assignee" json:"assignee"`                 // nullable
	LeaseExpiresAt *string  `db:"lease_expires_at" json:"lease_expires_at"` // nullable
	Result         *string  `db:"result" json:"result"`                     // nullable
	Model          string   `db:"model" json:"model"`
	Kind           string   `db:"kind" json:"kind"`
	ReviewModels   []string `db:"review_models" json:"review_models"`
	ReviewRound    int      `db:"review_round" json:"review_round"`
	TargetTaskID   *string  `db:"target_task_id" json:"target_task_id"` // nullable
	Verdict        *string  `db:"verdict" json:"verdict"`               // nullable
	AgentMerge     bool     `db:"agent_merge" json:"agent_merge"`
	Held           bool     `db:"held" json:"held"`
	Escalate       bool     `db:"escalate" json:"escalate"`
	Track          string   `db:"track" json:"track"`
	Branch         string   `db:"branch" json:"branch"`     // local_commit target: shares wi/<branch>; empty = per-task wi/<slug of title>
	Priority       int64    `db:"priority" json:"priority"` // effective: the topic anchor's priority
	TopicAnchorID  string   `db:"topic_anchor_id" json:"topic_anchor_id"`
	CreatedAt      string   `db:"created_at" json:"created_at"`
	UpdatedAt      string   `db:"updated_at" json:"updated_at"`
	ArchivedAt     *string  `db:"archived_at" json:"archived_at"`     // nullable
	SupersededBy   *string  `db:"superseded_by" json:"superseded_by"` // nullable
}

// TaskLink represents a link from a task to external resources (PR, branch, commit, CI).
type TaskLink struct {
	ID           string  `db:"id" json:"id"`
	TaskID       string  `db:"task_id" json:"task_id"`
	Kind         string  `db:"kind" json:"kind"` // 'pr', 'branch', 'commit', or 'ci'
	Value        string  `db:"value" json:"value"`
	TombstonedAt *string `db:"tombstoned_at" json:"tombstoned_at"` // nullable, set when the reconciler has established there is nothing left to do for the link (PR is gone, merged, closed, or closed by the reconciler) and the link must not be polled again
	ReviewRound  *int    `db:"review_round" json:"review_round"`   // the review round an implement submission added it in; nil for older links and other task kinds
}

// SubmissionManifest is the continuation manifest a research implement task submitted in one
// review round (docs/features/research-continuations.md). Rows are written once, in the
// submitting transaction, and never updated: a rework round adds a new row and earlier
// rounds' reviewed evidence stays as it was.
type SubmissionManifest struct {
	ReviewRound    int             `json:"review_round"`    // the review round this submission started
	ParentTaskID   string          `json:"parent_task_id"`  // the parent whose children the manifest proposes
	ManifestJSON   json.RawMessage `json:"manifest_json"`   // canonical JSON, exactly what was digested
	ManifestDigest string          `json:"manifest_digest"` // hex SHA-256 of ManifestJSON
	SubmittedAt    string          `json:"submitted_at"`
}

// TaskInput is the input format for bulk task creation.
type TaskInput struct {
	Key          string   `json:"key"` // optional client-provided key for intra-batch deps
	Title        string   `json:"title"`
	Spec         string   `json:"spec"`
	DocumentID   string   `json:"document_id"`
	DependsOn    []string `json:"depends_on"` // refs to task ids or keys in the batch
	Model        string   `json:"model"`
	ReviewModels []string `json:"review_models"`
	AgentMerge   bool     `json:"agent_merge"`
	Escalate     *bool    `json:"escalate"` // nullable, defaults to true if not provided
	Track        string   `json:"track"`    // optional, defaults to 'build' if not provided
	Branch       string   `json:"branch"`   // optional local_commit branch shared across tasks (wi/<branch>)
	Priority     *int64   `json:"priority"` // optional, defaults to 500; an explicit value must be in 1..1000
}

// LinkInput is the input format for task links during submission.
type LinkInput struct {
	Kind  string `json:"kind"` // 'pr', 'branch', 'commit', 'ci', or 'no_op'
	Value string `json:"value"`
}

// TaskWithDepsAndLinks combines a Task with its dependencies and links.
type TaskWithDepsAndLinks struct {
	ID             string   `json:"id"`
	ProjectID      string   `json:"project_id"`
	DocumentID     string   `json:"document_id"`
	Title          string   `json:"title"`
	Spec           string   `json:"spec"`
	State          string   `json:"state"`
	Assignee       *string  `json:"assignee"`
	LeaseExpiresAt *string  `json:"lease_expires_at"`
	Result         *string  `json:"result"`
	Model          string   `json:"model"`
	Kind           string   `json:"kind"`
	ReviewModels   []string `json:"review_models"`
	ReviewRound    int      `json:"review_round"`
	TargetTaskID   *string  `json:"target_task_id"`
	Verdict        *string  `json:"verdict"`
	AgentMerge     bool     `json:"agent_merge"`
	Held           bool     `json:"held"`
	Escalate       bool     `json:"escalate"`
	Track          string   `json:"track"`
	Branch         string   `json:"branch"`
	Priority       int64    `json:"priority"` // effective: the topic anchor's priority
	TopicAnchorID  string   `json:"topic_anchor_id"`
	LandingRound   *int     `json:"landing_round"`   // set while an approve lands this round's work (see BeginLanding)
	LandingCommit  *string  `json:"landing_commit"`  // the reviewed commit that approve is landing
	LandingAttempt *string  `json:"landing_attempt"` // the approve attempt that owns the reservation
	// CurrentRoundLinks is the submission under review: the active links of the submission that
	// started the current review round (see submissionLinks). Clients use it rather than
	// re-deriving the rule from Links.
	CurrentRoundLinks []TaskLink `json:"current_round_links"`
	CreatedAt         string     `json:"created_at"`
	UpdatedAt         string     `json:"updated_at"`
	ArchivedAt        *string    `json:"archived_at"`
	SupersededBy      *string    `json:"superseded_by"`
	DependsOn         []string   `json:"depends_on"`
	Links             []TaskLink `json:"links"`
	// SubmissionManifests lists the continuation manifest of each review round that carried one,
	// oldest round first; empty for tasks that never submitted a manifest.
	SubmissionManifests []SubmissionManifest `json:"submission_manifests"`
	// Continuation is the planned/created continuation view (docs/features/research-continuations.md);
	// FindingFollowUps lists tasks born from non-blocking review findings, kept separate because
	// they are not continuations. Both are absent when there is nothing to show.
	Continuation     *ContinuationInfo `json:"continuation,omitempty"`
	FindingFollowUps []FindingFollowUp `json:"finding_follow_ups,omitempty"`
}

// TaskListFilter contains filters for listing tasks.
type TaskListFilter struct {
	State             *string
	Assignee          *string
	Model             *string
	Kind              *string
	Claimable         bool
	IncludeArchived   bool
	IncludeSuperseded bool
}

// ProjectListFilter contains filters for listing projects.
type ProjectListFilter struct {
	Model             *string
	Kind              *string
	Claimable         bool
	IncludeArchived   bool
	IncludeSuperseded bool
}

// Event represents an audit/event log entry.
type Event struct {
	ID           string           `db:"id" json:"id"`
	TaskID       string           `db:"task_id" json:"task_id"`
	Actor        string           `db:"actor" json:"actor"`
	Kind         string           `db:"kind" json:"kind"`
	Verdict      *string          `db:"verdict" json:"verdict"`               // nullable
	Note         *string          `db:"note" json:"note"`                     // nullable
	Findings     *json.RawMessage `db:"findings" json:"findings"`             // nullable; structured review findings
	SourceTaskID *string          `db:"source_task_id" json:"source_task_id"` // nullable; the review task that produced this event, if any
	Disputes     *json.RawMessage `db:"disputes" json:"disputes"`             // nullable; worker disputes of review findings, set on a research rework's submit event
	CreatedAt    string           `db:"created_at" json:"created_at"`
}

// Finding is a single structured review finding, as defined by the research track
// spec (docs/features/research-track.md, section 3). Findings are optional and are
// only accepted on review-kind task submissions.
type Finding struct {
	ID            string  `json:"id"`
	Severity      string  `json:"severity"`
	File          string  `json:"file"`
	Line          int     `json:"line"`
	Summary       string  `json:"summary"`
	InChangedText bool    `json:"in_changed_text"`
	Status        string  `json:"status"`
	PriorID       *string `json:"prior_id,omitempty"`
}

// validateFindings parses and validates a raw JSON findings payload against the
// section 3 format. Each finding is validated completely, field by field, before
// validation moves to the next finding, so a rejection names the first invalid
// field in submission order.
func validateFindings(raw json.RawMessage) ([]Finding, error) {
	var rawFindings []json.RawMessage
	if err := json.Unmarshal(raw, &rawFindings); err != nil {
		return nil, invalid("INVALID_FINDINGS", "findings: must be an array")
	}

	findings := make([]Finding, 0, len(rawFindings))
	seenIDs := make(map[string]bool, len(rawFindings))

	for i, rf := range rawFindings {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rf, &m); err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d]: must be an object", i))
		}

		var f Finding

		idRaw, ok := m["id"]
		if !ok {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].id: must be non-empty", i))
		}
		var id string
		if err := json.Unmarshal(idRaw, &id); err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].id: must be a string", i))
		}
		if id == "" {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].id: must be non-empty", i))
		}
		if seenIDs[id] {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].id: duplicate id %q", i, id))
		}
		seenIDs[id] = true
		f.ID = id

		sevRaw, ok := m["severity"]
		if !ok {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].severity: must be one of P1, P2, P3", i))
		}
		var severity string
		if err := json.Unmarshal(sevRaw, &severity); err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].severity: must be a string", i))
		}
		if severity != "P1" && severity != "P2" && severity != "P3" {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].severity: must be one of P1, P2, P3", i))
		}
		f.Severity = severity

		fileRaw, ok := m["file"]
		if !ok {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].file: must be non-empty", i))
		}
		var file string
		if err := json.Unmarshal(fileRaw, &file); err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].file: must be a string", i))
		}
		if file == "" {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].file: must be non-empty", i))
		}
		f.File = file

		lineRaw, ok := m["line"]
		if !ok || len(lineRaw) == 0 || lineRaw[0] == '"' {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].line: must be an integer", i))
		}
		var lineNum json.Number
		if err := json.Unmarshal(lineRaw, &lineNum); err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].line: must be an integer", i))
		}
		lineVal, err := lineNum.Int64()
		if err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].line: must be an integer", i))
		}
		if lineVal <= 0 || lineVal > math.MaxInt32 {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].line: must be a positive integer", i))
		}
		f.Line = int(lineVal)

		sumRaw, ok := m["summary"]
		if !ok {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].summary: must be non-empty", i))
		}
		var summary string
		if err := json.Unmarshal(sumRaw, &summary); err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].summary: must be a string", i))
		}
		if summary == "" {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].summary: must be non-empty", i))
		}
		f.Summary = summary

		ictRaw, ok := m["in_changed_text"]
		if !ok || string(ictRaw) == "null" {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].in_changed_text: must be a boolean", i))
		}
		var inChangedText bool
		if err := json.Unmarshal(ictRaw, &inChangedText); err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].in_changed_text: must be a boolean", i))
		}
		f.InChangedText = inChangedText

		statusRaw, ok := m["status"]
		if !ok {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].status: must be one of new, still_open, resolved", i))
		}
		var status string
		if err := json.Unmarshal(statusRaw, &status); err != nil {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].status: must be a string", i))
		}
		if status != "new" && status != "still_open" && status != "resolved" {
			return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].status: must be one of new, still_open, resolved", i))
		}
		f.Status = status

		priorRaw, priorPresent := m["prior_id"]
		if status == "new" {
			if priorPresent {
				return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].prior_id: must be absent when status is new", i))
			}
		} else {
			if !priorPresent {
				return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].prior_id: required when status is %s", i, status))
			}
			var priorID string
			if err := json.Unmarshal(priorRaw, &priorID); err != nil {
				return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].prior_id: must be a string", i))
			}
			if priorID == "" {
				return nil, invalid("INVALID_FINDINGS", fmt.Sprintf("findings[%d].prior_id: must be non-empty", i))
			}
			f.PriorID = &priorID
		}

		findings = append(findings, f)
	}

	return findings, nil
}

// Dispute is a worker's dispute of one prior-round review finding, submitted with
// cited source evidence during research-track rework, per
// docs/features/research-track.md section 5. A dispute never alters the finding it
// names: the reviewer who raised it re-evaluates it, in its next round, against the
// evidence.
//
// Round and Lineage are resolved and filled in server-side (by submitTask, not the
// client) once FindingID has been matched to exactly one reviewer's finding in the
// round being reworked. They are what let a later submission tell whether it is
// re-disputing the same finding lineage: FindingID alone is not enough, since
// reviewers choose their own ids and commonly reuse them across rounds (see
// priorDisputes and its caller in submitTask).
type Dispute struct {
	FindingID string `json:"finding_id"`
	Evidence  string `json:"evidence"`
	Round     int    `json:"round,omitempty"`
	Lineage   string `json:"lineage,omitempty"`
}

// validateDisputes parses and validates a raw JSON disputes payload: an array of
// objects, each naming a finding_id and citing non-empty evidence, with no
// finding_id repeated within one submission. It does not check that a finding_id was
// actually raised by a reviewer, resolve which reviewer raised it, or reject a
// finding already disputed in an earlier round — that requires the task's review
// history (to resolve each finding_id's reviewer lineage and prior_id chain) and is
// done by the caller.
func validateDisputes(raw json.RawMessage) ([]Dispute, error) {
	var rawDisputes []json.RawMessage
	if err := json.Unmarshal(raw, &rawDisputes); err != nil {
		return nil, invalid("INVALID_DISPUTES", "disputes: must be an array")
	}

	disputes := make([]Dispute, 0, len(rawDisputes))
	seenIDs := make(map[string]bool, len(rawDisputes))

	for i, rd := range rawDisputes {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rd, &m); err != nil {
			return nil, invalid("INVALID_DISPUTES", fmt.Sprintf("disputes[%d]: must be an object", i))
		}

		var d Dispute

		idRaw, ok := m["finding_id"]
		if !ok {
			return nil, invalid("INVALID_DISPUTES", fmt.Sprintf("disputes[%d].finding_id: must be non-empty", i))
		}
		var id string
		if err := json.Unmarshal(idRaw, &id); err != nil {
			return nil, invalid("INVALID_DISPUTES", fmt.Sprintf("disputes[%d].finding_id: must be a string", i))
		}
		if id == "" {
			return nil, invalid("INVALID_DISPUTES", fmt.Sprintf("disputes[%d].finding_id: must be non-empty", i))
		}
		if seenIDs[id] {
			return nil, invalid("INVALID_DISPUTES", fmt.Sprintf("disputes[%d].finding_id: duplicate finding_id %q in this submission", i, id))
		}
		seenIDs[id] = true
		d.FindingID = id

		evRaw, ok := m["evidence"]
		if !ok {
			return nil, invalid("INVALID_DISPUTES", fmt.Sprintf("disputes[%d].evidence: must be non-empty", i))
		}
		var evidence string
		if err := json.Unmarshal(evRaw, &evidence); err != nil {
			return nil, invalid("INVALID_DISPUTES", fmt.Sprintf("disputes[%d].evidence: must be a string", i))
		}
		if strings.TrimSpace(evidence) == "" {
			return nil, invalid("INVALID_DISPUTES", fmt.Sprintf("disputes[%d].evidence: must be non-empty", i))
		}
		d.Evidence = evidence

		disputes = append(disputes, d)
	}

	return disputes, nil
}

// disputeContextEntry is a disputed finding paired with the worker's evidence,
// grouped by the raising reviewer's lineage so submitTask can hand each reviewer's
// next-round review task exactly the disputes it needs to re-evaluate.
type disputeContextEntry struct {
	Finding  Finding
	Evidence string
}

// priorDisputes collects every dispute recorded on an earlier submit event of this
// implement task, across every prior rework round, each carrying the Round and
// Lineage it was resolved against at the time it was stored. The caller uses these to
// locate each prior dispute's target finding within the current round's collected
// findings and chain them, so a finding already disputed cannot be disputed again
// under a later id in the same reviewer's prior_id chain (docs/features/
// research-track.md section 5: the raising reviewer's next re-evaluation of a dispute
// is what decides it, once). Comparing by bare FindingID would be both too strict (two
// reviewers commonly reuse the same id) and too loose (a reviewer's carried-forward
// finding gets a new id each round), so Round and Lineage — resolved once, at dispute
// time, never re-derived from the bare id alone — are what keep this scoped to one
// reviewer's one finding lineage.
func (s *sqliteStore) priorDisputes(ctx context.Context, tx *sql.Tx, taskID string) ([]Dispute, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT disputes FROM event WHERE task_id = ? AND kind = 'submit' AND disputes IS NOT NULL
	`, taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to query prior disputes: %w", err)
	}
	defer rows.Close()

	var all []Dispute
	for rows.Next() {
		var disputesText string
		if err := rows.Scan(&disputesText); err != nil {
			return nil, fmt.Errorf("failed to scan prior disputes: %w", err)
		}
		var priorDisputes []Dispute
		if err := json.Unmarshal([]byte(disputesText), &priorDisputes); err != nil {
			continue
		}
		all = append(all, priorDisputes...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate prior disputes: %w", err)
	}
	return all, nil
}

// ErrNotFound is returned when a resource is not found.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a constraint is violated (e.g., second design per project).
var ErrConflict = errors.New("conflict")

// ConflictError is a typed conflict with an error code and message.
// Handlers map it to HTTP 409 via errors.As. Candidates is optional structured
// data for conflicts that need to surface more than a message, e.g. AMBIGUOUS_ID.
type ConflictError struct {
	Code       string
	Message    string
	Candidates []string
}

func (e *ConflictError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func conflict(code, message string) error {
	return &ConflictError{Code: code, Message: message}
}

func ambiguousTaskID(candidates []string) error {
	return &ConflictError{
		Code:       "AMBIGUOUS_ID",
		Message:    fmt.Sprintf("task id prefix matches multiple tasks: %s", strings.Join(candidates, ", ")),
		Candidates: candidates,
	}
}

// ValidationError is a client-input error. Handlers map it to HTTP 400 via errors.As,
// surfacing Code and Message. Use invalid() to construct one. This is the validation
// convention for all mutation endpoints — prefer it over bare fmt.Errorf so the
// 400-vs-500 distinction never depends on string-matching error messages.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func invalid(code, message string) error {
	return &ValidationError{Code: code, Message: message}
}

// sanitizeFreeText strips C0 control characters (U+0000–U+001F) from a free-text
// field so they can never enter the store, EXCEPT the three whitespace controls
// that legitimately appear in human text — tab (\t), newline (\n), and carriage
// return (\r) — which are preserved. Newlines are deliberately kept, not removed:
// the constraint is to ESCAPE them on the way out (encoding/json does this for us),
// not to lose them. This is the write-side defense; the API response is valid JSON
// regardless of stored content because every handler serializes via encoding/json,
// which escapes any control char. DEL (0x7f) and other code points are left intact —
// they are valid in JSON strings and only U+0000–U+001F must be escaped.
func sanitizeFreeText(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return r
		}
		if r < 0x20 {
			return -1 // drop the rune
		}
		return r
	}, text)
}

// getDefaultModel returns the default model from the allowlist.
// Prefers 'haiku' if available (backward compatibility), otherwise returns the first allowlisted model.
func (s *sqliteStore) getDefaultModel() string {
	if s.allowedModelsM["haiku"] {
		return "haiku"
	}
	if len(s.allowedModels) > 0 {
		return s.allowedModels[0]
	}
	return "haiku"
}

// CreateProject creates a new project with the given name and repo.
// It generates the id and sets created_at automatically.
func (s *sqliteStore) CreateProject(ctx context.Context, name, repo string) (Project, error) {
	id := GenerateID()
	createdAt := nowTimestamp()

	_, err := s.conn.ExecContext(ctx, `
		INSERT INTO project (id, name, repo, created_at)
		VALUES (?, ?, ?, ?)
	`, id, name, repo, createdAt)
	if err != nil {
		return Project{}, fmt.Errorf("failed to create project: %w", err)
	}

	return Project{
		ID:        id,
		Name:      name,
		Repo:      repo,
		CreatedAt: createdAt,
	}, nil
}

// GetProject retrieves a project by id.
// Returns ErrNotFound if the project does not exist.
func (s *sqliteStore) GetProject(ctx context.Context, id string) (Project, error) {
	var p Project
	err := s.readConn.QueryRowContext(ctx, `
		SELECT id, name, repo, created_at, archived_at FROM project WHERE id = ?
	`, id).Scan(&p.ID, &p.Name, &p.Repo, &p.CreatedAt, &p.ArchivedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("failed to get project: %w", err)
	}

	return p, nil
}

// ListProjects lists projects with optional filters, ordered by created_at.
// If filter.Claimable is true, returns only projects with at least one claimable task
// matching the optional model and kind filters.
// By default, archived projects are excluded unless filter.IncludeArchived is true.
// Returns an empty slice (not nil) when no projects exist.
func (s *sqliteStore) ListProjects(ctx context.Context, filter ProjectListFilter) ([]Project, error) {
	query := `
		SELECT id, name, repo, created_at, archived_at FROM project
	`
	args := []interface{}{}
	whereAdded := false

	if filter.Claimable {
		supersededCondition := ""
		if !filter.IncludeSuperseded {
			supersededCondition = ` AND state != 'superseded'`
		}
		query += ` WHERE EXISTS (
			SELECT 1 FROM task
			WHERE task.project_id = project.id
			AND archived_at IS NULL` + supersededCondition + `
			AND ` + claimableSQL
		args = append(args, nowTimestamp())
		whereAdded = true

		if filter.Model != nil {
			query += ` AND model = ?`
			args = append(args, *filter.Model)
		}

		if filter.Kind != nil {
			query += ` AND kind = ?`
			args = append(args, *filter.Kind)
		}

		query += `
		)`
	}

	if !filter.IncludeArchived {
		if whereAdded {
			query += ` AND project.archived_at IS NULL`
		} else {
			query += ` WHERE archived_at IS NULL`
			whereAdded = true
		}
	}

	query += ` ORDER BY created_at`

	rows, err := s.readConn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query projects: %w", err)
	}
	defer rows.Close()

	projects := make([]Project, 0)
	for rows.Next() {
		var p Project
		err := rows.Scan(&p.ID, &p.Name, &p.Repo, &p.CreatedAt, &p.ArchivedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan project: %w", err)
		}
		projects = append(projects, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating projects: %w", err)
	}

	return projects, nil
}

// CreateDocument creates a new document (design or feature_spec) for a project.
// Verifies the project exists, and if kind is 'design', ensures at most one design per project.
// Returns ErrNotFound if the project does not exist.
// Returns ErrConflict if attempting to create a second design for the same project.
func (s *sqliteStore) CreateDocument(ctx context.Context, projectID, kind, title, ref string, commit *string) (Document, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Verify project exists
	var projectExists bool
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM project WHERE id = ?", projectID).Scan(&projectExists)
	if err != nil {
		return Document{}, fmt.Errorf("failed to check project: %w", err)
	}
	if !projectExists {
		return Document{}, ErrNotFound
	}

	// If kind is 'design', check that no design already exists for this project
	if kind == "design" {
		var designCount int
		err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM document WHERE project_id = ? AND kind = 'design'", projectID).Scan(&designCount)
		if err != nil {
			return Document{}, fmt.Errorf("failed to check existing designs: %w", err)
		}
		if designCount > 0 {
			return Document{}, ErrConflict
		}
	}

	// Create the document
	id := GenerateID()
	now := nowTimestamp()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO document (id, project_id, kind, title, ref, "commit", created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, projectID, kind, title, ref, commit, now, now)
	if err != nil {
		return Document{}, fmt.Errorf("failed to create document: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return Document{
		ID:        id,
		ProjectID: projectID,
		Kind:      kind,
		Title:     title,
		Ref:       ref,
		Commit:    commit,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// ListDocuments retrieves all documents for a project, optionally filtered by kind.
// Returns an empty slice (not nil) if no documents exist.
func (s *sqliteStore) ListDocuments(ctx context.Context, projectID string, kind *string) ([]Document, error) {
	query := `
		SELECT id, project_id, kind, title, ref, "commit", created_at, updated_at
		FROM document
		WHERE project_id = ?
	`
	args := []interface{}{projectID}

	if kind != nil {
		query += ` AND kind = ?`
		args = append(args, *kind)
	}

	query += ` ORDER BY created_at, id`

	rows, err := s.readConn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query documents: %w", err)
	}
	defer rows.Close()

	docs := make([]Document, 0)
	for rows.Next() {
		var d Document
		err := rows.Scan(&d.ID, &d.ProjectID, &d.Kind, &d.Title, &d.Ref, &d.Commit, &d.CreatedAt, &d.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan document: %w", err)
		}
		docs = append(docs, d)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating documents: %w", err)
	}

	return docs, nil
}

// CreateTasks bulk-creates tasks in a single transaction.
// - All tasks must have non-empty title, spec, and document_id.
// - Validates that document_id references a document in the given project.
// - Resolves depends_on refs as either batch keys or existing task ids in the project.
// - Returns 400-level error for validation failures; the tx rolls back and nothing is created.
func (s *sqliteStore) CreateTasks(ctx context.Context, projectID string, tasks []TaskInput) ([]Task, error) {
	if len(tasks) == 0 {
		return []Task{}, nil
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Validate all tasks and resolve dependencies
	keyToID := make(map[string]string)
	createdTasks := make([]Task, 0, len(tasks))
	now := nowTimestamp()

	for _, input := range tasks {
		// Strip raw control characters from free-text before validation/storage so
		// bad bytes can't enter the store; legitimate newlines/tabs are preserved.
		input.Title = sanitizeFreeText(input.Title)
		input.Spec = sanitizeFreeText(input.Spec)

		// Validate title and spec are non-empty
		if strings.TrimSpace(input.Title) == "" {
			return nil, invalid("EMPTY_TITLE", "title is required")
		}
		if strings.TrimSpace(input.Spec) == "" {
			return nil, invalid("EMPTY_SPEC", "spec is required")
		}
		if strings.TrimSpace(input.DocumentID) == "" {
			return nil, invalid("MISSING_DOCUMENT_ID", "document_id is required")
		}

		// Verify document_id references a document in this project
		var docProjectID string
		err := tx.QueryRowContext(ctx, "SELECT project_id FROM document WHERE id = ?", input.DocumentID).Scan(&docProjectID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, invalid("INVALID_DOCUMENT_ID", "document_id does not exist")
		}
		if err != nil {
			return nil, fmt.Errorf("failed to verify document: %w", err)
		}
		if docProjectID != projectID {
			return nil, invalid("DOCUMENT_NOT_IN_PROJECT", "document_id is not in this project")
		}

		// Generate task id
		taskID := GenerateID()
		keyToID[input.Key] = taskID

		model := input.Model
		if model == "" {
			if input.Track == "research" && s.researchDefaultModel != "" {
				model = s.researchDefaultModel
			} else {
				model = s.getDefaultModel()
			}
		}

		// Validate model against allowlist
		if !s.allowedModelsM[model] {
			return nil, invalid("UNKNOWN_MODEL", fmt.Sprintf("unknown model: %s", model))
		}

		// Validate each review_models entry against allowlist
		for _, reviewModel := range input.ReviewModels {
			if !s.allowedModelsM[reviewModel] {
				return nil, invalid("UNKNOWN_MODEL", fmt.Sprintf("unknown review model: %s", reviewModel))
			}
		}

		var reviewModelsJSON *string
		if len(input.ReviewModels) > 0 {
			data, err := json.Marshal(input.ReviewModels)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal review_models: %w", err)
			}
			str := string(data)
			reviewModelsJSON = &str
		}

		escalate := true
		if input.Escalate != nil {
			escalate = *input.Escalate
		}

		track := "build"
		if input.Track != "" {
			if !validTracks[input.Track] {
				return nil, invalid("UNKNOWN_TRACK", fmt.Sprintf("unknown track: %s", input.Track))
			}
			track = input.Track
		}

		if input.Branch != "" && !validBranch.MatchString(input.Branch) {
			return nil, invalid("INVALID_BRANCH", fmt.Sprintf("branch %q must be lowercase letters, digits, and single dashes (e.g. event-platform)", input.Branch))
		}

		priority := DefaultPriority
		if input.Priority != nil {
			if err := validateManualPriority(*input.Priority); err != nil {
				return nil, err
			}
			priority = *input.Priority
		}

		task := Task{
			ID:           taskID,
			ProjectID:    projectID,
			DocumentID:   input.DocumentID,
			Title:        input.Title,
			Spec:         input.Spec,
			State:        "backlog",
			Model:        model,
			Kind:         "implement",
			ReviewModels: input.ReviewModels,
			ReviewRound:  0,
			TargetTaskID: nil,
			AgentMerge:   input.AgentMerge,
			Escalate:     escalate,
			Track:        track,
			Branch:       input.Branch,
			Priority:     priority,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		// Normalize ReviewModels to empty slice when nil
		if task.ReviewModels == nil {
			task.ReviewModels = []string{}
		}

		// Insert task
		_, err = tx.ExecContext(ctx, `
			INSERT INTO task (id, project_id, document_id, title, spec, state, model, kind, review_models, review_round, target_task_id, agent_merge, escalate, track, branch, priority, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, task.ID, task.ProjectID, task.DocumentID, task.Title, task.Spec, task.State, task.Model, task.Kind, reviewModelsJSON, task.ReviewRound, task.TargetTaskID, task.AgentMerge, task.Escalate, task.Track, task.Branch, task.Priority, task.CreatedAt, task.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to insert task: %w", err)
		}

		createdTasks = append(createdTasks, task)
	}

	// Now insert task_dep edges, resolving references
	for i, input := range tasks {
		if len(input.DependsOn) == 0 {
			continue
		}

		taskID := createdTasks[i].ID

		for _, ref := range input.DependsOn {
			// Check for self-dependency
			if ref == input.Key && input.Key != "" {
				return nil, invalid("SELF_DEPENDENCY", "a task cannot depend on itself")
			}

			var dependsOnID string

			// Try to resolve as a batch key first
			if keyID, exists := keyToID[ref]; exists {
				dependsOnID = keyID
			} else {
				// Try to resolve as an existing task id in this project
				var existingProjectID string
				err := tx.QueryRowContext(ctx, "SELECT project_id FROM task WHERE id = ?", ref).Scan(&existingProjectID)
				if errors.Is(err, sql.ErrNoRows) {
					return nil, invalid("UNKNOWN_DEPENDENCY", "depends_on references an unknown task")
				}
				if err != nil {
					return nil, fmt.Errorf("failed to verify dependency: %w", err)
				}
				if existingProjectID != projectID {
					return nil, invalid("DEPENDENCY_NOT_IN_PROJECT", "depends_on references a task in another project")
				}
				dependsOnID = ref
			}

			// Insert edge
			_, err = tx.ExecContext(ctx, `
				INSERT INTO task_dep (task_id, depends_on_id)
				VALUES (?, ?)
			`, taskID, dependsOnID)
			if err != nil {
				return nil, fmt.Errorf("failed to insert task_dep: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return createdTasks, nil
}

// likeEscape escapes the SQL LIKE metacharacters ('%', '_') and the escape
// character itself so a caller-supplied string matches literally under a
// `LIKE ? ESCAPE '\'` clause. Without this, a prefix such as "____" or "%"
// would be treated as a wildcard pattern and match unrelated task ids.
func likeEscape(s string) string {
	return likeEscaper.Replace(s)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ResolveTaskID resolves a possibly-truncated task id to the full stored id.
// Stored ids are 36-char UUIDs, so an id of that length or longer is treated
// as exact and returned unchanged (existing exact-match callers see no
// behavior change). A shorter id is resolved as a prefix: exactly one match
// returns that id, no match returns ErrNotFound, and several matches return a
// *ConflictError with Code AMBIGUOUS_ID and the candidate ids attached. An id
// shorter than 8 characters is too short to safely disambiguate and is
// rejected as not-found without a database lookup. The prefix is matched
// literally — LIKE wildcards in the input are escaped, so "________" resolves
// to not-found rather than matching every task.
func (s *sqliteStore) ResolveTaskID(ctx context.Context, id string) (string, error) {
	if len(id) >= 36 {
		return id, nil
	}
	if len(id) < 8 {
		return "", ErrNotFound
	}

	rows, err := s.readConn.QueryContext(ctx, `SELECT id FROM task WHERE id LIKE ? ESCAPE '\' ORDER BY id`, likeEscape(id)+"%")
	if err != nil {
		return "", fmt.Errorf("failed to resolve task id prefix: %w", err)
	}
	defer rows.Close()

	var candidates []string
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			return "", fmt.Errorf("failed to scan candidate task id: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("error iterating candidate task ids: %w", err)
	}

	switch len(candidates) {
	case 0:
		return "", ErrNotFound
	case 1:
		return candidates[0], nil
	default:
		return "", ambiguousTaskID(candidates)
	}
}

// GetTask retrieves a task by id, including its dependencies and links.
// The id may be a unique prefix (at least 8 characters) of the full id; see
// ResolveTaskID. Returns ErrNotFound if the task does not exist.
func (s *sqliteStore) GetTask(ctx context.Context, id string) (TaskWithDepsAndLinks, error) {
	id, err := s.ResolveTaskID(ctx, id)
	if err != nil {
		return TaskWithDepsAndLinks{}, err
	}

	var t Task
	var reviewModelsJSON *string
	var landingRound *int
	var landingCommit *string
	var landingAttempt *string
	err = s.readConn.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by, landing_round, landing_commit, landing_attempt
		FROM task WHERE id = ?
	`, id).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy, &landingRound, &landingCommit, &landingAttempt)

	if errors.Is(err, sql.ErrNoRows) {
		return TaskWithDepsAndLinks{}, ErrNotFound
	}
	if err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("failed to get task: %w", err)
	}

	// Unmarshal review_models from JSON
	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	// Fetch dependencies
	depRows, err := s.readConn.QueryContext(ctx, `
		SELECT depends_on_id FROM task_dep WHERE task_id = ? ORDER BY depends_on_id
	`, id)
	if err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("failed to query dependencies: %w", err)
	}
	defer depRows.Close()

	dependsOn := make([]string, 0)
	for depRows.Next() {
		var depID string
		if err := depRows.Scan(&depID); err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to scan dependency: %w", err)
		}
		dependsOn = append(dependsOn, depID)
	}
	if err := depRows.Err(); err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("error iterating dependencies: %w", err)
	}

	// Fetch links
	linkRows, err := s.readConn.QueryContext(ctx, `
		SELECT id, task_id, kind, value, tombstoned_at, review_round FROM task_link WHERE task_id = ? ORDER BY id
	`, id)
	if err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("failed to query links: %w", err)
	}
	defer linkRows.Close()

	links := make([]TaskLink, 0)
	for linkRows.Next() {
		var link TaskLink
		if err := linkRows.Scan(&link.ID, &link.TaskID, &link.Kind, &link.Value, &link.TombstonedAt, &link.ReviewRound); err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to scan link: %w", err)
		}
		links = append(links, link)
	}
	if err := linkRows.Err(); err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("error iterating links: %w", err)
	}
	currentRoundLinks, err := submissionLinks(ctx, s.readConn, id, t.ReviewRound)
	if err != nil {
		return TaskWithDepsAndLinks{}, err
	}
	if currentRoundLinks == nil {
		currentRoundLinks = []TaskLink{}
	}

	manifests, err := listSubmissionManifests(ctx, s.readConn, id)
	if err != nil {
		return TaskWithDepsAndLinks{}, err
	}

	continuation, followUps, err := loadContinuationView(ctx, s.readConn, t, links, manifests)
	if err != nil {
		return TaskWithDepsAndLinks{}, err
	}

	return TaskWithDepsAndLinks{
		ID:                  t.ID,
		ProjectID:           t.ProjectID,
		DocumentID:          t.DocumentID,
		Title:               t.Title,
		Spec:                t.Spec,
		State:               t.State,
		Assignee:            t.Assignee,
		LeaseExpiresAt:      t.LeaseExpiresAt,
		Result:              t.Result,
		Model:               t.Model,
		Kind:                t.Kind,
		ReviewModels:        t.ReviewModels,
		ReviewRound:         t.ReviewRound,
		TargetTaskID:        t.TargetTaskID,
		Verdict:             t.Verdict,
		AgentMerge:          t.AgentMerge,
		Held:                t.Held,
		Escalate:            t.Escalate,
		Track:               t.Track,
		Branch:              t.Branch,
		Priority:            t.Priority,
		TopicAnchorID:       t.TopicAnchorID,
		LandingRound:        landingRound,
		LandingCommit:       landingCommit,
		LandingAttempt:      landingAttempt,
		CurrentRoundLinks:   currentRoundLinks,
		CreatedAt:           t.CreatedAt,
		UpdatedAt:           t.UpdatedAt,
		ArchivedAt:          t.ArchivedAt,
		SupersededBy:        t.SupersededBy,
		DependsOn:           dependsOn,
		Links:               links,
		SubmissionManifests: manifests,
		Continuation:        continuation,
		FindingFollowUps:    followUps,
	}, nil
}

// claimableSQL is the SQL predicate for the claimable condition, reused by both ListTasks and ClaimTask.
// A task is claimable iff:
//   - state = 'ready', OR state = 'in_progress' with an EXPIRED lease — a stalled or dead
//     worker's task becomes reclaimable once its lease lapses (the standard lease pattern;
//     the new claimer resumes the existing mr/<id8> branch, so no work is lost)
//   - all dependencies are done
//   - not held (held = 0)
//
// IMPORTANT: reused in ListTasks and ClaimTask (in the UPDATE statement) and contains exactly
// ONE '?' (the lease cutoff). If you change the '?' count, update all callers' arg lists.
const claimableSQL = `(state = 'ready' OR (state = 'in_progress' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?))
	AND held = 0
	AND NOT EXISTS (
		SELECT 1 FROM task_dep d
		JOIN task t2 ON t2.id = d.depends_on_id
		WHERE d.task_id = task.id AND t2.state != 'done'
	)`

// ListTasks retrieves tasks for a project with optional filters.
// Filters compose with AND logic.
// By default, archived tasks are excluded unless filter.IncludeArchived is true.
func (s *sqliteStore) ListTasks(ctx context.Context, projectID string, filter TaskListFilter) ([]Task, error) {
	query := `SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, ` + taskTopicColumns + `, created_at, updated_at, archived_at, superseded_by
		FROM task
		WHERE project_id = ?`
	args := []interface{}{projectID}

	if !filter.IncludeArchived {
		query += ` AND archived_at IS NULL`
	}

	if !filter.IncludeSuperseded {
		query += ` AND state != 'superseded'`
	}

	if filter.State != nil {
		query += ` AND state = ?`
		args = append(args, *filter.State)
	}

	if filter.Assignee != nil {
		query += ` AND assignee = ?`
		args = append(args, *filter.Assignee)
	}

	if filter.Model != nil {
		query += ` AND model = ?`
		args = append(args, *filter.Model)
	}

	if filter.Kind != nil {
		query += ` AND kind = ?`
		args = append(args, *filter.Kind)
	}

	if filter.Claimable {
		query += ` AND ` + claimableSQL
		args = append(args, nowTimestamp())
	}

	query += ` ORDER BY topic_priority DESC, created_at, id`

	rows, err := s.readConn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]Task, 0)
	for rows.Next() {
		var t Task
		var reviewModelsJSON *string
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy); err != nil {
			return nil, fmt.Errorf("failed to scan task: %w", err)
		}
		// Unmarshal review_models from JSON
		t.ReviewModels = []string{}
		if reviewModelsJSON != nil {
			if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
				return nil, fmt.Errorf("failed to unmarshal review_models: %w", err)
			}
		}
		tasks = append(tasks, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating tasks: %w", err)
	}

	return tasks, nil
}

// ListDependents returns all task IDs that depend on the given taskID (reverse dependencies).
// Returns an empty slice if the task has no dependents.
func (s *sqliteStore) ListDependents(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.readConn.QueryContext(ctx, `
		SELECT task_id FROM task_dep WHERE depends_on_id = ? ORDER BY task_id
	`, taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to query dependents: %w", err)
	}
	defer rows.Close()

	dependents := make([]string, 0)
	for rows.Next() {
		var depID string
		if err := rows.Scan(&depID); err != nil {
			return nil, fmt.Errorf("failed to scan dependent: %w", err)
		}
		dependents = append(dependents, depID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating dependents: %w", err)
	}

	return dependents, nil
}

// ClaimTask atomically claims a task as in_progress by a given agent.
// It reuses the claimableSQL predicate to ensure the task is ready, has no unfinished deps,
// and has no live lease. The claim also checks that the task's model matches the declared model.
// The claim is a single conditional UPDATE.
// Returns the claimed Task on success (rowsAffected == 1).
// Returns ErrNotFound if the task doesn't exist.
// Returns MODEL_MISMATCH ConflictError if the task's model doesn't match.
// Returns ErrConflict if the task is not claimable (already claimed, not ready, unfinished deps, etc).
//
// ClaimTask is the legacy/direct claim path and obeys the research admission
// policy like every other claim: a research task under an enabled policy is
// admitted (or denied with *AdmissionDeniedError) by ClaimResearchTask with a
// server-generated request ID. Non-research tasks and a disabled policy keep
// the plain behavior above.
func (s *sqliteStore) ClaimTask(ctx context.Context, taskID, agentID, model string, leaseTTL time.Duration) (Task, error) {
	res, err := s.ClaimResearchTask(ctx, ResearchClaim{TaskID: taskID, AgentID: agentID, Model: model, LeaseTTL: leaseTTL})
	return res.Task, err
}

// claimTaskTx performs the plain claim inside tx at the given server time:
// one conditional UPDATE reusing claimableSQL with the model check, a claim
// event, and a read of the claimed task. When nothing was claimed the error says why.
func (s *sqliteStore) claimTaskTx(ctx context.Context, tx *sql.Tx, now time.Time, taskID, agentID, model string, leaseTTL time.Duration) (Task, error) {
	nowTS := formatTS(now)
	leaseExpiry := formatTS(now.Add(leaseTTL))

	result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET state='in_progress', assignee=?, lease_expires_at=?, updated_at=?
		WHERE id=? AND model=? AND `+claimableSQL,
		agentID, leaseExpiry, nowTS, taskID, model, nowTS)
	if err != nil {
		return Task{}, fmt.Errorf("failed to claim task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected != 1 {
		return Task{}, classifyClaimFailure(ctx, tx, nowTS, taskID, model)
	}

	if _, err := s.AppendEvent(ctx, tx, taskID, agentID, "claim", nil, nil); err != nil {
		return Task{}, fmt.Errorf("failed to append claim event: %w", err)
	}

	var t Task
	var reviewModelsJSON *string
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch claimed task: %w", err)
	}
	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}
	return t, nil
}

// classifyClaimFailure explains why a task is not claimable. It never returns nil.
func classifyClaimFailure(ctx context.Context, tx *sql.Tx, nowTS, taskID, model string) error {
	var taskExists bool
	var taskModel string
	var isOtherwiseClaimable bool
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) > 0, COALESCE(model, ''), EXISTS(SELECT 1 FROM task WHERE id = ? AND `+claimableSQL+`)
		FROM task WHERE id = ?
	`, taskID, nowTS, taskID).Scan(&taskExists, &taskModel, &isOtherwiseClaimable)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("failed to check task: %w", err)
	}
	if !taskExists {
		return ErrNotFound
	}
	if taskModel != model && isOtherwiseClaimable {
		return conflict("MODEL_MISMATCH", fmt.Sprintf("Task model '%s' does not match declared model '%s'", taskModel, model))
	}
	// Not ready, unfinished deps, held, live lease, or model mismatch on an unclaimable task.
	return ErrConflict
}

// claimPreflight reports nil when a claim by a task of this model would succeed
// right now, else the error the claim would return. It writes nothing.
func claimPreflight(ctx context.Context, tx *sql.Tx, now time.Time, taskID, model string) error {
	nowTS := formatTS(now)
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task WHERE id = ? AND model = ? AND `+claimableSQL+`)`, taskID, model, nowTS).Scan(&ok)
	if err != nil {
		return fmt.Errorf("failed to check task claimability: %w", err)
	}
	if ok {
		return nil
	}
	return classifyClaimFailure(ctx, tx, nowTS, taskID, model)
}

// HeartbeatTask atomically extends the lease on an in_progress task.
// It reuses the pattern from ClaimTask: a single conditional UPDATE within a transaction,
// updating lease_expires_at and appending a heartbeat event in the same tx.
// Returns the updated Task on success (rowsAffected == 1).
// Returns ErrNotFound if the task doesn't exist.
// Returns ErrConflict if the task is not in_progress or not assigned to the given agentID.
// A research attempt ID in ctx (WithResearchAttempt) must be the task's current
// attempt for that agent, else the heartbeat fails with an ATTEMPT_FENCED or
// ATTEMPT_EXPIRED ConflictError. A live attempt is renewed alongside the task
// lease (never shortened); a heartbeat never spends a start.
func (s *sqliteStore) HeartbeatTask(ctx context.Context, taskID, agentID string, leaseTTL time.Duration) (Task, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	nowT := s.nowTime()
	now := formatTS(nowT)
	leaseExpiry := formatTS(nowT.Add(leaseTTL))

	// Single conditional UPDATE: only update if state is 'in_progress' AND assignee matches
	result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET lease_expires_at=?, updated_at=?
		WHERE id=? AND state='in_progress' AND assignee=?
	`, leaseExpiry, now, taskID, agentID)
	if err != nil {
		return Task{}, fmt.Errorf("failed to heartbeat task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 1 {
		// A research attempt bound to this task is fenced and renewed with the
		// lease; a stale attempt fails here and the UPDATE above rolls back.
		if err := fenceResearchAttempt(ctx, tx, nowT, taskID, agentID, researchAttemptFromContext(ctx), leaseTTL); err != nil {
			return Task{}, err
		}
		// SELECT the updated task within the same transaction
		var t Task
		var reviewModelsJSON *string
		err = tx.QueryRowContext(ctx, `
			SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
			FROM task WHERE id = ?
		`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
		if err != nil {
			return Task{}, fmt.Errorf("failed to fetch updated task: %w", err)
		}

		// Unmarshal review_models from JSON
		t.ReviewModels = []string{}
		if reviewModelsJSON != nil {
			if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
				return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
		}

		return t, nil
	}

	// rowsAffected == 0: task was not heartbeateable. Determine the cause for the right error.
	var taskExists bool
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM task WHERE id = ?", taskID).Scan(&taskExists)
	if err != nil {
		return Task{}, fmt.Errorf("failed to check task existence: %w", err)
	}

	if !taskExists {
		// Task does not exist -> ErrNotFound
		tx.Rollback()
		return Task{}, ErrNotFound
	}

	// Task exists but not heartbeateable (not in_progress or wrong assignee) -> ErrConflict
	tx.Rollback()
	return Task{}, ErrConflict
}

// PromoteTask atomically promotes a task from backlog to ready.
// It performs a single conditional UPDATE statement within a transaction.
// If the task is in backlog, it updates state to 'ready', appends a transition event,
// and returns the promoted Task.
// Returns ErrNotFound if the task doesn't exist.
// Returns ErrConflict if the task is not in backlog.
func (s *sqliteStore) PromoteTask(ctx context.Context, taskID string) (Task, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := nowTimestamp()

	// Single conditional UPDATE: only update if state is 'backlog'
	result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET state='ready', updated_at=?
		WHERE id=? AND state='backlog'
	`, now, taskID)
	if err != nil {
		return Task{}, fmt.Errorf("failed to promote task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 1 {
		// Promotion succeeded. Append transition event in the same transaction.
		note := "backlog->ready"
		_, err := s.AppendEvent(ctx, tx, taskID, "system", "transition", nil, &note)
		if err != nil {
			return Task{}, fmt.Errorf("failed to append transition event: %w", err)
		}

		// SELECT the promoted task within the same transaction
		var t Task
		var reviewModelsJSON *string
		err = tx.QueryRowContext(ctx, `
			SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
			FROM task WHERE id = ?
		`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
		if err != nil {
			return Task{}, fmt.Errorf("failed to fetch promoted task: %w", err)
		}

		// Unmarshal review_models from JSON
		t.ReviewModels = []string{}
		if reviewModelsJSON != nil {
			if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
				return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
		}

		return t, nil
	}

	// rowsAffected == 0: task was not in backlog. Determine the cause for the right error.
	var taskExists bool
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM task WHERE id = ?", taskID).Scan(&taskExists)
	if err != nil {
		return Task{}, fmt.Errorf("failed to check task existence: %w", err)
	}

	if !taskExists {
		// Task does not exist -> ErrNotFound
		tx.Rollback()
		return Task{}, ErrNotFound
	}

	// Task exists but not in backlog -> ErrConflict
	tx.Rollback()
	return Task{}, ErrConflict
}

// SubmitTask atomically transitions a task from in_progress to review.
// It validates link kinds, updates the task (clearing the lease), inserts task_link rows,
// appends a submit event, and returns the updated task with all links, all within one transaction.
// For review tasks, verdict is required and moves the task to done, appending a review event on the parent.
// For implement tasks, verdict is forbidden.
// thresholdFor determines the review round threshold for a given model.
// It uses escalationThresholds if provided, otherwise falls back to the default maxReviewRounds.
// For known models without overrides, use built-in defaults: haiku=8, sonnet=6, opus=4.
func thresholdFor(model string, escalationThresholds map[string]int, maxReviewRounds int) int {
	defaults := map[string]int{"haiku": 8, "sonnet": 6, "opus": 4}
	if len(escalationThresholds) > 0 {
		if threshold, ok := escalationThresholds[model]; ok {
			return threshold
		}
	}
	if threshold, ok := defaults[model]; ok {
		return threshold
	}
	return maxReviewRounds
}

func researchThresholdFor(model string, researchLadder []string, researchThresholds map[string]int, maxReviewRounds int) int {
	// Research thresholds only apply if a research ladder is configured
	if len(researchLadder) == 0 {
		return maxReviewRounds
	}
	// If model is on the research ladder, use its threshold if configured
	if slices.Contains(researchLadder, model) {
		if threshold, ok := researchThresholds[model]; ok {
			return threshold
		}
	}
	// Model not on ladder, or no threshold configured for it: use max rounds
	return maxReviewRounds
}

// When all reviews are done and at least one rejected, the parent transitions to ready if review_round <= threshold,
// or to blocked if review_round > threshold (circuit breaker).
// Returns the updated TaskWithDepsAndLinks on success.
// Returns ValidationError if a link kind is invalid or verdict is missing/invalid.
// Returns ErrNotFound if the task doesn't exist.
// Returns ErrConflict if the task is not in_progress or not assigned to the given agentID.
// SubmitTask submits a task result, with optional structured review findings
// (review-kind tasks only). It is a thin wrapper over submitTask for callers with no
// disputes to submit; the variadic findings parameter keeps its existing call sites
// (of which there are many, across the store, API and CLI) unchanged.
func (s *sqliteStore) SubmitTask(ctx context.Context, taskID, agentID, result string, verdict *string, links []LinkInput, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int, findings ...json.RawMessage) (TaskWithDepsAndLinks, error) {
	var f json.RawMessage
	if len(findings) > 0 {
		f = findings[0]
	}
	return s.submitTask(ctx, taskID, agentID, result, verdict, links, maxReviewRounds, escalationThresholds, researchEscalationThresholds, researchRoundBudget, f, nil, nil)
}

// SubmitTaskWithDisputes submits a task result along with a disputes payload: a
// worker's dispute, on a research-track rework submission, of specific findings from
// the round it is reworking, per docs/features/research-track.md section 5. See
// submitTask for the full behavior.
func (s *sqliteStore) SubmitTaskWithDisputes(ctx context.Context, taskID, agentID, result string, verdict *string, links []LinkInput, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int, findings json.RawMessage, disputes json.RawMessage) (TaskWithDepsAndLinks, error) {
	return s.submitTask(ctx, taskID, agentID, result, verdict, links, maxReviewRounds, escalationThresholds, researchEscalationThresholds, researchRoundBudget, findings, disputes, nil)
}

// SubmitTaskWithManifest is SubmitTaskWithDisputes plus an optional continuation manifest
// (docs/features/research-continuations.md): a research-track implement task whose spec opts in
// may carry the children it proposes. The manifest is validated, canonicalised and stored for
// the review round this submission starts, in the same transaction as the submission itself.
func (s *sqliteStore) SubmitTaskWithManifest(ctx context.Context, taskID, agentID, result string, verdict *string, links []LinkInput, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int, findings json.RawMessage, disputes json.RawMessage, manifestInput json.RawMessage) (TaskWithDepsAndLinks, error) {
	return s.submitTask(ctx, taskID, agentID, result, verdict, links, maxReviewRounds, escalationThresholds, researchEscalationThresholds, researchRoundBudget, findings, disputes, manifestInput)
}

func (s *sqliteStore) submitTask(ctx context.Context, taskID, agentID, result string, verdict *string, links []LinkInput, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int, findings json.RawMessage, disputes json.RawMessage, manifestInput json.RawMessage) (TaskWithDepsAndLinks, error) {
	// Validate link kinds first (before mutating anything).
	// "no_op" marks a review-verified no-op resolution: a worker that finds the
	// acceptance criteria already satisfied on main with no diff submits with a
	// no_op marker and NO pr link; the reviewer verifies the claim against main.
	validKinds := map[string]bool{"pr": true, "branch": true, "commit": true, "ci": true, "no_op": true}
	for _, link := range links {
		if !validKinds[link.Kind] {
			return TaskWithDepsAndLinks{}, invalid("INVALID_LINK_KIND", fmt.Sprintf("invalid link kind: %s", link.Kind))
		}
	}

	// Strip raw control characters from the free-text result before storage.
	result = sanitizeFreeText(result)

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := nowTimestamp()

	// First, determine the task kind before doing any updates
	var taskKind string
	var targetTaskID *string
	var taskTrack string
	var currentReviewRound int
	err = tx.QueryRowContext(ctx, `
		SELECT kind, target_task_id, track, review_round FROM task WHERE id = ? AND state='in_progress' AND assignee=?
	`, taskID, agentID).Scan(&taskKind, &targetTaskID, &taskTrack, &currentReviewRound)
	if err != nil {
		if err == sql.ErrNoRows {
			// Task not found or not submittable
			var taskExists bool
			txErr := tx.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM task WHERE id = ?", taskID).Scan(&taskExists)
			if txErr != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to check task existence: %w", txErr)
			}
			if !taskExists {
				tx.Rollback()
				return TaskWithDepsAndLinks{}, ErrNotFound
			}
			tx.Rollback()
			return TaskWithDepsAndLinks{}, ErrConflict
		}
		return TaskWithDepsAndLinks{}, fmt.Errorf("failed to determine task kind: %w", err)
	}

	// Reject a submission from a research attempt that a replacement superseded.
	// The attempt itself is left active: submitting ends task ownership, not the
	// dispatch. Only the store-level RenewResearchAttempt and FinalizeResearchAttempt
	// drive that lifecycle; no API route exposes them yet, so until a harness
	// integration calls them the preserved attempt lapses at its lease expiry.
	if err := fenceResearchAttempt(ctx, tx, s.nowTime(), taskID, agentID, researchAttemptFromContext(ctx), 0); err != nil {
		return TaskWithDepsAndLinks{}, err
	}

	// Validate verdict based on task kind
	if taskKind == "review" {
		// Verdict is required for review tasks
		if verdict == nil {
			return TaskWithDepsAndLinks{}, invalid("MISSING_VERDICT", "verdict is required for review tasks")
		}
		// Validate verdict value
		if *verdict != "approve" && *verdict != "reject" {
			return TaskWithDepsAndLinks{}, invalid("INVALID_VERDICT", "verdict must be 'approve' or 'reject'")
		}
	} else if taskKind == "implement" {
		// Verdict is forbidden for implement tasks
		if verdict != nil {
			return TaskWithDepsAndLinks{}, invalid("FORBIDDEN_VERDICT", "verdict is not allowed for implement tasks")
		}
	}

	// Validate findings based on task kind. A findings payload is optional and
	// explicit null is treated the same as absent, so submissions without findings
	// are unaffected. Findings are only accepted on review-kind tasks. Research
	// review tasks are the exception: section 3 requires an explicit findings
	// array (submit [] when there are none), since research aggregation below
	// relies on every review task's findings being present.
	findingsAbsent := findings == nil || string(findings) == "null"
	if taskKind == "review" && taskTrack == "research" && findingsAbsent {
		return TaskWithDepsAndLinks{}, invalid("MISSING_FINDINGS", "findings are required for research review tasks; submit [] when there are none")
	}
	var findingsToStore json.RawMessage
	if !findingsAbsent {
		if taskKind != "review" {
			return TaskWithDepsAndLinks{}, invalid("FINDINGS_NOT_ALLOWED", "findings are only allowed on review-kind tasks")
		}
		parsedFindings, ferr := validateFindings(findings)
		if ferr != nil {
			return TaskWithDepsAndLinks{}, ferr
		}
		marshaled, merr := json.Marshal(parsedFindings)
		if merr != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to marshal findings: %w", merr)
		}
		findingsToStore = marshaled
	}

	// Validate disputes based on task kind/track. A disputes payload is optional and
	// explicit null is treated the same as absent, so ordinary research rework and
	// every build/design submission are unaffected. Disputes are only accepted on
	// research-track implement (rework) submissions that have a prior review round
	// to dispute a finding from, per docs/features/research-track.md section 5.
	// Disputes never alter the finding they name; they are matched here only to
	// confirm the finding was actually raised, and by exactly one reviewer, so it can
	// be routed to that reviewer's next round unambiguously.
	disputesAbsent := disputes == nil || string(disputes) == "null"
	var disputesToStore json.RawMessage
	disputesByLineage := make(map[string][]disputeContextEntry)
	if !disputesAbsent {
		if taskKind != "implement" || taskTrack != "research" {
			return TaskWithDepsAndLinks{}, invalid("DISPUTES_NOT_ALLOWED", "disputes are only allowed on research-track implement (rework) submissions")
		}
		if currentReviewRound == 0 {
			return TaskWithDepsAndLinks{}, invalid("NO_PRIOR_ROUND", "disputes require a prior review round to dispute a finding from")
		}
		parsedDisputes, derr := validateDisputes(disputes)
		if derr != nil {
			return TaskWithDepsAndLinks{}, derr
		}

		// allFindings covers every round through currentReviewRound, so it has both
		// this round's candidate targets and any earlier round's already-disputed
		// findings, in one index space researchFindingChains can link by prior_id
		// within each reviewer's lineage.
		allFindings, _, cerr := s.collectResearchReviewReports(ctx, tx, taskID, currentReviewRound)
		if cerr != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to collect review findings for dispute validation: %w", cerr)
		}
		byID := make(map[string][]int)
		byRoundLineageID := make(map[string]int, len(allFindings))
		for i, cf := range allFindings {
			byRoundLineageID[fmt.Sprintf("%d\x00%s\x00%s", cf.round, cf.lineage, cf.ID)] = i
			if cf.round == currentReviewRound {
				byID[cf.ID] = append(byID[cf.ID], i)
			}
		}
		chainOf, _ := researchFindingChains(allFindings)

		priorDisputed, perr := s.priorDisputes(ctx, tx, taskID)
		if perr != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to collect prior disputes: %w", perr)
		}
		// A prior dispute blocks a new one only if the new target's finding chain —
		// linked by prior_id within one reviewer's lineage — was already disputed,
		// not merely if the raw finding_id string matches: reviewers pick their own
		// ids, so a bare-id match is both too strict (a different reviewer, or a
		// genuinely new finding, can reuse an old id) and too loose (a maintained
		// finding gets a new id each round it's carried forward).
		priorDisputedChainRoots := make(map[int]bool, len(priorDisputed))
		for _, pd := range priorDisputed {
			if idx, ok := byRoundLineageID[fmt.Sprintf("%d\x00%s\x00%s", pd.Round, pd.Lineage, pd.FindingID)]; ok {
				priorDisputedChainRoots[chainOf(idx)] = true
			}
		}

		for i, d := range parsedDisputes {
			targets := byID[d.FindingID]
			if len(targets) == 0 {
				return TaskWithDepsAndLinks{}, invalid("UNKNOWN_FINDING_ID", fmt.Sprintf("disputes: finding_id %q was not raised by any reviewer in round %d", d.FindingID, currentReviewRound))
			}
			if len(targets) > 1 {
				return TaskWithDepsAndLinks{}, invalid("AMBIGUOUS_FINDING_ID", fmt.Sprintf("disputes: finding_id %q was raised by more than one reviewer in round %d and cannot be disputed unambiguously", d.FindingID, currentReviewRound))
			}
			idx := targets[0]
			if priorDisputedChainRoots[chainOf(idx)] {
				return TaskWithDepsAndLinks{}, invalid("DUPLICATE_DISPUTE", fmt.Sprintf("disputes: finding_id %q was already disputed in an earlier round", d.FindingID))
			}
			target := allFindings[idx]
			disputesByLineage[target.lineage] = append(disputesByLineage[target.lineage], disputeContextEntry{Finding: target.Finding, Evidence: d.Evidence})
			parsedDisputes[i].Round = currentReviewRound
			parsedDisputes[i].Lineage = target.lineage
		}

		marshaled, merr := json.Marshal(parsedDisputes)
		if merr != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to marshal disputes: %w", merr)
		}
		disputesToStore = marshaled
	}

	// Validate the continuation manifest. It is optional and explicit null is treated as absent,
	// so submissions without one are unaffected. Only a research-track implement task whose spec
	// opts in may carry one, and it must name the submitted task as its parent.
	var submittedManifest *canonicalManifest
	if len(manifestInput) > 0 && string(manifestInput) != "null" {
		if taskKind != "implement" || taskTrack != "research" {
			return TaskWithDepsAndLinks{}, invalid("MANIFEST_NOT_ALLOWED", "a continuation manifest is only allowed on research-track implement submissions")
		}
		var spec string
		if err := tx.QueryRowContext(ctx, `SELECT spec FROM task WHERE id = ?`, taskID).Scan(&spec); err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to read task spec: %w", err)
		}
		if !specOptsIntoContinuations(spec) {
			return TaskWithDepsAndLinks{}, invalid("PARENT_NOT_OPTED_IN", fmt.Sprintf("task spec does not opt in to continuation manifests (it needs a line reading %q)", continuationOptInHeading))
		}
		cm, merr := s.canonicalizeManifest(manifestInput, taskID)
		if merr != nil {
			return TaskWithDepsAndLinks{}, merr
		}
		submittedManifest = cm
	}

	// Determine the next state based on task kind
	var nextState string
	if taskKind == "review" {
		nextState = "done"
	} else {
		nextState = "review"
	}

	// Single conditional UPDATE: transition from in_progress to the next state
	result_ptr := &result
	update_result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET state=?, result=?, verdict=?, lease_expires_at=NULL, updated_at=?
		WHERE id=? AND state='in_progress' AND assignee=?
	`, nextState, result_ptr, verdict, now, taskID, agentID)
	if err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("failed to submit task: %w", err)
	}

	rowsAffected, err := update_result.RowsAffected()
	if err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 1 {
		// Submit succeeded. Insert task_link rows (dedup by task_id, kind, value). An implement
		// submission starts review round currentReviewRound+1 (incremented below); its links are
		// tagged with that round, so readers can tell this round's commit from earlier rounds'.
		var linkRound *int
		if taskKind == "implement" {
			submittedRound := currentReviewRound + 1
			linkRound = &submittedRound
		}
		for _, link := range links {
			// Check if this link already exists
			var existingCount int
			err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM task_link WHERE task_id = ? AND kind = ? AND value = ?
			`, taskID, link.Kind, link.Value).Scan(&existingCount)
			if err != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to check link existence: %w", err)
			}

			if existingCount == 0 {
				linkID := GenerateID()
				_, err := tx.ExecContext(ctx, `
					INSERT INTO task_link (id, task_id, kind, value, review_round)
					VALUES (?, ?, ?, ?, ?)
				`, linkID, taskID, link.Kind, link.Value, linkRound)
				if err != nil {
					return TaskWithDepsAndLinks{}, fmt.Errorf("failed to insert link: %w", err)
				}
			} else if linkRound != nil {
				// Re-submitted unchanged (e.g. the same PR across rounds): it belongs to this round now.
				_, err := tx.ExecContext(ctx, `
					UPDATE task_link SET review_round = ? WHERE task_id = ? AND kind = ? AND value = ?
				`, linkRound, taskID, link.Kind, link.Value)
				if err != nil {
					return TaskWithDepsAndLinks{}, fmt.Errorf("failed to update link round: %w", err)
				}
			}
		}

		// Append submit event in the same transaction
		submitEvent, err := s.AppendEvent(ctx, tx, taskID, agentID, "submit", nil, nil)
		if err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to append submit event: %w", err)
		}

		// Record this submission's disputes on its own submit event. Disputes ride
		// alongside AppendEvent's shared findings column rather than through it,
		// since findings and disputes are mutually exclusive by task kind (findings on
		// review-kind submissions, disputes on implement-kind rework) and never occur
		// on the same event.
		if disputesToStore != nil {
			if _, err := tx.ExecContext(ctx, `
				UPDATE event SET disputes = ? WHERE id = ?
			`, string(disputesToStore), submitEvent.ID); err != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to record disputes on submit event: %w", err)
			}
		}

		// Fetch the submitted task to check if it's an implement task
		var t Task
		var reviewModelsJSON *string
		err = tx.QueryRowContext(ctx, `
			SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
			FROM task WHERE id = ?
		`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
		if err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to fetch submitted task: %w", err)
		}

		// Unmarshal review_models from JSON
		t.ReviewModels = []string{}
		if reviewModelsJSON != nil {
			if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
			}
		}

		// If this is an implement task entering review, auto-spawn review tasks
		if t.Kind == "implement" {
			// Increment review_round
			newReviewRound := t.ReviewRound + 1
			_, err := tx.ExecContext(ctx, `
				UPDATE task SET review_round = ? WHERE id = ?
			`, newReviewRound, taskID)
			if err != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to increment review_round: %w", err)
			}

			if submittedManifest != nil {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO task_submission_manifest (id, task_id, review_round, parent_task_id, manifest_json, manifest_digest, created_at)
					VALUES (?, ?, ?, ?, ?, ?, ?)
				`, GenerateID(), taskID, newReviewRound, submittedManifest.parentTaskID, submittedManifest.json, submittedManifest.digest, now); err != nil {
					return TaskWithDepsAndLinks{}, fmt.Errorf("failed to store submission manifest: %w", err)
				}
			}

			// Extract PR link and any no_op marker from the submitted links.
			// A no_op submission carries the marker and NO pr link: the implementer
			// claims the acceptance is already satisfied on main with no diff.
			var prLink string
			var noOpMarker string
			for _, link := range links {
				switch link.Kind {
				case "pr":
					prLink = link.Value
				case "no_op":
					noOpMarker = link.Value
				}
			}
			isNoOp := noOpMarker != "" && prLink == ""

			// Determine reviewers (default to ["opus"] if empty)
			reviewers := defaultReviewModels(t.ReviewModels)

			// Create a review task for each reviewer
			reviewerSlotsTaken := make(map[string]int)
			for _, reviewerModel := range reviewers {
				reviewTaskID := GenerateID()
				reviewTitle := "Review: " + t.Title + " [" + reviewerModel + "]"

				// This reviewer's lineage among this round's review tasks (see
				// researchReviewerLineage): the same lineage computation
				// collectResearchReviewReports uses when it later reads this round's
				// review tasks back, so a dispute resolved against a lineage above is
				// routed to the matching reviewer's spec below.
				slot := reviewerSlotsTaken[reviewerModel]
				reviewerSlotsTaken[reviewerModel] = slot + 1
				lineage := researchReviewerLineage(reviewerModel, slot)

				// Build the spec for the review task: a strict-review brief pointing at the parent's PR link
				reviewSpec := "Review the implementation:\n\n"
				if prLink != "" {
					reviewSpec += "Implementation PR: " + prLink + "\n\n"
				}
				reviewSpec += "Parent task: " + t.ID + "\n\n"
				if isNoOp {
					reviewSpec += "## NO-OP submission (verify, do not auto-reject)\n\n"
					reviewSpec += "The implementer reports the parent's acceptance criteria are ALREADY satisfied on `main` with no code changes, so there is NO PR. Do NOT reject merely because a PR is missing. VERIFY the claim against current `main`: if the parent's acceptance criteria genuinely hold in the repo, approve; if work is actually needed, reject with the specific gap.\n\n"
				}
				if disputed := disputesByLineage[lineage]; len(disputed) > 0 {
					reviewSpec += "## Prior Review Round: Disputed Finding(s)\n\n"
					reviewSpec += fmt.Sprintf("The worker disputes the following finding(s) you raised in round %d, citing source evidence. Re-evaluate each against the evidence: withdraw it (report it resolved) if the evidence resolves it, or maintain it (report it still_open) if not. This does not change the finding on record; your re-evaluation is what decides it.\n\n", currentReviewRound)
					for _, entry := range disputed {
						reviewSpec += fmt.Sprintf("- Finding %s [%s] at %s:%d: %s\n", entry.Finding.ID, entry.Finding.Severity, entry.Finding.File, entry.Finding.Line, entry.Finding.Summary)
						reviewSpec += fmt.Sprintf("  Worker's evidence: %s\n", entry.Evidence)
					}
					reviewSpec += "\n"
				}
				reviewSpec += "## Instructions\n\n"
				reviewSpec += "Examine the submitted implementation and provide approval or rejection with written feedback.\n\n"
				reviewSpec += "Approve if:\n"
				reviewSpec += "- The implementation matches the specification\n"
				reviewSpec += "- The code is correct and follows the project conventions\n"
				reviewSpec += "- Tests pass and coverage is adequate\n\n"
				reviewSpec += "Reject if:\n"
				reviewSpec += "- The implementation has issues or does not match the specification\n"
				reviewSpec += "- Further work is needed before merging\n\n"
				reviewSpec += "Provide your verdict: approve or reject"

				reviewModelsJSON := (*string)(nil) // review tasks don't have review_models
				_, err := tx.ExecContext(ctx, `
					INSERT INTO task (id, project_id, document_id, title, spec, state, model, kind, review_models, review_round, target_task_id, agent_merge, track, priority, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				`, reviewTaskID, t.ProjectID, t.DocumentID, reviewTitle, reviewSpec, "ready", reviewerModel, "review", reviewModelsJSON, newReviewRound, &t.ID, false, t.Track, t.Priority, now, now)
				if err != nil {
					return TaskWithDepsAndLinks{}, fmt.Errorf("failed to create review task: %w", err)
				}
			}

			// Append spawn_review event on the parent task
			reviewersList, _ := json.Marshal(reviewers)
			eventNote := "Round " + fmt.Sprintf("%d", newReviewRound) + " with models: " + string(reviewersList)
			_, err = s.AppendEvent(ctx, tx, taskID, "system", "spawn_review", nil, &eventNote)
			if err != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to append spawn_review event: %w", err)
			}

			// Update t.ReviewRound for the response
			t.ReviewRound = newReviewRound
		} else if t.Kind == "review" && targetTaskID != nil {
			// This is a review task. Append a review event on the parent task, tagged
			// with this review task's own id so research aggregation can identify
			// the current round's own submissions among all review events on the parent.
			_, err := s.appendEvent(ctx, tx, *targetTaskID, agentID, "review", verdict, &result, &taskID, findingsToStore)
			if err != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to append review event on parent: %w", err)
			}

			// Aggregate review verdicts and update parent state as needed
			_, err = s.aggregateReviewRound(ctx, tx, *targetTaskID, maxReviewRounds, escalationThresholds, s.researchEscalationLadder, researchEscalationThresholds, researchRoundBudget)
			if err != nil {
				return TaskWithDepsAndLinks{}, err
			}
		}

		// Fetch dependencies
		depRows, err := tx.QueryContext(ctx, `
			SELECT depends_on_id FROM task_dep WHERE task_id = ? ORDER BY depends_on_id
		`, taskID)
		if err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to query dependencies: %w", err)
		}
		defer depRows.Close()

		dependsOn := make([]string, 0)
		for depRows.Next() {
			var depID string
			if err := depRows.Scan(&depID); err != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to scan dependency: %w", err)
			}
			dependsOn = append(dependsOn, depID)
		}
		if err := depRows.Err(); err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("error iterating dependencies: %w", err)
		}

		// Fetch links (including those we just inserted)
		linkRows, err := tx.QueryContext(ctx, `
			SELECT id, task_id, kind, value, tombstoned_at, review_round FROM task_link WHERE task_id = ? ORDER BY id
		`, taskID)
		if err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to query links: %w", err)
		}
		defer linkRows.Close()

		fetchedLinks := make([]TaskLink, 0)
		for linkRows.Next() {
			var link TaskLink
			if err := linkRows.Scan(&link.ID, &link.TaskID, &link.Kind, &link.Value, &link.TombstonedAt, &link.ReviewRound); err != nil {
				return TaskWithDepsAndLinks{}, fmt.Errorf("failed to scan link: %w", err)
			}
			fetchedLinks = append(fetchedLinks, link)
		}
		if err := linkRows.Err(); err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("error iterating links: %w", err)
		}

		manifests, err := listSubmissionManifests(ctx, tx, taskID)
		if err != nil {
			return TaskWithDepsAndLinks{}, err
		}

		if err := tx.Commit(); err != nil {
			return TaskWithDepsAndLinks{}, fmt.Errorf("failed to commit transaction: %w", err)
		}

		return TaskWithDepsAndLinks{
			ID:             t.ID,
			ProjectID:      t.ProjectID,
			DocumentID:     t.DocumentID,
			Title:          t.Title,
			Spec:           t.Spec,
			State:          t.State,
			Assignee:       t.Assignee,
			LeaseExpiresAt: t.LeaseExpiresAt,
			Result:         t.Result,
			Model:          t.Model,
			Kind:           t.Kind,
			ReviewModels:   t.ReviewModels,
			ReviewRound:    t.ReviewRound,
			TargetTaskID:   t.TargetTaskID,
			Verdict:        t.Verdict,
			AgentMerge:     t.AgentMerge,
			Held:           t.Held,
			Escalate:       t.Escalate,
			Track:          t.Track,
			Branch:         t.Branch,
			CreatedAt:      t.CreatedAt,
			UpdatedAt:      t.UpdatedAt,
			ArchivedAt:     t.ArchivedAt,
			SupersededBy:   t.SupersededBy,
			DependsOn:      dependsOn,
			Links:          fetchedLinks,

			SubmissionManifests: manifests,
		}, nil
	}

	// rowsAffected == 0: task was not submittable. Determine the cause for the right error.
	var taskExists bool
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM task WHERE id = ?", taskID).Scan(&taskExists)
	if err != nil {
		return TaskWithDepsAndLinks{}, fmt.Errorf("failed to check task existence: %w", err)
	}

	if !taskExists {
		// Task does not exist -> ErrNotFound
		tx.Rollback()
		return TaskWithDepsAndLinks{}, ErrNotFound
	}

	// Task exists but not submittable (not in_progress or wrong assignee) -> ErrConflict
	tx.Rollback()
	return TaskWithDepsAndLinks{}, ErrConflict
}

// handleApprovedRound finalizes a review round whose outcome permits merging: for
// build and design this is "every reviewer approved"; for research it is "no
// reviewer reported a blocking finding". It checks for a no-op resolution (which
// finalizes straight to done), otherwise spawns a merge task when agent_merge is
// set, otherwise leaves the parent at approved. Returns the new parent state.
func (s *sqliteStore) handleApprovedRound(ctx context.Context, tx *sql.Tx, parentID string, parentReviewRound int, parentModel, parentTrack, parentTitle, parentProjectID, parentDocumentID string, parentAgentMerge bool, now string) (string, error) {
	newParentState := "approved"

	// The no-op finalisation needs the approved round itself to be a no_op: a no_op from an
	// earlier, rejected round must not finalise a later round that submitted real work (in
	// local_commit mode there is no pr link to tell them apart). The task's PR, by contrast, is
	// task-wide: a rework pushes to the same PR, and may not re-submit its link.
	roundLinks, err := submissionLinks(ctx, tx, parentID, parentReviewRound)
	if err != nil {
		return "", err
	}
	var hasNoOp bool
	for _, link := range roundLinks {
		if link.Kind == "no_op" {
			hasNoOp = true
		}
	}
	var activePRLinks int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM task_link WHERE task_id = ? AND kind = 'pr' AND tombstoned_at IS NULL
	`, parentID).Scan(&activePRLinks); err != nil {
		return "", fmt.Errorf("failed to query active pr links: %w", err)
	}
	hasPR := activePRLinks > 0

	// If no_op link with no pr link, go straight to done (regardless of agent_merge)
	if hasNoOp && !hasPR {
		newParentState = "done"
	} else if parentAgentMerge && hasPR {
		// Spawn merge task if approved with agent_merge && pr (not the no_op case)
		parentPriority := DefaultPriority
		if err := tx.QueryRowContext(ctx, `SELECT priority FROM task WHERE id = ?`, parentID).Scan(&parentPriority); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("failed to read parent priority for merge task: %w", err)
		}

		mergeTaskID := GenerateID()
		mergeTitle := "Merge: " + parentTitle
		_, err := tx.ExecContext(ctx, `
			INSERT INTO task (id, project_id, document_id, title, spec, state, model, kind, target_task_id, agent_merge, track, priority, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, mergeTaskID, parentProjectID, parentDocumentID, mergeTitle, "", "ready", parentModel, "merge", parentID, false, parentTrack, parentPriority, now, now)
		if err != nil {
			return "", fmt.Errorf("failed to create merge task: %w", err)
		}
	}

	return newParentState, nil
}

// isBlockingResearchFinding reports whether a single research review finding blocks
// its round, per docs/features/research-track.md section 3:
//   - P1/P2 in changed text blocks.
//   - P1/P2 anywhere blocks during round 1 (round 1 findings are always in changed
//     text by definition, but this is enforced independently of the reviewer's own
//     in_changed_text report).
//   - A still_open P1 or P2 finding blocks. A P3 never blocks.
//   - A resolved finding never blocks, even if it would otherwise match the rules
//     above: a reviewer reports it as resolved precisely because the round's changes
//     fixed the text the finding was raised against.
func isBlockingResearchFinding(f Finding, round int) bool {
	if f.Status == "resolved" {
		return false
	}
	isP1OrP2 := f.Severity == "P1" || f.Severity == "P2"
	if isP1OrP2 && f.InChangedText {
		return true
	}
	if isP1OrP2 && round == 1 {
		return true
	}
	return isP1OrP2 && f.Status == "still_open"
}

// checkResearchBlockingFindings reports whether any reviewer in the current round
// raised a blocking finding, per docs/features/research-track.md section 3, and
// collects every blocking finding raised (used to record the round-rejected event
// section 6's chain-wide round budget reads), each tagged with the reviewer lineage
// (see researchReviewerLineage) that raised it, so callers can trace a blocking
// finding back to a specific dispute for section 5 adjudication. It reads only the
// review events sourced from this round's own review tasks (via source_task_id), so
// it is unaffected by unrelated review events on the parent, e.g. a human/API
// AddReview call. An adjudication task (adjudicate_finding_id IS NOT NULL) is never
// one of this round's own review tasks: its ruling is binding only for the one
// finding it adjudicates and never votes on the round (section 5). It fails closed:
// if it cannot account for every review task's findings (a missing or unparseable
// findings payload, or fewer matching events than review tasks), it reports a
// blocking finding rather than risk a silent approval; the returned findings list may
// be empty in that case, since there's nothing valid to report.
func (s *sqliteStore) checkResearchBlockingFindings(ctx context.Context, tx *sql.Tx, parentID string, round int) (bool, []Finding, []string, error) {
	taskRows, err := tx.QueryContext(ctx, `
		SELECT id, model FROM task
		WHERE target_task_id = ? AND review_round = ? AND kind = 'review' AND adjudicate_finding_id IS NULL
		ORDER BY rowid
	`, parentID, round)
	if err != nil {
		return false, nil, nil, fmt.Errorf("failed to list round review tasks: %w", err)
	}
	reviewTaskIDs := make(map[string]bool)
	// Lineage mirrors researchReviewerLineage's computation in
	// collectResearchReviewReports: model plus the task's index among this round's
	// review tasks for that model, in creation order. Slot is round-local (recomputed
	// fresh per round), so restricting this query to `round` yields the same lineage
	// collectResearchReviewReports would compute from the full history.
	taskLineage := make(map[string]string)
	slotsTaken := make(map[string]int)
	for taskRows.Next() {
		var id, model string
		if err := taskRows.Scan(&id, &model); err != nil {
			taskRows.Close()
			return false, nil, nil, fmt.Errorf("failed to scan review task id: %w", err)
		}
		reviewTaskIDs[id] = true
		slot := slotsTaken[model]
		slotsTaken[model] = slot + 1
		taskLineage[id] = researchReviewerLineage(model, slot)
	}
	if err := taskRows.Err(); err != nil {
		taskRows.Close()
		return false, nil, nil, fmt.Errorf("failed to iterate review task ids: %w", err)
	}
	taskRows.Close()

	expected := len(reviewTaskIDs)
	if expected == 0 {
		return false, nil, nil, nil
	}

	// No LIMIT: an intervening AddReview (or any other) event on the parent must
	// never push a review task's own event out of the window we scan.
	evRows, err := tx.QueryContext(ctx, `
		SELECT source_task_id, findings FROM event
		WHERE task_id = ? AND kind = 'review' AND source_task_id IS NOT NULL
	`, parentID)
	if err != nil {
		return false, nil, nil, fmt.Errorf("failed to query review events: %w", err)
	}
	defer evRows.Close()

	seen := make(map[string]bool, expected)
	failClosed := false
	var blocking []Finding
	var blockingLineages []string
	for evRows.Next() {
		var sourceTaskID string
		var findingsText sql.NullString
		if err := evRows.Scan(&sourceTaskID, &findingsText); err != nil {
			return false, nil, nil, fmt.Errorf("failed to scan review event: %w", err)
		}
		if !reviewTaskIDs[sourceTaskID] || seen[sourceTaskID] {
			continue
		}
		seen[sourceTaskID] = true

		if !findingsText.Valid {
			// Fail closed: a research review event with no findings recorded.
			failClosed = true
			continue
		}
		var findings []Finding
		if err := json.Unmarshal([]byte(findingsText.String), &findings); err != nil {
			// Fail closed: malformed stored findings.
			failClosed = true
			continue
		}
		for _, f := range findings {
			if isBlockingResearchFinding(f, round) {
				blocking = append(blocking, f)
				blockingLineages = append(blockingLineages, taskLineage[sourceTaskID])
			}
		}
	}
	if err := evRows.Err(); err != nil {
		return false, nil, nil, fmt.Errorf("failed to iterate review events: %w", err)
	}

	if len(seen) != expected {
		// Fail closed: couldn't account for every review task's findings.
		failClosed = true
	}

	if failClosed {
		return true, blocking, blockingLineages, nil
	}

	return len(blocking) > 0, blocking, blockingLineages, nil
}

// researchAnchorKey is the stable identity of one disputed research finding: the
// (round, reviewer lineage, finding id) at which it was first disputed (Dispute.Round
// / Dispute.Lineage / Dispute.FindingID, resolved once by submitTask). It stays valid
// across however many further rounds the same reviewer carries the finding forward
// under a new id, because researchFindingChains links prior_id chains within one
// lineage and always resolves a chain's union-find root to its earliest member.
func researchAnchorKey(round int, lineage, findingID string) string {
	return fmt.Sprintf("%d\x00%s\x00%s", round, lineage, findingID)
}

// applyResearchAdjudication implements docs/features/research-track.md section 5 for
// one research round's blocking findings: a blocking finding that is the maintained
// continuation of a worker-disputed finding is decided by adjudication, not by its raw
// status, and never votes on the round itself. items and lineages are
// checkResearchBlockingFindings' returned blocking findings and the reviewer lineage
// that raised each, for the same round.
//
// It returns, parallel to items, whether each must be excluded from the round's
// blocking findings because an adjudicator overturned it (or an earlier round's
// adjudicator did, for a finding carried forward since), and whether the round as a
// whole must be deferred: left entirely undecided, with no rejected-round event, no
// budget count and no circuit breaker, because at least one relevant adjudication was
// just spawned or is still pending. A finding whose adjudicator upheld it, or whose
// dispute has no usable adjudicator at all, is left un-excluded: it blocks under the
// same rule as any other still_open finding.
func (s *sqliteStore) applyResearchAdjudication(ctx context.Context, tx *sql.Tx, parentID string, round int, parentReviewModels []string, items []Finding, lineages []string, now string) ([]bool, bool, error) {
	excluded := make([]bool, len(items))
	if len(items) == 0 {
		return excluded, false, nil
	}

	priorDisputed, err := s.priorDisputes(ctx, tx, parentID)
	if err != nil {
		return nil, false, fmt.Errorf("failed to collect prior disputes for adjudication: %w", err)
	}
	if len(priorDisputed) == 0 {
		return excluded, false, nil
	}

	// allFindings covers the parent's full review history through this round, so a
	// disputed finding's chain can be traced no matter how many further rounds it was
	// carried forward under a new id since the dispute (or since an earlier ruling).
	allFindings, _, err := s.collectResearchReviewReports(ctx, tx, parentID, round)
	if err != nil {
		return nil, false, fmt.Errorf("failed to collect review reports for adjudication: %w", err)
	}
	chainOf, _ := researchFindingChains(allFindings)

	byAnchorKey := make(map[string]int, len(allFindings))
	for i, cf := range allFindings {
		byAnchorKey[researchAnchorKey(cf.round, cf.lineage, cf.ID)] = i
	}

	// anchorForRoot maps a chain's union-find root index to the dispute that started
	// it, for every dispute this task has ever recorded. A blocking item whose own
	// chain root isn't in this map was never disputed and needs no adjudication.
	anchorForRoot := make(map[int]Dispute, len(priorDisputed))
	for _, pd := range priorDisputed {
		if idx, ok := byAnchorKey[researchAnchorKey(pd.Round, pd.Lineage, pd.FindingID)]; ok {
			anchorForRoot[chainOf(idx)] = pd
		}
	}
	if len(anchorForRoot) == 0 {
		return excluded, false, nil
	}

	var pending bool
	var parentProjectID, parentDocumentID, parentTrack string
	fetchedParent := false

	for i, f := range items {
		idx, ok := byAnchorKey[researchAnchorKey(round, lineages[i], f.ID)]
		if !ok {
			// Should never happen: every blocking item comes from a review event in
			// this exact round, which collectResearchReviewReports also scanned.
			// Fall through to default (non-excluded) treatment defensively.
			continue
		}
		anchor, isDisputed := anchorForRoot[chainOf(idx)]
		if !isDisputed {
			continue
		}

		_, adjState, adjVerdict, found, err := s.findAdjudicationTask(ctx, tx, parentID, anchor)
		if err != nil {
			return nil, false, err
		}

		if found {
			// An adjudication task already exists for this exact disputed finding.
			if adjState != "done" {
				pending = true
				continue
			}
			if adjVerdict.Valid && adjVerdict.String == "approve" {
				// Overturned: binding for this finding only, this round and every
				// later round the same chain is carried forward under.
				excluded[i] = true
			}
			// verdict == "reject": upheld. Default (still_open) treatment stands.
			continue
		}

		// No adjudication task yet for this disputed finding: this is the round in
		// which the raising reviewer's re-evaluation reports it maintained.
		if reason := s.researchAdjudicatorUnavailableReason(parentReviewModels); reason != "" {
			noted, err := s.researchAdjudicationAlreadyNoted(ctx, tx, parentID, anchor)
			if err != nil {
				return nil, false, err
			}
			if !noted {
				note := fmt.Sprintf(
					"Adjudicator unavailable for disputed finding %s (raised round %d, reviewer lineage %s): %s. The finding stays blocking.",
					anchor.FindingID, anchor.Round, anchor.Lineage, reason,
				)
				if _, err := s.AppendEvent(ctx, tx, parentID, "system", "research_adjudication_unavailable", nil, &note); err != nil {
					return nil, false, fmt.Errorf("failed to append adjudication-unavailable event: %w", err)
				}
			}
			// Not excluded: default (still_open) treatment stands, the finding blocks.
			continue
		}

		if !fetchedParent {
			if err := tx.QueryRowContext(ctx, `
				SELECT project_id, document_id, track FROM task WHERE id = ?
			`, parentID).Scan(&parentProjectID, &parentDocumentID, &parentTrack); err != nil {
				return nil, false, fmt.Errorf("failed to fetch parent for adjudication spawn: %w", err)
			}
			fetchedParent = true
		}
		if err := s.spawnAdjudicationTask(ctx, tx, parentID, parentProjectID, parentDocumentID, parentTrack, anchor, f, round, now); err != nil {
			return nil, false, err
		}
		pending = true
	}

	return excluded, pending, nil
}

// defaultReviewModels returns models, or ["opus"] when it is empty: the fallback
// SubmitTask applies when spawning a task's review tasks. Every later decision about
// who reviewed a round (e.g. the adjudicator-conflict check) must apply the same
// default, or an empty stored review_models makes that check pass vacuously.
func defaultReviewModels(models []string) []string {
	if len(models) == 0 {
		return []string{"opus"}
	}
	return models
}

// researchAdjudicatorUnavailableReason reports why docs/features/research-track.md
// section 5 adjudication cannot run for this task, or "" if it can: the adjudicator is
// unconfigured, not in the model allowlist (defense in depth; ODONIAN_RESEARCH_ADJUDICATOR
// is already validated against the allowlist at startup), or equal to one of the
// task's two configured reviewers (decision 2: the adjudicator must differ from both).
func (s *sqliteStore) researchAdjudicatorUnavailableReason(parentReviewModels []string) string {
	if s.researchAdjudicator == "" {
		return "ODONIAN_RESEARCH_ADJUDICATOR is not configured"
	}
	if !s.allowedModelsM[s.researchAdjudicator] {
		return fmt.Sprintf("configured adjudicator %q is not in the model allowlist", s.researchAdjudicator)
	}
	for _, m := range parentReviewModels {
		if m == s.researchAdjudicator {
			return fmt.Sprintf("configured adjudicator %q is one of this task's reviewers", s.researchAdjudicator)
		}
	}
	return ""
}

// researchAdjudicationAlreadyNoted reports whether a research_adjudication_unavailable
// event already exists on the parent for this exact dispute anchor, so a redundant
// aggregation call (e.g. from ReleaseTask) never appends a second one for the same
// still-undecided round.
func (s *sqliteStore) researchAdjudicationAlreadyNoted(ctx context.Context, tx *sql.Tx, parentID string, anchor Dispute) (bool, error) {
	marker := fmt.Sprintf("disputed finding %s (raised round %d, reviewer lineage %s)", anchor.FindingID, anchor.Round, anchor.Lineage)
	var count int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM event WHERE task_id = ? AND kind = 'research_adjudication_unavailable' AND note LIKE ?
	`, parentID, "%"+marker+"%").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check for existing adjudication-unavailable event: %w", err)
	}
	return count > 0, nil
}

// adjudicationTaskSpec renders the brief for a section 5 adjudication task, scoped to
// the one disputed finding it must rule on. Wording is intentionally plain: R12 owns
// the polished prompt; this only has to carry enough for a model to act on today.
func adjudicationTaskSpec(parentID string, finding Finding, evidence string) string {
	return fmt.Sprintf(
		"Adjudicate one disputed research review finding, per docs/features/research-track.md section 5.\n\n"+
			"Parent task: %s\n\n"+
			"## Disputed finding\n\n"+
			"- id: %s\n- severity: %s\n- file: %s\n- line: %d\n- status: %s\n- summary: %s\n\n"+
			"## Worker's evidence disputing the finding\n\n%s\n\n"+
			"## Instructions\n\n"+
			"The finding's raising reviewer re-evaluated the worker's evidence and maintained the finding. "+
			"Independently verify the finding against the cited source and the worker's evidence. "+
			"Your ruling is binding for this finding only; it does not vote on the review round.\n\n"+
			"Submit verdict \"approve\" if the finding should be OVERTURNED (the evidence resolves it; it must not block).\n"+
			"Submit verdict \"reject\" if the finding should be UPHELD (it remains a valid blocking finding).\n\n"+
			"This task does not use the structured findings format: submit with findings: [].",
		parentID, finding.ID, finding.Severity, finding.File, finding.Line, finding.Status, finding.Summary, evidence,
	)
}

// findAdjudicationTask looks up the (at most one, per the migration's unique index)
// adjudication task for anchor targeting parentID, splitting anchor.Lineage into its
// stored reviewer-model and reviewer-slot columns (see splitResearchReviewerLineage).
func (s *sqliteStore) findAdjudicationTask(ctx context.Context, tx *sql.Tx, parentID string, anchor Dispute) (taskID, state string, verdict sql.NullString, found bool, err error) {
	model, slot, ok := splitResearchReviewerLineage(anchor.Lineage)
	if !ok {
		return "", "", sql.NullString{}, false, fmt.Errorf("malformed dispute lineage %q", anchor.Lineage)
	}
	err = tx.QueryRowContext(ctx, `
		SELECT id, state, verdict FROM task
		WHERE target_task_id = ? AND adjudicate_finding_round = ? AND adjudicate_finding_reviewer_model = ?
		  AND adjudicate_finding_reviewer_slot = ? AND adjudicate_finding_id = ?
	`, parentID, anchor.Round, model, slot, anchor.FindingID).Scan(&taskID, &state, &verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", sql.NullString{}, false, nil
	}
	if err != nil {
		return "", "", sql.NullString{}, false, fmt.Errorf("failed to look up adjudication task: %w", err)
	}
	return taskID, state, verdict, true, nil
}

// spawnAdjudicationTask creates the one review-kind task that adjudicates anchor,
// scoped to that finding alone, assigned to s.researchAdjudicator. It is idempotent: a
// pre-existing task for the same (target_task_id, anchor) — from an earlier call in
// this same aggregation, a retried submission, or any other redundant call — is left
// alone rather than duplicated. Idempotency needs only the check-then-insert below,
// with the migration's unique index as a backstop: the store's single-writer
// connection (Open sets MaxOpenConns(1)) means no other transaction can be racing this
// one, so a real duplicate can only arise across separate, sequential transactions,
// which the pre-check already catches.
func (s *sqliteStore) spawnAdjudicationTask(ctx context.Context, tx *sql.Tx, parentID, parentProjectID, parentDocumentID, parentTrack string, anchor Dispute, finding Finding, round int, now string) error {
	_, _, _, found, err := s.findAdjudicationTask(ctx, tx, parentID, anchor)
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	model, slot, ok := splitResearchReviewerLineage(anchor.Lineage)
	if !ok {
		return fmt.Errorf("malformed dispute lineage %q", anchor.Lineage)
	}

	parentPriority := DefaultPriority
	if err := tx.QueryRowContext(ctx, `SELECT priority FROM task WHERE id = ?`, parentID).Scan(&parentPriority); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("failed to read parent priority: %w", err)
	}

	taskID := GenerateID()
	title := fmt.Sprintf("Adjudicate disputed finding %s [%s]", finding.ID, finding.Severity)
	spec := adjudicationTaskSpec(parentID, finding, anchor.Evidence)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO task (
			id, project_id, document_id, title, spec, state, model, kind, review_round, target_task_id,
			agent_merge, track, adjudicate_finding_round, adjudicate_finding_reviewer_model,
			adjudicate_finding_reviewer_slot, adjudicate_finding_id, priority, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, 'ready', ?, 'review', ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)
	`, taskID, parentProjectID, parentDocumentID, title, spec, s.researchAdjudicator, round, parentID,
		parentTrack, anchor.Round, model, slot, anchor.FindingID, parentPriority, now, now)
	if err != nil {
		return fmt.Errorf("failed to spawn adjudication task: %w", err)
	}

	eventNote := fmt.Sprintf("Adjudication task %s spawned for disputed finding %s (raised round %d, reviewer lineage %s)", taskID, anchor.FindingID, anchor.Round, anchor.Lineage)
	if _, err := s.AppendEvent(ctx, tx, parentID, "system", "research_adjudication_spawned", nil, &eventNote); err != nil {
		return fmt.Errorf("failed to append research_adjudication_spawned event: %w", err)
	}
	return nil
}

// appendResearchRoundRejectedEvent records that a research review round failed,
// with the blocking findings that failed it, per docs/features/research-track.md
// section 6. This is the single, authoritative record countChainWideRejectedRounds
// and describeChainWideBlockingFindings read: it is only ever appended at the moment
// aggregateReviewRound determines a round actually failed, so a round that never
// reaches that point (e.g. its task is superseded while the round is still in
// review, before every reviewer has submitted) is never counted, without needing to
// re-derive "was this round actually rejected" later from partial data.
func (s *sqliteStore) appendResearchRoundRejectedEvent(ctx context.Context, tx *sql.Tx, parentID string, round int, findings []Finding) error {
	note := fmt.Sprintf("Round %d rejected", round)
	if len(findings) == 0 {
		_, err := s.AppendEvent(ctx, tx, parentID, "system", "research_round_rejected", nil, &note)
		return err
	}
	data, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("failed to marshal round-rejected findings: %w", err)
	}
	_, err = s.AppendEvent(ctx, tx, parentID, "system", "research_round_rejected", nil, &note, json.RawMessage(data))
	return err
}

// supersedeChain walks a research task's supersede chain, oldest predecessor first
// and the given task last, by repeatedly finding the task whose superseded_by points
// at the current one (supersedeTaskTx sets superseded_by on the retired predecessor,
// pointing forward at its successor, so walking predecessors means querying
// backward from each task). The budget in docs/features/research-track.md section 6
// counts rejected rounds across this whole chain, so escalation or manual
// supersession can never reset it.
func (s *sqliteStore) supersedeChain(ctx context.Context, tx *sql.Tx, taskID string) ([]string, error) {
	chain := []string{taskID}
	current := taskID
	for {
		var predecessor string
		err := tx.QueryRowContext(ctx, `SELECT id FROM task WHERE superseded_by = ?`, current).Scan(&predecessor)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to find predecessor of %s: %w", current, err)
		}
		chain = append(chain, predecessor)
		current = predecessor
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// countChainWideRejectedRounds sums the research_round_rejected events recorded on
// every task in taskID's supersede chain (docs/features/research-track.md section
// 6), so the round budget counts across the whole chain and never resets on
// escalation or manual supersession.
func (s *sqliteStore) countChainWideRejectedRounds(ctx context.Context, tx *sql.Tx, taskID string) (int, error) {
	chain, err := s.supersedeChain(ctx, tx, taskID)
	if err != nil {
		return 0, err
	}
	var total int
	for _, id := range chain {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM event WHERE task_id = ? AND kind = 'research_round_rejected'
		`, id).Scan(&count); err != nil {
			return 0, fmt.Errorf("failed to count rejected rounds for %s: %w", id, err)
		}
		total += count
	}
	return total, nil
}

// describeOutstandingResearchRoundFindings renders one rejected round's findings for
// the decompose note, or a placeholder when the round failed closed with nothing
// valid to report (docs/features/research-track.md section 6: "the block event lists
// the blocking findings from each round"). A stored finding whose lineage chain was
// later reported resolved, per findOutstanding, is dropped: the note should only
// carry what's still unresolved so the owner can decompose around the real gap, not
// findings the worker already fixed in a later round. findOutstanding matches a
// stored finding back to its live report to look up that status; a lookup miss
// (which should not normally happen, since every stored finding was itself once a
// parsed review report) keeps the finding, so a matching failure can never hide a
// real blocker.
func describeOutstandingResearchRoundFindings(findingsText sql.NullString, round int, findOutstanding func(int, Finding) (matched, outstanding bool)) string {
	if !findingsText.Valid || findingsText.String == "" {
		return "review data incomplete or malformed"
	}
	var findings []Finding
	if err := json.Unmarshal([]byte(findingsText.String), &findings); err != nil || len(findings) == 0 {
		return "review data incomplete or malformed"
	}
	kept := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if matched, outstanding := findOutstanding(round, f); !matched || outstanding {
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 {
		return "all findings resolved in a later round"
	}
	parts := make([]string, 0, len(kept))
	for _, f := range kept {
		parts = append(parts, fmt.Sprintf("%s[%s:%d]: %s", f.Severity, f.File, f.Line, f.Summary))
	}
	return strings.Join(parts, "; ")
}

// describeChainWideBlockingFindings renders the still-unresolved blocking findings
// from every rejected round across taskID's supersede chain, oldest first, numbered
// chain-wide so the owner can see whether findings were shrinking or recurring across
// the whole history, not just the current task (docs/features/research-track.md
// section 6). A finding that failed a round but was later reported resolved by the
// same reviewer lineage, in a later round of the same task, is left out: it re-derives
// resolution status from every review report on each task, the same way
// createResearchFollowUpTasks does for non-blocking findings, since the
// research_round_rejected snapshot only ever holds a round's blocking findings at the
// moment it failed and never learns about a later round's resolution. Resolution is
// reconciled per task only: a blocker raised on a predecessor and resolved on its
// successor still appears in the note. That is acceptable for this milestone, since
// prior_id lineage is per task and R9 spec compaction carries unresolved findings
// forward across supersession.
func (s *sqliteStore) describeChainWideBlockingFindings(ctx context.Context, tx *sql.Tx, taskID string) (string, error) {
	chain, err := s.supersedeChain(ctx, tx, taskID)
	if err != nil {
		return "", err
	}
	var rounds []string
	chainRound := 0
	for _, id := range chain {
		var maxRound int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(review_round), 0) FROM task WHERE target_task_id = ? AND kind = 'review'
		`, id).Scan(&maxRound); err != nil {
			return "", fmt.Errorf("failed to find max review round for %s: %w", id, err)
		}
		allFindings, err := s.collectResearchReviewFindings(ctx, tx, id, maxRound)
		if err != nil {
			return "", fmt.Errorf("failed to collect review findings for %s: %w", id, err)
		}
		_, isOutstanding := researchFindingChains(allFindings)

		// Matches a finding stored in a research_round_rejected event back to its
		// live report at the same round, by exact field equality (the stored copy is
		// a direct serialization of that report, so every field matches). used[]
		// prevents two identical stored findings from both matching the same report.
		used := make([]bool, len(allFindings))
		findOutstanding := func(round int, f Finding) (matched, outstanding bool) {
			for i, cf := range allFindings {
				if used[i] || cf.round != round {
					continue
				}
				if cf.ID == f.ID && cf.Severity == f.Severity && cf.File == f.File && cf.Line == f.Line &&
					cf.Summary == f.Summary && cf.InChangedText == f.InChangedText && cf.Status == f.Status {
					used[i] = true
					return true, isOutstanding(i)
				}
			}
			return false, false
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT note, findings FROM event
			WHERE task_id = ? AND kind = 'research_round_rejected'
			ORDER BY created_at, id
		`, id)
		if err != nil {
			return "", fmt.Errorf("failed to query rejected-round events for %s: %w", id, err)
		}
		// appendResearchRoundRejectedEvent stamps each event's note with the task's
		// real review_round at the moment the round failed ("Round %d rejected"), so
		// that round is read back from the note rather than inferred from the event's
		// position. Position alone is not reliable: TransitionTask allows a task in
		// review to be sent to blocked and back to ready, which lets a round be
		// abandoned (and resubmitted past) without ever completing aggregation, so the
		// Nth rejected event is not always local round N.
		for rows.Next() {
			var noteText sql.NullString
			var findingsText sql.NullString
			if err := rows.Scan(&noteText, &findingsText); err != nil {
				rows.Close()
				return "", fmt.Errorf("failed to scan rejected-round event: %w", err)
			}
			var localRound int
			if noteText.Valid {
				fmt.Sscanf(noteText.String, "Round %d rejected", &localRound)
			}
			chainRound++
			rounds = append(rounds, fmt.Sprintf("Round %d: %s", chainRound, describeOutstandingResearchRoundFindings(findingsText, localRound, findOutstanding)))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", fmt.Errorf("failed to iterate rejected-round events for %s: %w", id, err)
		}
		rows.Close()
	}
	return strings.Join(rounds, " | "), nil
}

// applyRoundCircuitBreaker is the per-tier round-count circuit breaker shared by
// build/design (verdict rejection) and research (blocking finding): once the
// model's tier threshold is exceeded, escalate to the next tier if escalation is
// enabled and a next tier exists, otherwise block. For research tasks, this only
// runs once docs/features/research-track.md section 6's chain-wide round budget has
// not already decided the round (the budget always takes precedence). Returns the
// new parent state ("ready", "blocked", or "" if the parent was just superseded via
// escalation, in which case the caller must not update the now-superseded parent's
// own state).
func (s *sqliteStore) applyRoundCircuitBreaker(ctx context.Context, tx *sql.Tx, parentID, parentTrack, parentModel string, parentEscalate bool, parentReviewRound, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationLadder []string, researchEscalationThresholds map[string]int, now string) (string, error) {
	var threshold int
	var isTopTier bool
	var nextModel string
	var hasNextTier bool

	if parentTrack == "research" {
		threshold = researchThresholdFor(parentModel, researchEscalationLadder, researchEscalationThresholds, maxReviewRounds)
		isTopTier = s.isResearchTopTier(parentModel)
		if parentEscalate && !isTopTier {
			nextModel, hasNextTier = s.researchNextTier(parentModel)
		}
	} else {
		threshold = thresholdFor(parentModel, escalationThresholds, maxReviewRounds)
		isTopTier = s.isTopTier(parentModel)
		if parentEscalate && !isTopTier {
			nextModel, hasNextTier = s.nextTier(parentModel)
		}
	}

	if parentReviewRound <= threshold {
		return "ready", nil
	}

	if !(parentEscalate && !isTopTier && hasNextTier) {
		return "blocked", nil
	}

	escalatedTaskID, err := s.supersedeTaskTx(ctx, tx, parentID, &nextModel)
	if err != nil {
		return "", fmt.Errorf("failed to escalate task: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE task
		SET state='ready', updated_at=?
		WHERE id=?
	`, now, escalatedTaskID)
	if err != nil {
		return "", fmt.Errorf("failed to promote escalated task: %w", err)
	}
	escalationNote := "backlog->ready (auto-promoted via escalation)"
	if _, err := s.AppendEvent(ctx, tx, escalatedTaskID, "system", "transition", nil, &escalationNote); err != nil {
		return "", fmt.Errorf("failed to append escalation transition event: %w", err)
	}
	eventNote := fmt.Sprintf("%s→%s round %d, superseded by %s", parentModel, nextModel, parentReviewRound, escalatedTaskID)
	if _, err := s.AppendEvent(ctx, tx, parentID, "system", "escalation", nil, &eventNote); err != nil {
		return "", fmt.Errorf("failed to append escalation event: %w", err)
	}
	return "", nil
}

// researchFindingDedupKey is the structured, parent-scoped identity of a non-blocking
// research finding, persisted as a task_link value to make follow-up creation
// idempotent across reviewers, rounds and repeated aggregation (docs/features/
// research-track.md section 4). It is JSON-encoded rather than joined with a
// delimiter: a delimiter like ":" is ambiguous when a file path or summary contains
// it, so two distinct findings could otherwise collide onto the same key. JSON
// escaping keeps each field's boundary unambiguous.
type researchFindingDedupKey struct {
	ParentID string `json:"parent_id"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Summary  string `json:"summary"`
}

// researchFindingSourceValue links a follow-up task's research_finding_source
// task_link back to the specific review task and finding id it was raised on.
type researchFindingSourceValue struct {
	ReviewTaskID string `json:"review_task_id"`
	FindingID    string `json:"finding_id"`
}

// researchFindingIdentity groups findings describing the same underlying defect: same
// file, line and summary text. It's only ever compared in memory, so a plain
// comparable struct works as a map key without any string encoding.
type researchFindingIdentity struct {
	File    string
	Line    int
	Summary string
}

// researchCollectedFinding is a Finding annotated with which review task and round it
// came from, so createResearchFollowUpTasks can attribute it to a reviewer and decide
// whether it's still outstanding.
type researchCollectedFinding struct {
	Finding
	round         int
	reviewTaskID  string
	reviewerModel string
	reviewerSlot  int
	lineage       string
}

// researchReviewerLineage names one reviewer across rounds and across a supersede
// chain: its model plus its slot among that model's review tasks in a round (see
// collectResearchReviewReports). Replacements copy review_models, so the same lineage
// names the same reviewer on a predecessor and its replacement.
func researchReviewerLineage(model string, slot int) string {
	return fmt.Sprintf("%s\x00%d", model, slot)
}

// splitResearchReviewerLineage reverses researchReviewerLineage, for the one place
// (spawnAdjudicationTask) that must persist a reviewer identity as SQL columns rather
// than compare it in memory: task.adjudicate_finding_reviewer_model/_slot are stored
// separately, rather than as a single "model\x00slot" TEXT value, so the row never
// holds a value with an embedded NUL byte.
func splitResearchReviewerLineage(lineage string) (model string, slot int, ok bool) {
	idx := strings.IndexByte(lineage, 0)
	if idx < 0 {
		return "", 0, false
	}
	model = lineage[:idx]
	slot, err := strconv.Atoi(lineage[idx+1:])
	if err != nil {
		return "", 0, false
	}
	return model, slot, true
}

// newResearchUnionFind returns a union-find over n indices into an allFindings slice,
// shared by researchFindingChains (linking prior_id lineages) and
// createResearchFollowUpTasks (grouping outstanding reports into one follow-up),
// which each need their own independent partition of the same indices.
func newResearchUnionFind(n int) (find func(int) int, union func(int, int)) {
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union = func(a, b int) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[ra] = rb
		}
	}
	return find, union
}

// collectResearchReviewFindings gathers every finding reported on parentID's own
// review tasks through round throughRound, tagged with the round, review task and
// reviewer lineage it came from, sorted by round then review task then finding id.
// Shared by createResearchFollowUpTasks, which groups non-blocking findings into
// follow-ups, and describeChainWideBlockingFindings, which uses it to tell whether a
// round's blocking finding was later reported resolved.
func (s *sqliteStore) collectResearchReviewFindings(ctx context.Context, tx *sql.Tx, parentID string, throughRound int) ([]researchCollectedFinding, error) {
	allFindings, _, err := s.collectResearchReviewReports(ctx, tx, parentID, throughRound)
	return allFindings, err
}

// collectResearchReviewReports is collectResearchReviewFindings plus, for each
// reviewer lineage, the latest round in which that lineage actually submitted a
// review (with or without findings). Supersession compaction needs the latter to tell
// a reviewer who re-reviewed and reported nothing outstanding apart from one whose
// review task is still pending.
func (s *sqliteStore) collectResearchReviewReports(ctx context.Context, tx *sql.Tx, parentID string, throughRound int) ([]researchCollectedFinding, map[string]int, error) {
	if throughRound < 1 {
		return nil, nil, nil
	}

	// Every review task this parent has had, across every round so far, with the
	// round it belongs to and the model that reviewed it (recorded as the follow-up's
	// raising reviewer). Ordered by rowid, i.e. creation order, which is what
	// assigns each review task its reviewer slot below.
	// adjudicate_finding_id IS NULL excludes section 5 adjudication tasks: their
	// ruling is binding only for the one finding they adjudicate, so they must never
	// be assigned a reviewer slot or contribute to a reviewer lineage's findings here.
	taskRows, err := tx.QueryContext(ctx, `
		SELECT id, review_round, model FROM task
		WHERE target_task_id = ? AND kind = 'review' AND review_round BETWEEN 1 AND ? AND adjudicate_finding_id IS NULL
		ORDER BY review_round, rowid, id
	`, parentID, throughRound)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list parent's review tasks: %w", err)
	}
	type reviewTaskInfo struct {
		round   int
		model   string
		slot    int
		lineage string
	}
	reviewTasks := make(map[string]reviewTaskInfo)
	slotsTaken := make(map[string]int)
	for taskRows.Next() {
		var id, model string
		var round int
		if err := taskRows.Scan(&id, &round, &model); err != nil {
			taskRows.Close()
			return nil, nil, fmt.Errorf("failed to scan review task: %w", err)
		}
		// A reviewer's lineage is its model plus its slot: the index of its review
		// task among the same round's review tasks for that model, in creation
		// order. Review tasks are spawned by iterating review_models in order, so
		// the same slot names the same reviewer in every round, even when
		// review_models lists a model more than once. Finding ids (and so
		// prior_id) are only meaningful within one reviewer's lineage. This assumes
		// review_models is unchanged between rounds and each round spawns exactly one
		// review task per entry; otherwise slots shift and lineages mismatch.
		slotKey := fmt.Sprintf("%d\x00%s", round, model)
		slot := slotsTaken[slotKey]
		slotsTaken[slotKey] = slot + 1
		reviewTasks[id] = reviewTaskInfo{round: round, model: model, slot: slot, lineage: researchReviewerLineage(model, slot)}
	}
	if err := taskRows.Err(); err != nil {
		taskRows.Close()
		return nil, nil, fmt.Errorf("failed to iterate review tasks: %w", err)
	}
	taskRows.Close()

	if len(reviewTasks) == 0 {
		return nil, nil, nil
	}

	// Each review task's own findings, from the review event it produced. Matches
	// checkResearchBlockingFindings: keyed by source_task_id, first matching event
	// wins, so an intervening AddReview (or other) event on the parent can't push a
	// review task's own event out of the window scanned.
	evRows, err := tx.QueryContext(ctx, `
		SELECT source_task_id, findings FROM event
		WHERE task_id = ? AND kind = 'review' AND source_task_id IS NOT NULL
	`, parentID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query review events: %w", err)
	}
	var allFindings []researchCollectedFinding
	seen := make(map[string]bool, len(reviewTasks))
	latestSubmittedRound := make(map[string]int)
	for evRows.Next() {
		var sourceTaskID string
		var findingsText sql.NullString
		if err := evRows.Scan(&sourceTaskID, &findingsText); err != nil {
			evRows.Close()
			return nil, nil, fmt.Errorf("failed to scan review event: %w", err)
		}
		info, ok := reviewTasks[sourceTaskID]
		if !ok {
			continue
		}
		// Any review event from the review task means its reviewer submitted this
		// round, even one that carries no findings array.
		if info.round > latestSubmittedRound[info.lineage] {
			latestSubmittedRound[info.lineage] = info.round
		}
		if seen[sourceTaskID] || !findingsText.Valid {
			continue
		}
		seen[sourceTaskID] = true
		var findings []Finding
		if err := json.Unmarshal([]byte(findingsText.String), &findings); err != nil {
			continue
		}
		for _, f := range findings {
			allFindings = append(allFindings, researchCollectedFinding{Finding: f, round: info.round, reviewTaskID: sourceTaskID, reviewerModel: info.model, reviewerSlot: info.slot, lineage: info.lineage})
		}
	}
	if err := evRows.Err(); err != nil {
		evRows.Close()
		return nil, nil, fmt.Errorf("failed to iterate review events: %w", err)
	}
	evRows.Close()

	// Sort for deterministic candidate selection: by round, then review task, then
	// finding id.
	sort.Slice(allFindings, func(i, j int) bool {
		if allFindings[i].round != allFindings[j].round {
			return allFindings[i].round < allFindings[j].round
		}
		if allFindings[i].reviewTaskID != allFindings[j].reviewTaskID {
			return allFindings[i].reviewTaskID < allFindings[j].reviewTaskID
		}
		return allFindings[i].ID < allFindings[j].ID
	})

	return allFindings, latestSubmittedRound, nil
}

// researchFindingChains links each report in allFindings, which must already be
// sorted by round, into its prior_id lineage chain within one reviewer, per
// docs/features/research-track.md section 3. It returns chainOf, the root index of a
// report's lineage chain, and isOutstanding, a predicate reporting whether a given
// report index is still outstanding: not itself resolved, and not settled by a later
// resolved report in its chain.
func researchFindingChains(allFindings []researchCollectedFinding) (chainOf func(int) int, isOutstanding func(int) bool) {
	// Chains: a finding carried forward across rounds as still_open, reworded each
	// time, and finally resolved, is one chain of reports linked by prior_id within
	// one reviewer lineage. Ids are only unique within a single submission (section
	// 3 asks for ids "unique within the task", but validation only enforces
	// uniqueness within one findings array), so a prior_id resolves against the
	// latest instance of that id from a strictly earlier round, never against a
	// report in the same round's submission (reviewers commonly renumber each
	// round, so a same-round id can collide with the prior_id) and never against
	// the current report itself, if it names its own id as prior_id.
	chainOf, linkChain := newResearchUnionFind(len(allFindings))
	latestIndexByLineageAndID := make(map[string]int, len(allFindings))
	for start := 0; start < len(allFindings); {
		// allFindings is sorted by round: [start, end) is one round's reports.
		end := start
		for end < len(allFindings) && allFindings[end].round == allFindings[start].round {
			end++
		}
		for i := start; i < end; i++ {
			if cf := allFindings[i]; cf.PriorID != nil {
				if prior, ok := latestIndexByLineageAndID[cf.lineage+"\x00"+*cf.PriorID]; ok {
					linkChain(i, prior)
				}
			}
		}
		for i := start; i < end; i++ {
			latestIndexByLineageAndID[allFindings[i].lineage+"\x00"+allFindings[i].ID] = i
		}
		start = end
	}

	// A resolved report settles its chain up to and including its round. Only
	// reports from later rounds (a chain re-opened after resolution) are still
	// outstanding. Resolution is per chain, not per identity: a fresh report that
	// happens to repeat a resolved finding's file, line and summary, without a
	// prior_id into that chain, is a new finding and still outstanding.
	lastResolvedRound := make(map[int]int)
	for i, cf := range allFindings {
		if cf.Status == "resolved" {
			if root := chainOf(i); cf.round > lastResolvedRound[root] {
				lastResolvedRound[root] = cf.round
			}
		}
	}
	isOutstanding = func(i int) bool {
		cf := allFindings[i]
		return cf.Status != "resolved" && cf.round > lastResolvedRound[chainOf(i)]
	}
	return chainOf, isOutstanding
}

// createResearchFollowUpTasks implements docs/features/research-track.md section 4:
// one backlog follow-up task per non-blocking research finding, deduplicated by file,
// line and summary across reviewers, rounds and repeated aggregation. It is called
// whenever a research-track parent's review round passes, and looks across every
// round up to and including the current one: a non-blocking finding raised in an
// earlier round that failed for an unrelated reason, and never resolved, still needs
// a follow-up once the parent is finally approved — it would otherwise be silently
// dropped once that round's review events stop being the ones aggregation inspects.
//
// Returns only the IDs of follow-ups newly created by this call. Findings that
// already have a follow-up (from an earlier aggregation call) are matched via the
// research_finding_dedup task_link and skipped, so the caller can update the parent's
// result and event exactly once per new follow-up and repeated aggregation stays
// idempotent.
func (s *sqliteStore) createResearchFollowUpTasks(ctx context.Context, tx *sql.Tx, parentID, parentProjectID, parentDocumentID string, throughRound int, now string) ([]string, error) {
	allFindings, err := s.collectResearchReviewFindings(ctx, tx, parentID, throughRound)
	if err != nil {
		return nil, err
	}
	if len(allFindings) == 0 {
		return nil, nil
	}
	chainOf, isOutstanding := researchFindingChains(allFindings)

	// Groups: outstanding reports of the same chain, plus outstanding reports with
	// identical file, line and summary (the same finding raised by several
	// reviewers, or in several rounds), describe one finding and yield at most one
	// follow-up.
	groupOf, linkGroup := newResearchUnionFind(len(allFindings))
	firstIndexByIdentity := make(map[researchFindingIdentity]int)
	for i, cf := range allFindings {
		if !isOutstanding(i) {
			continue
		}
		linkGroup(i, chainOf(i))
		identity := researchFindingIdentity{File: cf.File, Line: cf.Line, Summary: cf.Summary}
		if first, ok := firstIndexByIdentity[identity]; ok {
			linkGroup(i, first)
		} else {
			firstIndexByIdentity[identity] = i
		}
	}

	type followUpCandidate struct {
		finding   researchCollectedFinding
		members   []researchFindingIdentity
		reviewers []string
	}
	// allFindings is in chronological order, so the group's last non-blocking
	// report is its representative and the follow-up describes the finding's most
	// recent wording. A group whose severity changed across reports is still
	// represented by its last non-blocking report, not its latest report overall.
	candidatesByGroup := make(map[int]*followUpCandidate)
	for i, cf := range allFindings {
		if !isOutstanding(i) {
			continue
		}
		group := groupOf(i)
		candidate := candidatesByGroup[group]
		if candidate == nil {
			candidate = &followUpCandidate{}
			candidatesByGroup[group] = candidate
		}
		identity := researchFindingIdentity{File: cf.File, Line: cf.Line, Summary: cf.Summary}
		if !slices.Contains(candidate.members, identity) {
			candidate.members = append(candidate.members, identity)
		}
		// Non-blocking per section 3: P3 findings, and P1/P2 findings in unchanged
		// text after round 1.
		isP1OrP2 := cf.Severity == "P1" || cf.Severity == "P2"
		nonBlocking := cf.Severity == "P3" || (isP1OrP2 && !cf.InChangedText && cf.round > 1)
		if !nonBlocking {
			continue
		}
		candidate.finding = cf
		if !slices.Contains(candidate.reviewers, cf.reviewerModel) {
			candidate.reviewers = append(candidate.reviewers, cf.reviewerModel)
		}
	}

	candidates := make([]followUpCandidate, 0, len(candidatesByGroup))
	for _, candidate := range candidatesByGroup {
		if len(candidate.reviewers) == 0 {
			// No non-blocking report in this group.
			continue
		}
		sort.Strings(candidate.reviewers)
		candidates = append(candidates, *candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].finding, candidates[j].finding
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Summary < b.Summary
	})

	model := s.researchDefaultModel
	if model == "" {
		model = s.getDefaultModel()
	}

	var parentReviewModels []string
	var parentReviewModelsJSON *string
	var parentPriority int64
	err = tx.QueryRowContext(ctx, `SELECT review_models, priority FROM task WHERE id = ?`, parentID).Scan(&parentReviewModelsJSON, &parentPriority)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("failed to read parent review_models and priority: %w", err)
	}
	if err != nil {
		parentPriority = DefaultPriority
	}
	if parentReviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*parentReviewModelsJSON), &parentReviewModels); err != nil {
			return nil, fmt.Errorf("failed to unmarshal parent review_models: %w", err)
		}
	}

	var createdIDs []string
	for _, candidate := range candidates {
		cf := candidate.finding

		// A chain already has a follow-up if any identity in it was the one recorded
		// by an earlier aggregation call, even if the chain has since grown a newer
		// wording (so the representative, and its dedup key, changed).
		alreadyCreated := false
		for _, member := range candidate.members {
			memberKey, err := json.Marshal(researchFindingDedupKey{ParentID: parentID, File: member.File, Line: member.Line, Summary: member.Summary})
			if err != nil {
				return nil, fmt.Errorf("failed to marshal dedup key: %w", err)
			}
			var existingID string
			err = tx.QueryRowContext(ctx, `
				SELECT task_id FROM task_link
				WHERE kind = 'research_finding_dedup' AND value = ? AND tombstoned_at IS NULL
				LIMIT 1
			`, string(memberKey)).Scan(&existingID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("failed to check existing follow-up: %w", err)
			}
			if err == nil {
				alreadyCreated = true
				break
			}
		}
		if alreadyCreated {
			// Already created by an earlier aggregation call: reuse, don't recreate.
			continue
		}

		dedupValue, err := json.Marshal(researchFindingDedupKey{ParentID: parentID, File: cf.File, Line: cf.Line, Summary: cf.Summary})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal dedup key: %w", err)
		}

		followUpID := GenerateID()
		title := fmt.Sprintf("Research follow-up: %s (%s:%d)", cf.Severity, cf.File, cf.Line)
		spec := fmt.Sprintf(
			"Follow-up for a non-blocking research finding. Not a rewrite of the parent's assignment.\n\nSeverity: %s\nFile: %s\nLine: %d\nRaised by: %s\nParent task: %s\n\nFinding: %s\n",
			cf.Severity, cf.File, cf.Line, strings.Join(candidate.reviewers, ", "), parentID, cf.Summary,
		)

		var followUpReviewModelsJSON *string
		if len(parentReviewModels) > 0 {
			data, err := json.Marshal(parentReviewModels)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal follow-up review_models: %w", err)
			}
			str := string(data)
			followUpReviewModelsJSON = &str
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task (id, project_id, document_id, title, spec, state, model, kind, review_models, track, priority, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'backlog', ?, 'implement', ?, 'research', ?, ?, ?)
		`, followUpID, parentProjectID, parentDocumentID, title, spec, model, followUpReviewModelsJSON, DefaultPriority, now, now); err != nil {
			return nil, fmt.Errorf("failed to create follow-up task: %w", err)
		}

		// Linked to the parent via a task_link, not task.target_task_id: see the
		// migration 0016 comment for why target_task_id would corrupt the parent's own
		// review round tally once the follow-up has review rounds of its own.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_link (id, task_id, kind, value)
			VALUES (?, ?, 'research_parent', ?)
		`, GenerateID(), followUpID, parentID); err != nil {
			return nil, fmt.Errorf("failed to insert parent link: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_link (id, task_id, kind, value)
			VALUES (?, ?, 'research_finding_dedup', ?)
		`, GenerateID(), followUpID, string(dedupValue)); err != nil {
			return nil, fmt.Errorf("failed to insert dedup link: %w", err)
		}

		sourceValue, err := json.Marshal(researchFindingSourceValue{ReviewTaskID: cf.reviewTaskID, FindingID: cf.ID})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal source finding value: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_link (id, task_id, kind, value)
			VALUES (?, ?, 'research_finding_source', ?)
		`, GenerateID(), followUpID, string(sourceValue)); err != nil {
			return nil, fmt.Errorf("failed to insert source finding link: %w", err)
		}

		createdIDs = append(createdIDs, followUpID)
	}

	return createdIDs, nil
}

// InsertManifestChildren validates and inserts children from a manifest within a caller-owned
// transaction. Research children enter ready state; generated build/design children enter backlog.
// Deduplicates by (parentID, childKey, manifestDigest) to ensure idempotent calls with the same manifest.
// Returns created task IDs in manifest order. All-or-nothing: on any error, the transaction
// should be rolled back and nothing is created.
func (s *sqliteStore) InsertManifestChildren(ctx context.Context, tx *sql.Tx, m *manifest.Manifest, manifestDigest string, parentID, parentProjectID, parentDocumentID string, now string) ([]string, error) {
	// Validate the manifest against store rules (rejects nil and empty manifests too)
	if err := m.Validate(s.allowedModelsM, validTracks); err != nil {
		if validationErr, ok := err.(manifest.ValidationError); ok {
			return nil, invalid(validationErr.Code, validationErr.Message)
		}
		return nil, fmt.Errorf("manifest validation failed: %w", err)
	}

	// Validate that manifest's parent_task_id matches the provided parentID
	if m.ParentTaskID != parentID {
		return nil, invalid("MISMATCHED_PARENT_ID", fmt.Sprintf("manifest parent_task_id %q does not match provided parentID %q", m.ParentTaskID, parentID))
	}

	// Load parent task to verify it exists and belongs to the correct project/document
	var actualProjectID, actualDocumentID string
	var parentPriority int64
	err := tx.QueryRowContext(ctx, `
		SELECT project_id, document_id, priority FROM task WHERE id = ?
	`, parentID).Scan(&actualProjectID, &actualDocumentID, &parentPriority)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, invalid("PARENT_NOT_FOUND", fmt.Sprintf("parent task %q not found", parentID))
		}
		return nil, fmt.Errorf("failed to load parent task: %w", err)
	}

	// Verify the parent belongs to the expected project and document
	if actualProjectID != parentProjectID || actualDocumentID != parentDocumentID {
		return nil, invalid("MISMATCHED_PARENT_PROJECT_DOCUMENT", fmt.Sprintf("parent task %q belongs to project %q document %q, not %q %q", parentID, actualProjectID, actualDocumentID, parentProjectID, parentDocumentID))
	}

	// Deduplicate check: for each child, see if we've already created it from this exact manifest.
	// If so, reuse it. Only skip already-created children if they're from the same manifest digest.
	dedupKey := func(childKey string) (string, error) {
		key := map[string]interface{}{
			"parent_id":       parentID,
			"child_key":       childKey,
			"manifest_digest": manifestDigest,
		}
		data, err := json.Marshal(key)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	keyToID := make(map[string]string)
	createdTasks := make([]string, 0, len(m.Children))
	newChildKeys := make(map[string]bool)

	// First pass: check for already-created children and generate IDs for new ones
	for _, child := range m.Children {
		dupKey, err := dedupKey(child.Key)
		if err != nil {
			return nil, fmt.Errorf("failed to build dedup key: %w", err)
		}

		// Check if this child was already created from this exact manifest
		var existingID string
		err = tx.QueryRowContext(ctx, `
			SELECT task_id FROM task_link
			WHERE kind = 'continuation_child_dedup' AND value = ? AND tombstoned_at IS NULL
			LIMIT 1
		`, dupKey).Scan(&existingID)
		if err == nil {
			// Already created from this manifest
			keyToID[child.Key] = existingID
			createdTasks = append(createdTasks, existingID)
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("failed to check existing child: %w", err)
		}

		// New child: generate ID
		taskID := GenerateID()
		keyToID[child.Key] = taskID
		createdTasks = append(createdTasks, taskID)
		newChildKeys[child.Key] = true
	}

	// Second pass: insert new children (those not already present from this manifest)
	for _, child := range m.Children {
		// Skip if already created
		if !newChildKeys[child.Key] {
			continue
		}

		taskID := keyToID[child.Key]

		dupKey, err := dedupKey(child.Key)
		if err != nil {
			return nil, fmt.Errorf("failed to build dedup key: %w", err)
		}

		// Sanitize free text
		child.Title = sanitizeFreeText(child.Title)
		child.Spec = sanitizeFreeText(child.Spec)

		// Validate title and spec are non-empty
		if strings.TrimSpace(child.Title) == "" {
			return nil, invalid("EMPTY_TITLE", "child title is required")
		}
		if strings.TrimSpace(child.Spec) == "" {
			return nil, invalid("EMPTY_SPEC", "child spec is required")
		}

		// Resolve model
		model := child.Model
		if model == "" {
			if child.Track == "research" && s.researchDefaultModel != "" {
				model = s.researchDefaultModel
			} else {
				model = s.getDefaultModel()
			}
		}

		// Validate model is in allowlist (should already be validated by manifest.Validate, but double-check)
		if !s.allowedModelsM[model] {
			return nil, invalid("UNKNOWN_MODEL", fmt.Sprintf("unknown model: %s", model))
		}

		// Validate review_models
		for _, reviewModel := range child.ReviewModels {
			if !s.allowedModelsM[reviewModel] {
				return nil, invalid("UNKNOWN_MODEL", fmt.Sprintf("unknown review model: %s", reviewModel))
			}
		}

		// Encode review_models to JSON
		var reviewModelsJSON *string
		if len(child.ReviewModels) > 0 {
			data, err := json.Marshal(child.ReviewModels)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal review_models: %w", err)
			}
			str := string(data)
			reviewModelsJSON = &str
		}

		state := continuationChildInitialState(child.Track)

		// Resolve defaults for agent_merge and escalate
		agentMerge := false
		if child.AgentMerge != nil {
			agentMerge = *child.AgentMerge
		}

		escalate := true
		if child.Escalate != nil {
			escalate = *child.Escalate
		}

		// Insert the child task
		_, err = tx.ExecContext(ctx, `
			INSERT INTO task (id, project_id, document_id, title, spec, state, model, kind, review_models, review_round, agent_merge, escalate, track, priority, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'implement', ?, 0, ?, ?, ?, ?, ?, ?)
		`, taskID, parentProjectID, parentDocumentID, child.Title, child.Spec, state, model, reviewModelsJSON, agentMerge, escalate, child.Track, parentPriority, now, now)
		if err != nil {
			return nil, fmt.Errorf("failed to insert child task: %w", err)
		}

		// Add dedup link to prevent re-creation with the same manifest
		_, err = tx.ExecContext(ctx, `
			INSERT INTO task_link (id, task_id, kind, value)
			VALUES (?, ?, 'continuation_child_dedup', ?)
		`, GenerateID(), taskID, dupKey)
		if err != nil {
			return nil, fmt.Errorf("failed to insert dedup link: %w", err)
		}

		// Add parent link
		_, err = tx.ExecContext(ctx, `
			INSERT INTO task_link (id, task_id, kind, value)
			VALUES (?, ?, 'continuation_parent', ?)
		`, GenerateID(), taskID, parentID)
		if err != nil {
			return nil, fmt.Errorf("failed to insert parent link: %w", err)
		}

		// Store child metadata as JSON links for provenance
		if len(child.ClaimIDs) > 0 {
			data, err := json.Marshal(child.ClaimIDs)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal claim_ids: %w", err)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO task_link (id, task_id, kind, value)
				VALUES (?, ?, 'continuation_child_claim_ids', ?)
			`, GenerateID(), taskID, string(data))
			if err != nil {
				return nil, fmt.Errorf("failed to insert claim_ids link: %w", err)
			}
		}

		if len(child.SourceStartPoints) > 0 {
			data, err := json.Marshal(child.SourceStartPoints)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal source_start_points: %w", err)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO task_link (id, task_id, kind, value)
				VALUES (?, ?, 'continuation_child_source_start_points', ?)
			`, GenerateID(), taskID, string(data))
			if err != nil {
				return nil, fmt.Errorf("failed to insert source_start_points link: %w", err)
			}
		}

		if len(child.FileScope) > 0 {
			data, err := json.Marshal(child.FileScope)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal file_scope: %w", err)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO task_link (id, task_id, kind, value)
				VALUES (?, ?, 'continuation_child_file_scope', ?)
			`, GenerateID(), taskID, string(data))
			if err != nil {
				return nil, fmt.Errorf("failed to insert file_scope link: %w", err)
			}
		}

		if len(child.AcceptanceCriteria) > 0 {
			data, err := json.Marshal(child.AcceptanceCriteria)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal acceptance_criteria: %w", err)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO task_link (id, task_id, kind, value)
				VALUES (?, ?, 'continuation_child_acceptance_criteria', ?)
			`, GenerateID(), taskID, string(data))
			if err != nil {
				return nil, fmt.Errorf("failed to insert acceptance_criteria link: %w", err)
			}
		}
	}

	// Third pass: insert task dependencies (after all child IDs are known)
	// Only insert dependencies for newly created children to maintain idempotency
	for _, child := range m.Children {
		if len(child.Dependencies) == 0 {
			continue
		}

		// Skip dependencies for already-existing children (idempotency)
		if !newChildKeys[child.Key] {
			continue
		}

		taskID := keyToID[child.Key]

		for _, dep := range child.Dependencies {
			var dependsOnID string

			switch dep.Kind {
			case manifest.DependencyParent:
				// Dependency on the parent task
				if dep.Ref != m.ParentTaskID {
					return nil, invalid("MISMATCHED_PARENT_DEPENDENCY", "parent dependency ref must match manifest parent_task_id")
				}
				dependsOnID = parentID

			case manifest.DependencyChild:
				// Dependency on another child in this manifest (by key)
				id, exists := keyToID[dep.Ref]
				if !exists {
					return nil, invalid("UNKNOWN_CHILD_DEPENDENCY", fmt.Sprintf("child dependency references unknown child key %q", dep.Ref))
				}
				dependsOnID = id

			case manifest.DependencyTask:
				// Dependency on an external task by ID
				// Verify the task exists and is in the same project
				var existingProjectID string
				err := tx.QueryRowContext(ctx, "SELECT project_id FROM task WHERE id = ?", dep.Ref).Scan(&existingProjectID)
				if errors.Is(err, sql.ErrNoRows) {
					return nil, invalid("UNKNOWN_TASK_DEPENDENCY", fmt.Sprintf("task dependency references non-existent task %q", dep.Ref))
				}
				if err != nil {
					return nil, fmt.Errorf("failed to verify task dependency: %w", err)
				}
				if existingProjectID != parentProjectID {
					return nil, invalid("DEPENDENCY_NOT_IN_PROJECT", "task dependency references a task in another project")
				}
				dependsOnID = dep.Ref

			default:
				return nil, invalid("UNKNOWN_DEPENDENCY_KIND", fmt.Sprintf("unknown dependency kind: %s", dep.Kind))
			}

			// Insert dependency edge
			_, err := tx.ExecContext(ctx, `
				INSERT INTO task_dep (task_id, depends_on_id)
				VALUES (?, ?)
			`, taskID, dependsOnID)
			if err != nil {
				return nil, fmt.Errorf("failed to insert task dependency: %w", err)
			}
		}
	}

	return createdTasks, nil
}

// aggregateReviewRound tallies all review tasks for a parent in the current round and determines the new parent state.
// It handles: verdict counting, merge task spawning (if approved with agent_merge),
// escalation (if rejected and threshold exceeded), and blocking. Returns the new parent state.
// A return value of "" means no state change needed. Caller must apply the returned state.
// All state updates and event appending happen within this function.
func (s *sqliteStore) aggregateReviewRound(ctx context.Context, tx *sql.Tx, parentID string, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationLadder []string, researchEscalationThresholds map[string]int, researchRoundBudget int) (string, error) {
	now := nowTimestamp()

	var parentReviewRound int
	var parentState string
	var parentHeld bool
	var parentModel string
	var parentEscalate bool
	var parentAgentMerge bool
	var parentProjectID string
	var parentDocumentID string
	var parentTitle string
	var parentTrack string
	var parentReviewModelsJSON *string
	var parentLandingRound *int
	err := tx.QueryRowContext(ctx, `
		SELECT review_round, state, held, model, escalate, agent_merge, project_id, document_id, title, track, review_models, landing_round FROM task WHERE id = ?
	`, parentID).Scan(&parentReviewRound, &parentState, &parentHeld, &parentModel, &parentEscalate, &parentAgentMerge, &parentProjectID, &parentDocumentID, &parentTitle, &parentTrack, &parentReviewModelsJSON, &parentLandingRound)
	if err != nil {
		return "", fmt.Errorf("failed to fetch parent task review_round: %w", err)
	}
	var parentReviewModels []string
	if parentReviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*parentReviewModelsJSON), &parentReviewModels); err != nil {
			return "", fmt.Errorf("failed to unmarshal parent review_models: %w", err)
		}
	}
	// Same default SubmitTask applied when it spawned this round's review tasks
	// (see defaultReviewModels): an empty review_models column means the round's
	// actual reviewer was "opus", not "no reviewers", so the adjudicator-conflict
	// check below must compare against the same default or it passes vacuously.
	parentReviewModels = defaultReviewModels(parentReviewModels)

	// If parent is held, skip auto-transition (hold is an operator lock that overrides auto-flow)
	if parentHeld {
		return "", nil
	}

	// Count total, done, and approve verdict review tasks for the parent in the current
	// round. adjudicate_finding_id IS NULL excludes section 5 adjudication tasks: their
	// ruling is binding only for the finding they adjudicate and must never vote on
	// (or otherwise count toward) the round itself.
	var totalReviewTasks int
	var doneReviewTasks int
	var approveReviewTasks int
	err = tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*) as total,
			SUM(CASE WHEN state='done' THEN 1 ELSE 0 END) as done,
			SUM(CASE WHEN state='done' AND verdict='approve' THEN 1 ELSE 0 END) as approve
		FROM task
		WHERE target_task_id = ? AND review_round = ? AND adjudicate_finding_id IS NULL
	`, parentID, parentReviewRound).Scan(&totalReviewTasks, &doneReviewTasks, &approveReviewTasks)
	if err != nil {
		return "", fmt.Errorf("failed to tally review tasks: %w", err)
	}

	// Guard: if parent is in a terminal state (failed/blocked/abandoned), do not resurrect it
	var newParentState string
	// researchBlockNote, when set, is the section 6 chain-wide-budget note that
	// overrides the generic "auto-blocked" transition note below.
	var researchBlockNote string
	isTerminal := parentState == "failed" || parentState == "blocked" || parentState == "abandoned"

	if !isTerminal && doneReviewTasks == totalReviewTasks {
		// Determine whether the round failed. Build and design keep the existing
		// verdict tally. Research aggregation instead uses the structured findings
		// from section 3: a round fails if any reviewer reported a blocking finding,
		// regardless of that reviewer's approve/reject verdict.
		var roundFailed bool
		var researchRoundFindings []Finding
		if parentTrack == "research" {
			hasBlocking, findings, lineages, err := s.checkResearchBlockingFindings(ctx, tx, parentID, parentReviewRound)
			if err != nil {
				return "", err
			}

			// Section 5: a blocking finding that is the maintained continuation of a
			// worker-disputed finding is decided by adjudication, not by its raw
			// status. While any relevant adjudication is still pending, the round is
			// not decided at all: no rejected-round event, no circuit breaker, no
			// state change. This keeps a round from ever being decided twice, since
			// every prior call for this round returns here before recording anything.
			excluded, pendingAdjudication, err := s.applyResearchAdjudication(ctx, tx, parentID, parentReviewRound, parentReviewModels, findings, lineages, now)
			if err != nil {
				return "", err
			}
			if pendingAdjudication {
				return "", nil
			}
			if len(excluded) > 0 {
				filtered := findings[:0]
				for i, f := range findings {
					if !excluded[i] {
						filtered = append(filtered, f)
					}
				}
				findings = filtered
				hasBlocking = len(findings) > 0
			}

			roundFailed = hasBlocking
			researchRoundFindings = findings
		} else {
			roundFailed = approveReviewTasks < totalReviewTasks
		}

		if !roundFailed {
			state, err := s.handleApprovedRound(ctx, tx, parentID, parentReviewRound, parentModel, parentTrack, parentTitle, parentProjectID, parentDocumentID, parentAgentMerge, now)
			if err != nil {
				return "", err
			}
			newParentState = state

			// Section 4: when a research round passes, create one backlog follow-up
			// task per non-blocking finding (deduplicated). Only newly created
			// follow-ups update the parent's result and event, so repeated
			// aggregation for the same round stays idempotent.
			if parentTrack == "research" {
				createdIDs, err := s.createResearchFollowUpTasks(ctx, tx, parentID, parentProjectID, parentDocumentID, parentReviewRound, now)
				if err != nil {
					return "", err
				}
				if len(createdIDs) > 0 {
					var currentResult sql.NullString
					if err := tx.QueryRowContext(ctx, "SELECT result FROM task WHERE id = ?", parentID).Scan(&currentResult); err != nil {
						return "", fmt.Errorf("failed to fetch parent result: %w", err)
					}
					resultLine := fmt.Sprintf("Follow-up tasks created: %s", strings.Join(createdIDs, ", "))
					newResult := resultLine
					if currentResult.Valid && currentResult.String != "" {
						newResult = currentResult.String + "\n" + resultLine
					}
					if _, err := tx.ExecContext(ctx, "UPDATE task SET result = ?, updated_at = ? WHERE id = ?", newResult, now, parentID); err != nil {
						return "", fmt.Errorf("failed to update parent result with follow-ups: %w", err)
					}

					eventNote := fmt.Sprintf("Created %d follow-up task(s): %s", len(createdIDs), strings.Join(createdIDs, ", "))
					if _, err := s.AppendEvent(ctx, tx, parentID, "system", "follow_up_created", nil, &eventNote); err != nil {
						return "", fmt.Errorf("failed to append follow_up_created event: %w", err)
					}
				}
			}
		} else {
			// Section 6's chain-wide research round budget takes precedence over the
			// per-tier circuit breaker below: a rejected research round is always
			// recorded first, and if the chain-wide count has reached the budget, the
			// task blocks for decomposition regardless of tier or escalation.
			if parentTrack == "research" {
				if err := s.appendResearchRoundRejectedEvent(ctx, tx, parentID, parentReviewRound, researchRoundFindings); err != nil {
					return "", err
				}
				chainWideRejectedRounds, err := s.countChainWideRejectedRounds(ctx, tx, parentID)
				if err != nil {
					return "", err
				}
				if chainWideRejectedRounds >= researchRoundBudget {
					description, err := s.describeChainWideBlockingFindings(ctx, tx, parentID)
					if err != nil {
						return "", err
					}
					newParentState = "blocked"
					researchBlockNote = fmt.Sprintf("Chain-wide research round budget reached (%d/%d rejected rounds); reason decompose — %s", chainWideRejectedRounds, researchRoundBudget, description)
				}
			}

			if newParentState == "" {
				// Circuit breaker: if review_round > threshold, check escalation vs
				// blocking. Shared by build/design (verdict rejection) and research
				// (blocking finding), so the two paths can't drift.
				state, err := s.applyRoundCircuitBreaker(ctx, tx, parentID, parentTrack, parentModel, parentEscalate, parentReviewRound, maxReviewRounds, escalationThresholds, researchEscalationLadder, researchEscalationThresholds, now)
				if err != nil {
					return "", err
				}
				newParentState = state
			}
		}
	}

	// An approve is landing the parent's reviewed work: a late review result must not move it
	// (the landing reservation only lets it go to done).
	if parentLandingRound != nil {
		newParentState = ""
	}

	// Update parent state if needed (and not in terminal state)
	if newParentState != "" && newParentState != parentState && !isTerminal {
		_, err := tx.ExecContext(ctx, `
			UPDATE task SET state = ?, updated_at = ? WHERE id = ?
		`, newParentState, now, parentID)
		if err != nil {
			return "", fmt.Errorf("failed to update parent task state: %w", err)
		}

		// Append transition event for audit trail
		var eventNote string
		if newParentState == "approved" {
			eventNote = "Aggregation: all reviewers approved"
		} else if newParentState == "done" {
			eventNote = "auto-finalized: no-op task approved by all reviewers"
		} else if newParentState == "ready" {
			eventNote = "Aggregation: at least one reviewer rejected"
		} else if newParentState == "blocked" {
			if researchBlockNote != "" {
				eventNote = researchBlockNote
			} else {
				eventNote = fmt.Sprintf("auto-blocked: %d consecutive review rounds without approval — needs human attention", parentReviewRound)
			}
		}
		_, err = s.AppendEvent(ctx, tx, parentID, "system", "transition", nil, &eventNote)
		if err != nil {
			return "", fmt.Errorf("failed to append transition event: %w", err)
		}
	}

	return newParentState, nil
}

// AddReview records a review verdict event for a task.
// The task must be in 'review' state. Verdict must be 'approve' or 'reject'.
// Returns the created Event on success.
// Returns ErrNotFound if the task doesn't exist.
// Returns ErrConflict if the task is not in 'review' state.
// Returns ValidationError if verdict is invalid.
func (s *sqliteStore) AddReview(ctx context.Context, taskID, actor, verdict string, note *string) (Event, error) {
	// Validate verdict
	if verdict != "approve" && verdict != "reject" {
		return Event{}, invalid("INVALID_VERDICT", "verdict must be 'approve' or 'reject'")
	}

	// Strip raw control characters from the free-text note before storage.
	if note != nil {
		cleaned := sanitizeFreeText(*note)
		note = &cleaned
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Load the task's current state
	var taskState string
	err = tx.QueryRowContext(ctx, "SELECT state FROM task WHERE id = ?", taskID).Scan(&taskState)
	if err != nil {
		if err == sql.ErrNoRows {
			return Event{}, ErrNotFound
		}
		return Event{}, fmt.Errorf("failed to load task state: %w", err)
	}

	// Task must be in 'review' state
	if taskState != "review" {
		return Event{}, ErrConflict
	}

	// Append the review event
	verdictPtr := &verdict
	event, err := s.AppendEvent(ctx, tx, taskID, actor, "review", verdictPtr, note)
	if err != nil {
		return Event{}, err
	}

	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return event, nil
}

// TransitionTask moves a task to a new state according to the transition rules.
// Valid transitions:
//   - to='done': allowed ONLY from 'approved'
//   - to='ready': allowed from 'approved' or 'blocked' (blocked→ready clears stale assignee/lease)
//   - to='blocked': allowed from any ACTIVE state (backlog, ready, in_progress, review, approved)
//   - to='failed': allowed from any ACTIVE state (backlog, ready, in_progress, review, approved) or 'blocked' (retire a dead blocked task)
//   - to='superseded': allowed from any ACTIVE state (backlog, ready, in_progress, review, approved)
//   - to='abandoned': allowed ONLY from 'approved' (terminal state)
//   - anything else: ErrConflict
//
// Returns the updated Task on success.
// Returns ErrNotFound if the task doesn't exist.
// Returns ErrConflict if the transition is not allowed.
// Returns ValidationError if 'to' state is invalid.
func (s *sqliteStore) TransitionTask(ctx context.Context, taskID, to string, note *string) (Task, error) {
	return s.transitionTask(ctx, taskID, to, note, nil)
}

// CompleteLanding marks an approved task done for the approve attempt that holds its landing
// reservation, once that approve has published the reviewed work to the task's branch. It is the
// only way an approved task whose review round submitted a commit (a local_commit task) reaches
// done: TransitionTask refuses that, so dependents can never unblock before the work has landed.
// Returns NOT_RESERVED or LANDING_ATTEMPT_MISMATCH conflicts unless attempt owns the reservation.
func (s *sqliteStore) CompleteLanding(ctx context.Context, taskID, attempt string, note *string) (Task, error) {
	return s.transitionTask(ctx, taskID, "done", note, &attempt)
}

// transitionTask is TransitionTask, or with landingAttempt set, CompleteLanding.
func (s *sqliteStore) transitionTask(ctx context.Context, taskID, to string, note *string, landingAttempt *string) (Task, error) {
	// Validate 'to' state
	validTargets := map[string]bool{"done": true, "ready": true, "blocked": true, "failed": true, "superseded": true, "abandoned": true}
	if !validTargets[to] {
		return Task{}, invalid("INVALID_TARGET_STATE", "target state must be one of: done, ready, blocked, failed, superseded, abandoned")
	}

	// Strip raw control characters from the free-text note before storage.
	if note != nil {
		cleaned := sanitizeFreeText(*note)
		note = &cleaned
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Load current state and kind (kind gates the merge-task terminal transition)
	var taskState, taskKind string
	err = tx.QueryRowContext(ctx, "SELECT state, kind FROM task WHERE id = ?", taskID).Scan(&taskState, &taskKind)
	if err != nil {
		if err == sql.ErrNoRows {
			return Task{}, ErrNotFound
		}
		return Task{}, fmt.Errorf("failed to load task state: %w", err)
	}

	// Apply transition rules
	canTransition := false

	switch to {
	case "done":
		// Allowed from 'approved' (being in approved implies a passed review).
		// Also allowed from 'in_progress' for merge-kind tasks: a merge task's
		// lifecycle is ready→in_progress→done with no review step, so the merger
		// finalizes it directly after the squash-merge.
		if taskState == "approved" || (taskState == "in_progress" && taskKind == "merge") {
			canTransition = true
		}

	case "ready":
		// Allowed from 'approved' or 'blocked' (unblock/retry path)
		if taskState == "approved" || taskState == "blocked" {
			canTransition = true
		}

	case "blocked":
		// Allowed from any active state (backlog, ready, in_progress, review, approved)
		activeStates := map[string]bool{"backlog": true, "ready": true, "in_progress": true, "review": true, "approved": true}
		if activeStates[taskState] {
			canTransition = true
		}

	case "failed":
		// Allowed from any active state or blocked (retire a dead blocked task)
		activeStates := map[string]bool{"backlog": true, "ready": true, "in_progress": true, "review": true, "approved": true}
		if activeStates[taskState] || taskState == "blocked" {
			canTransition = true
		}

	case "superseded":
		// Allowed from any active state (backlog, ready, in_progress, review, approved)
		activeStates := map[string]bool{"backlog": true, "ready": true, "in_progress": true, "review": true, "approved": true}
		if activeStates[taskState] {
			canTransition = true
		}

	case "abandoned":
		// Allowed only from 'approved'
		if taskState == "approved" {
			canTransition = true
		}
	}

	if !canTransition {
		return Task{}, ErrConflict
	}

	if taskState == "approved" {
		if landingAttempt != nil {
			if err := checkLandingOwner(ctx, tx, taskID, *landingAttempt); err != nil {
				return Task{}, err
			}
		} else {
			// While an approve is landing the task, only that approve may move it (to done).
			if err := checkNoLanding(ctx, tx, taskID); err != nil {
				return Task{}, err
			}
			if to == "done" {
				if err := checkNothingToLand(ctx, tx, taskID); err != nil {
					return Task{}, err
				}
			}
		}
	} else if landingAttempt != nil {
		return Task{}, conflict("NOT_APPROVED", fmt.Sprintf("task is in %q state, expected approved", taskState))
	}

	// Perform the conditional UPDATE
	// On blocked→ready, also clear assignee and lease_expires_at to make it freshly claimable.
	// Any transition ends an approve's landing reservation.
	now := nowTimestamp()
	result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET state=?, updated_at=?, landing_round=NULL, landing_commit=NULL, landing_attempt=NULL,
		    assignee=CASE WHEN ? = 'blocked' THEN NULL ELSE assignee END,
		    lease_expires_at=CASE WHEN ? = 'blocked' THEN NULL ELSE lease_expires_at END
		WHERE id=? AND state=?
	`, to, now, taskState, taskState, taskID, taskState)
	if err != nil {
		return Task{}, fmt.Errorf("failed to transition task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected != 1 {
		// This shouldn't happen given our checks above, but be safe
		return Task{}, ErrConflict
	}

	// Append transition event (actor="system", verdict=nil)
	_, err = s.AppendEvent(ctx, tx, taskID, "system", "transition", nil, note)
	if err != nil {
		return Task{}, err
	}

	// Materialize continuation children only when approved research parent reaches done
	// and has opted in to the continuation manifest feature
	if to == "done" && taskState == "approved" {
		// Load full task to check track and spec for continuation opt-in
		var taskTrack, taskSpec, taskProjectID, taskDocumentID string
		var taskReviewRound int
		err := tx.QueryRowContext(ctx, `
			SELECT track, spec, project_id, document_id, review_round FROM task WHERE id = ?
		`, taskID).Scan(&taskTrack, &taskSpec, &taskProjectID, &taskDocumentID, &taskReviewRound)
		if err != nil {
			return Task{}, fmt.Errorf("failed to load task for continuation check: %w", err)
		}

		// Only create children for research track tasks that opt in
		if taskTrack == "research" && specOptsIntoContinuations(taskSpec) {
			// Load manifests for this task and get the one for the current review round
			manifests, err := listSubmissionManifests(ctx, tx, taskID)
			if err != nil {
				return Task{}, fmt.Errorf("failed to load submission manifests: %w", err)
			}

			// Find the manifest for the current review round
			var manifestToUse *SubmissionManifest
			for i := range manifests {
				if manifests[i].ReviewRound == taskReviewRound {
					manifestToUse = &manifests[i]
					break
				}
			}

			if manifestToUse != nil {
				// Canonicalize and verify the manifest digest using strict validation
				cm, err := s.canonicalizeManifest(json.RawMessage(manifestToUse.ManifestJSON), taskID)
				if err != nil {
					return Task{}, fmt.Errorf("failed to validate manifest for materialization: %w", err)
				}

				// Verify the digest matches what was stored
				if cm.digest != manifestToUse.ManifestDigest {
					return Task{}, fmt.Errorf("manifest digest mismatch: stored %q does not match canonical %q", manifestToUse.ManifestDigest, cm.digest)
				}

				// Parse the manifest for insertion
				m := &manifest.Manifest{}
				if err := json.Unmarshal(manifestToUse.ManifestJSON, m); err != nil {
					return Task{}, fmt.Errorf("failed to parse manifest: %w", err)
				}

				// Insert continuation children with idempotency
				if _, err := s.InsertManifestChildren(ctx, tx, m, manifestToUse.ManifestDigest, taskID, taskProjectID, taskDocumentID, now); err != nil {
					return Task{}, fmt.Errorf("failed to insert continuation children: %w", err)
				}
			}
		}
	}

	// SELECT the updated task
	var t Task
	var reviewModelsJSON *string
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch transitioned task: %w", err)
	}

	// Unmarshal review_models from JSON
	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return t, nil
}

// researchHistorySentinel marks the start of a generated research-compaction block in
// a task's spec. It is an HTML comment rather than a plain heading so it can never
// collide with a heading a human happens to write in a research assignment (unlike a
// plain "## Research task history" marker, which a user could write by accident and
// have silently truncated).
const researchHistorySentinel = "<!-- odonian:research-compaction -->"

// extractOriginalAssignment recovers a research task's original assignment by
// stripping any generated compaction block (history and unresolved-findings sections)
// appended by a prior supersession. It cuts at researchHistorySentinel, which
// supersedeTaskTx always writes immediately before that block, so a spec without the
// sentinel (the true original assignment, or user content that merely mentions
// "research task history" in prose) is returned unchanged. Trailing newlines are
// trimmed so re-attaching a fresh block is idempotent across repeated supersessions.
func extractOriginalAssignment(spec string) string {
	idx := strings.Index(spec, researchHistorySentinel)
	if idx == -1 {
		return spec
	}
	return strings.TrimRight(spec[:idx], "\n")
}

// researchReviewerRef identifies one reviewer lineage (model and slot) that raised a
// carried finding.
type researchReviewerRef struct {
	Model string `json:"model"`
	Slot  int    `json:"slot"`
}

func (r researchReviewerRef) lineage() string {
	return researchReviewerLineage(r.Model, r.Slot)
}

// carriedResearchFinding is one unresolved finding carried forward into a research
// replacement's spec, tagged with every reviewer lineage that raised it (a finding
// deduplicated across reviewers keeps all of them) so a later supersession can tell
// which of those reviewers have since re-reviewed.
type carriedResearchFinding struct {
	Finding
	Reviewers []researchReviewerRef `json:"reviewers,omitempty"`
}

// unresolvedResearchFindings returns the findings a research replacement must carry
// forward per docs/features/research-track.md section 7: each reviewer's last
// submitted round's unresolved findings, not the task's full review history.
//
// "Last round" is evaluated per reviewer lineage, not per task: with several review
// models, one reviewer can submit round N while another is still pending (the usual
// reason to re-route a task with `odonian supersede`). A lineage's latest submitted
// round on taskID is authoritative for that reviewer: its outstanding findings there
// are carried, and anything the reviewer reported earlier and did not restate is
// treated as settled. A lineage that has not submitted any review on taskID yet
// (review tasks spawned but pending, or none at all) keeps whatever taskID's own spec
// was already carrying for it from the predecessor, so a pending reviewer's open
// findings are never dropped. Carried findings without a recorded lineage are always
// kept, since no reviewer on taskID can be shown to have re-reviewed them.
//
// "Unresolved" reuses collectResearchReviewReports and researchFindingChains, the
// same prior_id lineage tracking createResearchFollowUpTasks and
// describeChainWideBlockingFindings use. Findings are deduplicated by (severity, file,
// line, summary) so one finding raised by two reviewers is listed once; the deduplicated
// record keeps the union of their lineages.
func (s *sqliteStore) unresolvedResearchFindings(ctx context.Context, tx *sql.Tx, taskID, spec string) ([]carriedResearchFinding, error) {
	var maxRound int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(review_round), 0) FROM task WHERE target_task_id = ? AND kind = 'review'
	`, taskID).Scan(&maxRound); err != nil {
		return nil, fmt.Errorf("failed to find max review round: %w", err)
	}
	allFindings, latestSubmittedRound, err := s.collectResearchReviewReports(ctx, tx, taskID, maxRound)
	if err != nil {
		return nil, fmt.Errorf("failed to collect review findings: %w", err)
	}
	_, isOutstanding := researchFindingChains(allFindings)

	var candidates []carriedResearchFinding
	for _, c := range extractCarriedFindings(spec) {
		if len(c.Reviewers) == 0 {
			candidates = append(candidates, c)
			continue
		}
		var pending []researchReviewerRef
		for _, r := range c.Reviewers {
			if _, reReviewed := latestSubmittedRound[r.lineage()]; !reReviewed {
				pending = append(pending, r)
			}
		}
		if len(pending) > 0 {
			c.Reviewers = pending
			candidates = append(candidates, c)
		}
	}
	for i, cf := range allFindings {
		if cf.round != latestSubmittedRound[cf.lineage] || !isOutstanding(i) {
			continue
		}
		candidates = append(candidates, carriedResearchFinding{
			Finding:   cf.Finding,
			Reviewers: []researchReviewerRef{{Model: cf.reviewerModel, Slot: cf.reviewerSlot}},
		})
	}

	index := make(map[string]int, len(candidates))
	var unresolved []carriedResearchFinding
	for _, c := range candidates {
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s", c.Severity, c.File, c.Line, c.Summary)
		i, ok := index[key]
		if !ok {
			index[key] = len(unresolved)
			unresolved = append(unresolved, c)
			continue
		}
		for _, r := range c.Reviewers {
			if !slices.Contains(unresolved[i].Reviewers, r) {
				unresolved[i].Reviewers = append(unresolved[i].Reviewers, r)
			}
		}
	}
	return unresolved, nil
}

// extractCarriedFindings recovers the findings a research task's own spec is already
// carrying forward from its predecessor (the "Structured findings (JSON)" block
// buildResearchSupersessionSpec writes), so unresolvedResearchFindings can keep the
// ones whose reviewer has not re-reviewed yet; extractOriginalAssignment strips the
// whole generated block, so without this they would be silently dropped. Returns nil
// if spec has no such block. The search starts after researchHistorySentinel, not the
// whole spec, so a coincidental "Structured findings (JSON):" fenced block in the
// user-authored original assignment can never be parsed as carried findings.
func extractCarriedFindings(spec string) []carriedResearchFinding {
	sentinelIdx := strings.Index(spec, researchHistorySentinel)
	if sentinelIdx == -1 {
		return nil
	}
	block := spec[sentinelIdx:]

	const marker = "**Structured findings (JSON):**\n```json\n"
	idx := strings.Index(block, marker)
	if idx == -1 {
		return nil
	}
	rest := block[idx+len(marker):]
	end := strings.Index(rest, "\n```")
	if end == -1 {
		return nil
	}
	var findings []carriedResearchFinding
	if err := json.Unmarshal([]byte(rest[:end]), &findings); err != nil {
		return nil
	}
	return findings
}

// buildResearchSupersessionSpec implements docs/features/research-track.md section 7
// for research tasks: the replacement keeps the original assignment and attaches only
// the last round's unresolved findings, in structured form, plus a link to the
// predecessor task. The full round-by-round history is never inlined; it stays in the
// task's events and in each predecessor's own spec (which itself links further back),
// so the chain is reconstructible without the spec growing on every supersession.
func (s *sqliteStore) buildResearchSupersessionSpec(ctx context.Context, tx *sql.Tx, taskID, spec string) (string, error) {
	originalAssignment := extractOriginalAssignment(spec)

	unresolved, err := s.unresolvedResearchFindings(ctx, tx, taskID, spec)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(originalAssignment)
	b.WriteString("\n\n")
	b.WriteString(researchHistorySentinel)
	b.WriteString("\n## Research task history\n\n")
	b.WriteString(fmt.Sprintf("- Predecessor task (superseded): %s\n", taskID))
	b.WriteString("- The complete round-by-round history lives in the predecessor's events and its own history section; follow the predecessor link back through the chain to see every round.\n")

	if len(unresolved) > 0 {
		b.WriteString("\n## Unresolved findings from last review round\n\n")
		for _, f := range unresolved {
			b.WriteString(fmt.Sprintf("- **%s** (%s, %s:%d): %s\n", f.ID, f.Severity, f.File, f.Line, f.Summary))
		}
		data, err := json.Marshal(unresolved)
		if err != nil {
			return "", fmt.Errorf("failed to marshal unresolved findings: %w", err)
		}
		b.WriteString("\n**Structured findings (JSON):**\n```json\n")
		b.Write(data)
		b.WriteString("\n```\n")
	}

	return b.String(), nil
}

// supersedeTaskTx is the core implementation of task supersession.
// It creates a replacement task with copied fields and dependencies,
// re-points all dependents, and marks the old task as superseded.
// The transaction is NOT committed by this function.
// Returns the new task ID or an error.
func (s *sqliteStore) supersedeTaskTx(ctx context.Context, tx *sql.Tx, taskID string, modelOverride *string) (string, error) {
	// 1. Load old task
	var oldTask Task
	var reviewModelsJSON *string
	err := tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&oldTask.ID, &oldTask.ProjectID, &oldTask.DocumentID, &oldTask.Title, &oldTask.Spec, &oldTask.State, &oldTask.Assignee, &oldTask.LeaseExpiresAt, &oldTask.Result, &oldTask.Model, &oldTask.Kind, &reviewModelsJSON, &oldTask.ReviewRound, &oldTask.TargetTaskID, &oldTask.Verdict, &oldTask.AgentMerge, &oldTask.Held, &oldTask.Escalate, &oldTask.Track, &oldTask.Branch, &oldTask.Priority, &oldTask.TopicAnchorID, &oldTask.CreatedAt, &oldTask.UpdatedAt, &oldTask.ArchivedAt, &oldTask.SupersededBy)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("failed to load old task: %w", err)
	}

	// 1a. Check if task is in a terminal state
	switch oldTask.State {
	case "done", "failed", "abandoned", "superseded":
		return "", ErrConflict
	case "approved":
		if err := checkNoLanding(ctx, tx, taskID); err != nil {
			return "", err
		}
	}

	// Unmarshal review_models
	oldTask.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &oldTask.ReviewModels); err != nil {
			return "", fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	// 1b. Gather prior reject feedback (research tasks instead get a compacted spec:
	// docs/features/research-track.md section 7).
	if oldTask.Track == "research" {
		newSpec, err := s.buildResearchSupersessionSpec(ctx, tx, taskID, oldTask.Spec)
		if err != nil {
			return "", err
		}
		oldTask.Spec = newSpec
	} else {
		rows, err := tx.QueryContext(ctx, `
			SELECT actor, verdict, note FROM event
			WHERE task_id = ? AND kind IN ('review', 'submit') AND verdict = 'reject'
			ORDER BY created_at ASC
		`, taskID)
		if err != nil {
			return "", fmt.Errorf("failed to query feedback events: %w", err)
		}
		defer rows.Close()

		var feedbackBlock strings.Builder
		var hasFeedback bool
		for rows.Next() {
			var actor string
			var verdict *string
			var note *string
			if err := rows.Scan(&actor, &verdict, &note); err != nil {
				return "", fmt.Errorf("failed to scan feedback event: %w", err)
			}

			if !hasFeedback {
				feedbackBlock.WriteString("## Prior attempt feedback\n\n")
				hasFeedback = true
			}

			feedbackBlock.WriteString(fmt.Sprintf("**%s (verdict: %s)**\n", actor, *verdict))
			if note != nil && *note != "" {
				feedbackBlock.WriteString(fmt.Sprintf("%s\n", *note))
			}
			feedbackBlock.WriteString("\n")
		}

		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("failed to iterate feedback events: %w", err)
		}

		// Append feedback to spec if any was found
		if hasFeedback {
			oldTask.Spec = oldTask.Spec + "\n\n" + feedbackBlock.String()
		}
	}

	// 2. Create replacement task
	newTaskID := GenerateID()
	now := nowTimestamp()

	model := oldTask.Model
	if modelOverride != nil {
		model = *modelOverride
	}

	// Validate model against allowlist
	if !s.allowedModelsM[model] {
		return "", invalid("UNKNOWN_MODEL", fmt.Sprintf("unknown model: %s", model))
	}

	var newReviewModelsJSON *string
	if len(oldTask.ReviewModels) > 0 {
		data, err := json.Marshal(oldTask.ReviewModels)
		if err != nil {
			return "", fmt.Errorf("failed to marshal review_models: %w", err)
		}
		str := string(data)
		newReviewModelsJSON = &str
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO task (id, project_id, document_id, title, spec, state, model, kind, review_models, review_round, agent_merge, escalate, track, branch, priority, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, newTaskID, oldTask.ProjectID, oldTask.DocumentID, oldTask.Title, oldTask.Spec, "backlog", model, oldTask.Kind, newReviewModelsJSON, 0, oldTask.AgentMerge, oldTask.Escalate, oldTask.Track, oldTask.Branch, oldTask.Priority, now, now)
	if err != nil {
		return "", fmt.Errorf("failed to insert replacement task: %w", err)
	}

	// 3. Copy upstream dependencies
	_, err = tx.ExecContext(ctx, `
		INSERT INTO task_dep (task_id, depends_on_id)
		SELECT ?, depends_on_id FROM task_dep WHERE task_id = ?
	`, newTaskID, taskID)
	if err != nil {
		return "", fmt.Errorf("failed to copy dependencies: %w", err)
	}

	// 4. Re-point dependents
	_, err = tx.ExecContext(ctx, `
		UPDATE task_dep SET depends_on_id = ? WHERE depends_on_id = ?
	`, newTaskID, taskID)
	if err != nil {
		return "", fmt.Errorf("failed to re-point dependents: %w", err)
	}

	// 5. Retire old task
	_, err = tx.ExecContext(ctx, `
		UPDATE task SET state = ?, superseded_by = ?, updated_at = ? WHERE id = ?
	`, "superseded", newTaskID, now, taskID)
	if err != nil {
		return "", fmt.Errorf("failed to retire old task: %w", err)
	}

	// 6. Emit event
	note := fmt.Sprintf("Superseded by %s", newTaskID)
	_, err = s.AppendEvent(ctx, tx, taskID, "system", "task_superseded", nil, &note)
	if err != nil {
		return "", err
	}

	return newTaskID, nil
}

// SupersedeTask atomically creates a replacement task with copied fields and dependencies,
// re-points all dependents, and marks the old task as superseded.
// After the transaction commits, it best-effort closes the old task's recorded pull
// request (if any) and deletes its head branch — see closeSupersededPR.
// Returns the new Task or ErrNotFound if the old task doesn't exist.
func (s *sqliteStore) SupersedeTask(ctx context.Context, taskID string, modelOverride *string) (Task, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	newTaskID, err := s.supersedeTaskTx(ctx, tx, taskID, modelOverride)
	if err != nil {
		return Task{}, err
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	// The task state change above is authoritative; closing its pull request is
	// best-effort cleanup that must never roll back or block the supersession that
	// already happened. Run it in the background so a slow or throttled GitHub API
	// can't add latency to this call, on a context detached from the request (which
	// is canceled the moment the HTTP handler returns) and bounded by its own
	// timeout so it can't run forever either.
	go func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		s.closeSupersededPR(closeCtx, taskID, newTaskID)
		if s.supersedeCloseHook != nil {
			s.supersedeCloseHook()
		}
	}()

	// Load the new task from the database (this gets the feedback-augmented spec)
	var newTask Task
	var reviewModelsJSON *string
	err = s.conn.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, newTaskID).Scan(&newTask.ID, &newTask.ProjectID, &newTask.DocumentID, &newTask.Title, &newTask.Spec, &newTask.State, &newTask.Assignee, &newTask.LeaseExpiresAt, &newTask.Result, &newTask.Model, &newTask.Kind, &reviewModelsJSON, &newTask.ReviewRound, &newTask.TargetTaskID, &newTask.Verdict, &newTask.AgentMerge, &newTask.Held, &newTask.Escalate, &newTask.Track, &newTask.Branch, &newTask.Priority, &newTask.TopicAnchorID, &newTask.CreatedAt, &newTask.UpdatedAt, &newTask.ArchivedAt, &newTask.SupersededBy)
	if err != nil {
		return Task{}, fmt.Errorf("failed to load new task: %w", err)
	}

	// Unmarshal review_models
	newTask.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &newTask.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	return newTask, nil
}

// UpdateTaskEscalate sets a task's escalate flag and appends a "policy-change"
// audit event recording the old and new values. It is permitted on any
// nonterminal task (including blocked) and rejected on terminal states
// (done, failed, abandoned, superseded). Setting the flag to its current
// value is a no-op: the task is returned unchanged and no event is appended,
// so repeated calls with the same value never accumulate duplicate history.
// It never resumes or otherwise transitions the task, and never touches
// dependencies, links, or review history.
func (s *sqliteStore) UpdateTaskEscalate(ctx context.Context, taskID string, escalate bool) (Task, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var t Task
	var reviewModelsJSON *string
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch task: %w", err)
	}

	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	switch t.State {
	case "done", "failed", "abandoned", "superseded":
		return Task{}, conflict("TERMINAL_STATE", fmt.Sprintf("cannot update escalate on task in terminal state %q", t.State))
	}

	if t.Escalate == escalate {
		if err := tx.Commit(); err != nil {
			return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
		}
		return t, nil
	}

	now := nowTimestamp()
	if _, err := tx.ExecContext(ctx, `UPDATE task SET escalate = ?, updated_at = ? WHERE id = ?`, escalate, now, taskID); err != nil {
		return Task{}, fmt.Errorf("failed to update task escalate flag: %w", err)
	}

	note := fmt.Sprintf("escalate policy changed: %v → %v", t.Escalate, escalate)
	if _, err := s.AppendEvent(ctx, tx, taskID, "system", "policy-change", nil, &note); err != nil {
		return Task{}, fmt.Errorf("failed to append policy-change event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	t.Escalate = escalate
	t.UpdatedAt = now
	return t, nil
}

// closeSupersededPR closes the old task's recorded pull request (if any and still
// open), posts a comment naming the replacement task, and deletes its head branch.
// GitHub retains refs/pull/N/head for a closed PR, so the commits stay reachable even
// after the branch is gone. Every step is best-effort: a failure is logged and the
// remaining steps are skipped, but the caller never sees an error, since a pull
// request that can't be closed must never be treated as a failed supersession.
func (s *sqliteStore) closeSupersededPR(ctx context.Context, oldTaskID, newTaskID string) {
	logger := slog.Default()

	oldTask, err := s.GetTask(ctx, oldTaskID)
	if err != nil {
		logger.Error("failed to load superseded task for PR close", "task_id", oldTaskID, "error", err)
		return
	}

	var prLink *TaskLink
	for i := range oldTask.Links {
		if oldTask.Links[i].Kind == "pr" {
			prLink = &oldTask.Links[i]
			break
		}
	}

	if prLink == nil {
		return
	}

	owner, repo, prNumber, err := forge.ParsePRURL(prLink.Value)
	if err != nil {
		logger.Error("failed to parse PR URL for superseded task", "task_id", oldTaskID, "pr_url", prLink.Value, "error", err)
		return
	}

	token, err := forge.OwnerToken(owner)
	if err != nil {
		logger.Error("failed to get forge token for PR close", "task_id", oldTaskID, "owner", owner, "error", err)
		return
	}

	if token == "" {
		logger.Info("skipped supersede PR cleanup: no forge token", "task_id", oldTaskID, "owner", owner, "pr_url", prLink.Value)
		return
	}

	state, err := forge.GetPRState(ctx, owner, repo, prNumber, token)
	if err != nil {
		logger.Error("failed to get PR state for superseded task", "task_id", oldTaskID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
		return
	}

	// Idempotent and never touches a merged or already-closed PR.
	if state != "open" {
		return
	}

	comment := fmt.Sprintf("Superseded by task %s. Closing this pull request; the current attempt continues there.", newTaskID)
	if err := forge.PostPRComment(ctx, owner, repo, prNumber, token, comment); err != nil {
		logger.Error("failed to post comment on superseded PR", "task_id", oldTaskID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
		// Continue to close the PR even if the comment failed.
	}

	if err := forge.ClosePR(ctx, owner, repo, prNumber, token); err != nil {
		logger.Error("failed to close superseded PR", "task_id", oldTaskID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
		return
	}

	branch := "mr/" + oldTaskID[:8]
	if err := forge.DeleteBranch(ctx, owner, repo, branch, token); err != nil {
		logger.Error("failed to delete superseded task's branch", "task_id", oldTaskID, "owner", owner, "repo", repo, "branch", branch, "error", err)
	}

	logger.Info("closed superseded PR", "task_id", oldTaskID, "owner", owner, "repo", repo, "pr_number", prNumber, "replacement_task_id", newTaskID)
}

// ArchiveTask sets the archived_at timestamp for a task to the current time.
// Archiving is orthogonal to the task state machine and does not alter the state.
// Returns the updated Task or ErrNotFound if the task doesn't exist.
func (s *sqliteStore) ArchiveTask(ctx context.Context, taskID string) (Task, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := nowTimestamp()

	// Update the task's archived_at timestamp
	result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET archived_at=?, updated_at=?
		WHERE id=?
	`, now, now, taskID)
	if err != nil {
		return Task{}, fmt.Errorf("failed to archive task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected != 1 {
		return Task{}, ErrNotFound
	}

	// SELECT the updated task
	var t Task
	var reviewModelsJSON *string
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch archived task: %w", err)
	}

	// Unmarshal review_models from JSON
	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return t, nil
}

// UnarchiveTask clears the archived_at timestamp for a task (sets it to NULL).
// Returns the updated Task or ErrNotFound if the task doesn't exist.
func (s *sqliteStore) UnarchiveTask(ctx context.Context, taskID string) (Task, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := nowTimestamp()

	// Clear the task's archived_at timestamp
	result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET archived_at=NULL, updated_at=?
		WHERE id=?
	`, now, taskID)
	if err != nil {
		return Task{}, fmt.Errorf("failed to unarchive task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected != 1 {
		return Task{}, ErrNotFound
	}

	// SELECT the updated task
	var t Task
	var reviewModelsJSON *string
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch unarchived task: %w", err)
	}

	// Unmarshal review_models from JSON
	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return t, nil
}

// ArchiveProject sets the archived_at timestamp for a project to the current time.
// Returns the updated Project or ErrNotFound if the project doesn't exist.
func (s *sqliteStore) ArchiveProject(ctx context.Context, projectID string) (Project, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := nowTimestamp()

	// Update the project's archived_at timestamp
	result, err := tx.ExecContext(ctx, `
		UPDATE project
		SET archived_at=?
		WHERE id=?
	`, now, projectID)
	if err != nil {
		return Project{}, fmt.Errorf("failed to archive project: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Project{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected != 1 {
		return Project{}, ErrNotFound
	}

	// SELECT the updated project
	var p Project
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, repo, created_at, archived_at FROM project WHERE id = ?
	`, projectID).Scan(&p.ID, &p.Name, &p.Repo, &p.CreatedAt, &p.ArchivedAt)
	if err != nil {
		return Project{}, fmt.Errorf("failed to fetch archived project: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return p, nil
}

// UnarchiveProject clears the archived_at timestamp for a project (sets it to NULL).
// Returns the updated Project or ErrNotFound if the project doesn't exist.
func (s *sqliteStore) UnarchiveProject(ctx context.Context, projectID string) (Project, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Clear the project's archived_at timestamp
	result, err := tx.ExecContext(ctx, `
		UPDATE project
		SET archived_at=NULL
		WHERE id=?
	`, projectID)
	if err != nil {
		return Project{}, fmt.Errorf("failed to unarchive project: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Project{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected != 1 {
		return Project{}, ErrNotFound
	}

	// SELECT the updated project
	var p Project
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, repo, created_at, archived_at FROM project WHERE id = ?
	`, projectID).Scan(&p.ID, &p.Name, &p.Repo, &p.CreatedAt, &p.ArchivedAt)
	if err != nil {
		return Project{}, fmt.Errorf("failed to fetch unarchived project: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return p, nil
}

// TombstoneLink marks a task link as tombstoned when the reconciler has established there is nothing left to do for it
// (PR is gone, merged, closed, or closed by the reconciler). This prevents future reconciler passes from unnecessarily
// retrying the link.
func (s *sqliteStore) TombstoneLink(ctx context.Context, taskID, linkID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.conn.ExecContext(ctx, `
		UPDATE task_link
		SET tombstoned_at = ?
		WHERE id = ? AND task_id = ?
	`, now, linkID, taskID)
	if err != nil {
		return fmt.Errorf("failed to tombstone link: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected != 1 {
		return ErrNotFound
	}

	return nil
}

// submissionLinks returns the active (non-tombstoned) links of the submission that started
// review round reviewRound: those tagged with that round. A task none of whose links are tagged
// was only ever submitted before links were tagged, so its untagged links are all there is.
func submissionLinks(ctx context.Context, q eventQuerier, taskID string, reviewRound int) ([]TaskLink, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, task_id, kind, value, tombstoned_at, review_round FROM task_link WHERE task_id = ? AND tombstoned_at IS NULL ORDER BY id
	`, taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to query active task links: %w", err)
	}
	defer rows.Close()
	var roundLinks, untagged []TaskLink
	anyTagged := false
	for rows.Next() {
		var link TaskLink
		if err := rows.Scan(&link.ID, &link.TaskID, &link.Kind, &link.Value, &link.TombstonedAt, &link.ReviewRound); err != nil {
			return nil, fmt.Errorf("failed to scan task link: %w", err)
		}
		switch {
		case link.ReviewRound == nil:
			untagged = append(untagged, link)
		case *link.ReviewRound == reviewRound:
			anyTagged = true
			roundLinks = append(roundLinks, link)
		default:
			anyTagged = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate task links: %w", err)
	}
	if anyTagged {
		return roundLinks, nil
	}
	return untagged, nil
}

// checkNoLanding returns a LANDING_IN_PROGRESS conflict if an approve holds task taskID's
// landing reservation (see BeginLanding).
func checkNoLanding(ctx context.Context, tx *sql.Tx, taskID string) error {
	var landingRound sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT landing_round FROM task WHERE id = ?", taskID).Scan(&landingRound); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("failed to load landing reservation: %w", err)
	}
	if landingRound.Valid {
		return conflict("LANDING_IN_PROGRESS", fmt.Sprintf("an approve is landing this task's reviewed work (round %d) on its local_commit branch, so only that approve can move it (to done); finish it with `odonian approve %s`, or, if nothing has landed yet, cancel it with `odonian approve %s --cancel-landing`", landingRound.Int64, taskID, taskID))
	}
	return nil
}

// checkLandingOwner returns a conflict unless attempt holds task taskID's landing reservation.
func checkLandingOwner(ctx context.Context, tx *sql.Tx, taskID, attempt string) error {
	var landingRound sql.NullInt64
	var landingAttempt sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT landing_round, landing_attempt FROM task WHERE id = ?", taskID).Scan(&landingRound, &landingAttempt); err != nil {
		return fmt.Errorf("failed to load landing reservation: %w", err)
	}
	if !landingRound.Valid {
		return conflict("NOT_RESERVED", "the task holds no landing reservation; reserve it (POST /tasks/{id}/landing) before completing the landing")
	}
	if landingAttempt.String != attempt {
		return conflict("LANDING_ATTEMPT_MISMATCH", "the landing reservation belongs to another approve attempt")
	}
	return nil
}

// checkNothingToLand returns a LANDING_REQUIRED conflict if task taskID's current review round
// submitted a commit (a local_commit task): marking it done directly would unblock its
// dependents without that commit on the branch they start from. Such a task reaches done only
// through CompleteLanding.
func checkNothingToLand(ctx context.Context, tx *sql.Tx, taskID string) error {
	var reviewRound int
	if err := tx.QueryRowContext(ctx, "SELECT review_round FROM task WHERE id = ?", taskID).Scan(&reviewRound); err != nil {
		return fmt.Errorf("failed to load review round: %w", err)
	}
	roundLinks, err := submissionLinks(ctx, tx, taskID, reviewRound)
	if err != nil {
		return err
	}
	for _, link := range roundLinks {
		if link.Kind == "commit" {
			return conflict("LANDING_REQUIRED", fmt.Sprintf("this task's reviewed work is local_commit commit %s, which must land on its branch before the task is done; approve it with `odonian approve %s`", link.Value, taskID))
		}
	}
	return nil
}

// BeginLanding reserves an approved task for `odonian approve` to land the work reviewed in
// reviewRound (the reviewed commit is recorded alongside). It is atomic and conditional: it fails
// with NOT_APPROVED unless the task is approved, and with STALE_REVIEW_ROUND unless reviewRound is
// still the task's current review round — so a task reworked and re-approved while approve ran its
// merge gate is never finalised with the version approve prepared. Until the task transitions,
// it can only move to done. attempt identifies the approve making the reservation; re-reserving
// the same round and commit (a resumed approve) succeeds and makes attempt the owner, so only it
// can cancel the reservation (see CancelLanding). A different round or commit is a
// LANDING_IN_PROGRESS conflict. Returns ErrNotFound for an unknown task.
func (s *sqliteStore) BeginLanding(ctx context.Context, taskID string, reviewRound int, commit, attempt string) error {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var taskState string
	var currentRound int
	var landingRound sql.NullInt64
	var landingCommit sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT state, review_round, landing_round, landing_commit FROM task WHERE id = ?", taskID).Scan(&taskState, &currentRound, &landingRound, &landingCommit)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to load task: %w", err)
	}
	if taskState != "approved" {
		return conflict("NOT_APPROVED", fmt.Sprintf("task is in %q state, expected approved", taskState))
	}
	if currentRound != reviewRound {
		return conflict("STALE_REVIEW_ROUND", fmt.Sprintf("task has been reviewed again since this approve read it (it is in review round %d, the approve prepared round %d)", currentRound, reviewRound))
	}
	if landingRound.Valid {
		if int(landingRound.Int64) != reviewRound || landingCommit.String != commit {
			return conflict("LANDING_IN_PROGRESS", fmt.Sprintf("another approve is landing round %d (commit %s)", landingRound.Int64, landingCommit.String))
		}
		// A resumed approve: it takes over the reservation.
		if _, err := tx.ExecContext(ctx, "UPDATE task SET landing_attempt=? WHERE id=?", attempt, taskID); err != nil {
			return fmt.Errorf("failed to take over landing reservation: %w", err)
		}
		return tx.Commit()
	}
	if err := checkReviewedCommit(ctx, tx, taskID, reviewRound, commit); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task SET landing_round=?, landing_commit=?, landing_attempt=? WHERE id=? AND state='approved' AND review_round=?
	`, reviewRound, commit, attempt, taskID, reviewRound); err != nil {
		return fmt.Errorf("failed to reserve task for landing: %w", err)
	}
	return tx.Commit()
}

// checkReviewedCommit returns an UNREVIEWED_COMMIT conflict unless commit is the commit submitted
// for review round reviewRound, so approve lands exactly what the reviewers approved. A round that
// submitted no commit (including a no_op) has nothing to land. A task submitted before links
// carried their round passes only with exactly one commit link.
func checkReviewedCommit(ctx context.Context, tx *sql.Tx, taskID string, reviewRound int, commit string) error {
	roundLinks, err := submissionLinks(ctx, tx, taskID, reviewRound)
	if err != nil {
		return err
	}
	var reviewedCommits []string
	var noOp, untagged bool
	for _, link := range roundLinks {
		untagged = untagged || link.ReviewRound == nil
		switch link.Kind {
		case "commit":
			reviewedCommits = append(reviewedCommits, link.Value)
		case "no_op":
			noOp = true
		}
	}
	if untagged && len(reviewedCommits) > 1 {
		// Submitted before links carried their round, and reworked: any of these may be an
		// earlier, rejected round's commit, so none of them can be landed as the reviewed one.
		return conflict("UNREVIEWED_COMMIT", fmt.Sprintf("the task's %d commit links (%s) predate links recording their review round, so which one round %d reviewed is unknown; reject it and re-submit to record it", len(reviewedCommits), strings.Join(reviewedCommits, ", "), reviewRound))
	}
	if slices.Contains(reviewedCommits, commit) {
		return nil
	}
	if len(reviewedCommits) == 0 && noOp {
		return conflict("UNREVIEWED_COMMIT", fmt.Sprintf("review round %d was a no-op submission, so there is nothing reviewed to land", reviewRound))
	}
	if len(reviewedCommits) == 0 {
		return conflict("UNREVIEWED_COMMIT", fmt.Sprintf("review round %d has no submitted commit, so there is nothing reviewed to land", reviewRound))
	}
	return conflict("UNREVIEWED_COMMIT", fmt.Sprintf("commit %s is not the commit reviewed in round %d (%s); the task's wip branch has changed since it was submitted", commit, reviewRound, strings.Join(reviewedCommits, ", ")))
}

// CancelLanding drops a landing reservation, but only if attempt still owns it: an approve that
// stalled and was replaced (the replacement re-reserved and so took ownership) gets a
// LANDING_ATTEMPT_MISMATCH conflict instead of clearing its replacement's reservation. Only safe
// when the reserved work has not reached the branch, which the server cannot see: callers check
// that first. Cancelling an unreserved task is a no-op. Returns ErrNotFound for an unknown task.
func (s *sqliteStore) CancelLanding(ctx context.Context, taskID, attempt string) error {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var landingRound sql.NullInt64
	var landingAttempt sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT landing_round, landing_attempt FROM task WHERE id = ?", taskID).Scan(&landingRound, &landingAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to load landing reservation: %w", err)
	}
	if !landingRound.Valid {
		return nil
	}
	if landingAttempt.String != attempt {
		return conflict("LANDING_ATTEMPT_MISMATCH", "the landing reservation now belongs to another approve attempt, so it was not cancelled")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE task SET landing_round=NULL, landing_commit=NULL, landing_attempt=NULL WHERE id=? AND landing_attempt=?", taskID, attempt); err != nil {
		return fmt.Errorf("failed to cancel landing: %w", err)
	}
	return tx.Commit()
}

// HoldTask sets the held flag on a task, preventing it from being claimed or auto-transitioned.
// Hold works from any state (it is an orthogonal lock, not a state transition).
// Returns the updated Task on success.
// Returns ErrNotFound if the task doesn't exist.
func (s *sqliteStore) HoldTask(ctx context.Context, taskID string) (Task, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := nowTimestamp()

	result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET held=1, updated_at=?
		WHERE id=?
	`, now, taskID)
	if err != nil {
		return Task{}, fmt.Errorf("failed to hold task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected != 1 {
		return Task{}, ErrNotFound
	}

	var t Task
	var reviewModelsJSON *string
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch held task: %w", err)
	}

	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return t, nil
}

// ReleaseTask clears the held flag on a task, restoring normal automated flow.
// Release works from any state (it is an orthogonal lock, not a state transition).
// If the task is in review and all review tasks targeting it are done, aggregates the review round.
// Returns the updated Task on success.
// Returns ErrNotFound if the task doesn't exist.
func (s *sqliteStore) ReleaseTask(ctx context.Context, taskID string, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int, researchRoundBudget int) (Task, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := nowTimestamp()

	result, err := tx.ExecContext(ctx, `
		UPDATE task
		SET held=0, updated_at=?
		WHERE id=?
	`, now, taskID)
	if err != nil {
		return Task{}, fmt.Errorf("failed to release task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected != 1 {
		return Task{}, ErrNotFound
	}

	var t Task
	var reviewModelsJSON *string
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch released task: %w", err)
	}

	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	// If task is in review, check if all review tasks are done and aggregate if so
	if t.State == "review" {
		var totalReviewTasks, doneReviewTasks int
		err = tx.QueryRowContext(ctx, `
			SELECT
				COUNT(*) as total,
				SUM(CASE WHEN state='done' THEN 1 ELSE 0 END) as done
			FROM task
			WHERE target_task_id = ? AND review_round = ? AND adjudicate_finding_id IS NULL
		`, taskID, t.ReviewRound).Scan(&totalReviewTasks, &doneReviewTasks)
		if err != nil {
			return Task{}, fmt.Errorf("failed to tally review tasks: %w", err)
		}

		// If all review tasks are done, aggregate the round
		if totalReviewTasks > 0 && doneReviewTasks == totalReviewTasks {
			_, err = s.aggregateReviewRound(ctx, tx, taskID, maxReviewRounds, escalationThresholds, s.researchEscalationLadder, researchEscalationThresholds, researchRoundBudget)
			if err != nil {
				return Task{}, err
			}

			// Re-fetch the task to get the updated state
			err = tx.QueryRowContext(ctx, `
				SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
				FROM task WHERE id = ?
			`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
			if err != nil {
				return Task{}, fmt.Errorf("failed to fetch task after aggregation: %w", err)
			}

			t.ReviewModels = []string{}
			if reviewModelsJSON != nil {
				if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
					return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return t, nil
}

// UpdateTaskDependsOn updates the depends_on edges for a task.
// Returns ValidationError if self-dependency is detected.
// Returns ConflictError if a cycle would be created.
func (s *sqliteStore) UpdateTaskDependsOn(ctx context.Context, taskID string, depIDs []string) (Task, error) {
	// Check for self-dependency
	for _, depID := range depIDs {
		if depID == taskID {
			return Task{}, invalid("SELF_DEPENDENCY", "a task cannot depend on itself")
		}
	}

	// Load the full dependency graph
	rows, err := s.conn.QueryContext(ctx, `
		SELECT task_id, depends_on_id FROM task_dep
	`)
	if err != nil {
		return Task{}, fmt.Errorf("failed to query dependency graph: %w", err)
	}
	defer rows.Close()

	edges := make(map[string][]string)
	for rows.Next() {
		var taskID, depID string
		if err := rows.Scan(&taskID, &depID); err != nil {
			return Task{}, fmt.Errorf("failed to scan dependency: %w", err)
		}
		edges[taskID] = append(edges[taskID], depID)
	}
	if err := rows.Err(); err != nil {
		return Task{}, fmt.Errorf("error iterating dependency graph: %w", err)
	}

	// Check if the update would create a cycle
	if wouldCreateCycle(edges, taskID, depIDs) {
		return Task{}, conflict("CYCLE_DETECTED", "updating depends_on would create a cycle")
	}

	// Update the dependencies in a transaction
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if err := s.setTaskDepends(ctx, tx, taskID, depIDs); err != nil {
		return Task{}, err
	}

	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	// Return the updated task
	var t Task
	var reviewModelsJSON *string
	err = s.conn.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, `+taskTopicColumns+`, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.TopicAnchorID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch updated task: %w", err)
	}

	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}

	return t, nil
}

// PruneEvents removes old events according to retention policy.
// Events older than retentionDays are deleted unconditionally.
// For tasks in terminal states (done/failed/archived/abandoned), events older than terminalRetentionDays are deleted.
// Events for active (non-terminal) tasks are never pruned.
// Events for research-track tasks are kept indefinitely because the reviewer scorecard is computed from them.
// Returns the number of rows deleted.
func (s *sqliteStore) PruneEvents(ctx context.Context, terminalRetentionDays int) (int64, error) {
	// Calculate cutoff timestamp for terminal task events
	terminalCutoff := time.Now().UTC().AddDate(0, 0, -terminalRetentionDays).Format(timestampLayout)

	// Delete events for terminal tasks older than terminalCutoff.
	// Active-task events are never deleted.
	// Research-track task events are kept indefinitely to preserve the reviewer scorecard.
	result, err := s.conn.ExecContext(ctx, `
		DELETE FROM event
		WHERE
			task_id IN (
				SELECT id FROM task
				WHERE state IN ('done', 'failed', 'archived', 'abandoned')
				AND track != 'research'
			)
			AND created_at < ?
	`, terminalCutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to prune events: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return rowsAffected, nil
}

// listSubmissionManifests returns taskID's stored continuation manifests, oldest round first
// (never nil, so it serialises as []).
func listSubmissionManifests(ctx context.Context, q eventQuerier, taskID string) ([]SubmissionManifest, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT review_round, parent_task_id, manifest_json, manifest_digest, created_at
		FROM task_submission_manifest WHERE task_id = ? ORDER BY review_round
	`, taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to query submission manifests: %w", err)
	}
	defer rows.Close()
	manifests := []SubmissionManifest{}
	for rows.Next() {
		var sm SubmissionManifest
		var manifestJSON string
		if err := rows.Scan(&sm.ReviewRound, &sm.ParentTaskID, &manifestJSON, &sm.ManifestDigest, &sm.SubmittedAt); err != nil {
			return nil, fmt.Errorf("failed to scan submission manifest: %w", err)
		}
		sm.ManifestJSON = json.RawMessage(manifestJSON)
		manifests = append(manifests, sm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating submission manifests: %w", err)
	}
	return manifests, nil
}

// continuationOptInHeading is the exact line a research task's spec must contain, on a line of
// its own, to opt in to carrying a continuation manifest. Matching ignores case and surrounding
// whitespace but nothing else, so prose that mentions the phrase, or a deeper heading such as
// "### continuation manifest", does not opt a task in.
const continuationOptInHeading = "## continuation manifest"

func specOptsIntoContinuations(spec string) bool {
	for _, line := range strings.Split(spec, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), continuationOptInHeading) {
			return true
		}
	}
	return false
}

type canonicalManifest struct {
	parentTaskID string
	json         string
	digest       string
}

// canonicalizeManifest parses raw strictly (unknown fields and trailing data are rejected),
// requires its parent_task_id to be taskID, validates it against the child envelope (the
// deployment's model allowlist, the task tracks, and the manifest package's limits), and
// returns its canonical JSON with that JSON's SHA-256. The canonical form is the manifest
// re-encoded from its struct with nil lists normalised to empty, so documents that differ only
// in whitespace, key order, or null-versus-empty lists share one JSON text and one digest.
func (s *sqliteStore) canonicalizeManifest(raw json.RawMessage, taskID string) (*canonicalManifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m manifest.Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, invalid("INVALID_MANIFEST_JSON", fmt.Sprintf("manifest is not a valid continuation manifest: %v", err))
	}
	if dec.More() {
		return nil, invalid("INVALID_MANIFEST_JSON", "manifest has trailing data after the JSON object")
	}
	if m.ParentTaskID != taskID {
		return nil, invalid("MISMATCHED_PARENT_TASK", fmt.Sprintf("manifest parent_task_id %q must be the submitted task %q", m.ParentTaskID, taskID))
	}
	if err := m.Validate(s.allowedModelsM, validTracks); err != nil {
		var ve manifest.ValidationError
		if errors.As(err, &ve) {
			return nil, invalid(ve.Code, ve.Message)
		}
		return nil, invalid("INVALID_MANIFEST", err.Error())
	}

	if m.Children == nil {
		m.Children = []manifest.Child{}
	}
	if m.PendingCandidates == nil {
		m.PendingCandidates = []manifest.PendingCandidate{}
	}
	for i := range m.Children {
		c := &m.Children[i]
		if c.ReviewModels == nil {
			c.ReviewModels = []string{}
		}
		if c.ClaimIDs == nil {
			c.ClaimIDs = []string{}
		}
		if c.SourceStartPoints == nil {
			c.SourceStartPoints = []string{}
		}
		if c.FileScope == nil {
			c.FileScope = []string{}
		}
		if c.AcceptanceCriteria == nil {
			c.AcceptanceCriteria = []string{}
		}
		if c.Dependencies == nil {
			c.Dependencies = []manifest.Dependency{}
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("failed to encode canonical manifest: %w", err)
	}
	canonical := strings.TrimSuffix(buf.String(), "\n")
	sum := sha256.Sum256([]byte(canonical))
	return &canonicalManifest{parentTaskID: m.ParentTaskID, json: canonical, digest: hex.EncodeToString(sum[:])}, nil
}
