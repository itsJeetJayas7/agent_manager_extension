// Package db provides SQLite persistence: WAL mode, foreign keys,
// embedded migrations, and transactional state transitions.
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"agent-conductor/internal/model"
)

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path with pragmas.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc sqlite is safest with a single writer connection.
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		return nil, err
	}
	s := &Store{db: d}
	if err := s.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// migrations run in order; version = index+1.
var migrations = []string{
	// 1: core schema
	`CREATE TABLE batches (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'QUEUED',
		base_cwd TEXT NOT NULL DEFAULT '',
		base_branch TEXT NOT NULL DEFAULT '',
		base_commit TEXT NOT NULL DEFAULT '',
		integration_branch TEXT NOT NULL DEFAULT '',
		integration_wt_path TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		started_at TEXT NOT NULL DEFAULT '',
		finished_at TEXT NOT NULL DEFAULT '',
		applied INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE batch_rows (
		id TEXT PRIMARY KEY,
		batch_id TEXT NOT NULL REFERENCES batches(id),
		position INTEGER NOT NULL,
		mode TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'PENDING'
	);
	CREATE INDEX idx_rows_batch ON batch_rows(batch_id, position);
	CREATE TABLE runs (
		id TEXT PRIMARY KEY,
		batch_id TEXT NOT NULL REFERENCES batches(id),
		row_id TEXT NOT NULL REFERENCES batch_rows(id),
		card_index INTEGER NOT NULL DEFAULT 0,
		title TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		effort TEXT NOT NULL DEFAULT '',
		permission TEXT NOT NULL DEFAULT 'bypass',
		staged INTEGER NOT NULL DEFAULT 0,
		attach_secrets INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL,
		stage TEXT NOT NULL DEFAULT '',
		prompt_cipher TEXT NOT NULL DEFAULT '',
		prompt_hash TEXT NOT NULL DEFAULT '',
		prompt_preview TEXT NOT NULL DEFAULT '',
		secret_version_id TEXT NOT NULL DEFAULT '',
		worktree_path TEXT NOT NULL DEFAULT '',
		branch TEXT NOT NULL DEFAULT '',
		provider_session_id TEXT NOT NULL DEFAULT '',
		provider_turn_id TEXT NOT NULL DEFAULT '',
		provider_sub_session TEXT NOT NULL DEFAULT '',
		provider_title TEXT NOT NULL DEFAULT '',
		continuation_count INTEGER NOT NULL DEFAULT 0,
		audit_cycles INTEGER NOT NULL DEFAULT 0,
		verify_cycles INTEGER NOT NULL DEFAULT 0,
		error_code TEXT NOT NULL DEFAULT '',
		error_msg TEXT NOT NULL DEFAULT '',
		final_outcome TEXT NOT NULL DEFAULT '',
		meaningful_progress INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		queued_at TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL DEFAULT '',
		finished_at TEXT NOT NULL DEFAULT '',
		next_eligible_at TEXT NOT NULL DEFAULT '',
		queue_wait_ns INTEGER NOT NULL DEFAULT 0,
		active_ns INTEGER NOT NULL DEFAULT 0,
		quota_wait_ns INTEGER NOT NULL DEFAULT 0,
		dispatch_uuid TEXT NOT NULL DEFAULT '',
		attempt INTEGER NOT NULL DEFAULT 0,
		sort_key INTEGER NOT NULL DEFAULT 0,
		cwd TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX idx_runs_batch ON runs(batch_id);
	CREATE INDEX idx_runs_state ON runs(state);
	CREATE INDEX idx_runs_sort ON runs(sort_key);
	CREATE TABLE run_attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL REFERENCES runs(id),
		attempt_no INTEGER NOT NULL,
		dispatch_uuid TEXT NOT NULL,
		provider_session TEXT NOT NULL DEFAULT '',
		provider_turn TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL,
		ended_at TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL DEFAULT '',
		meaningful_progress INTEGER NOT NULL DEFAULT 0,
		UNIQUE(run_id, attempt_no)
	);
	CREATE TABLE run_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		ts TEXT NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX idx_events_run ON run_events(run_id, id);
	CREATE TABLE provider_sessions (
		provider TEXT NOT NULL,
		native_id TEXT NOT NULL,
		run_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		cwd TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		last_event TEXT NOT NULL DEFAULT '',
		last_seen_at TEXT NOT NULL,
		PRIMARY KEY (provider, native_id)
	);
	CREATE TABLE quota_windows (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL,
		window_id TEXT NOT NULL,
		name TEXT NOT NULL,
		used_percent REAL NOT NULL DEFAULT 0,
		resets_at TEXT NOT NULL DEFAULT '',
		exhausted INTEGER NOT NULL DEFAULT 0,
		source TEXT NOT NULL,
		confidence TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(provider, window_id)
	);
	CREATE TABLE worktrees (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		batch_id TEXT NOT NULL,
		run_id TEXT NOT NULL DEFAULT '',
		path TEXT NOT NULL UNIQUE,
		branch TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'ACTIVE',
		created_at TEXT NOT NULL
	);
	CREATE TABLE settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE secret_versions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at TEXT NOT NULL,
		cipher TEXT NOT NULL,
		hash TEXT NOT NULL,
		preview TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE service_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
		CREATE TABLE anchors (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL,
		window_reset TEXT NOT NULL,
		attempt_id TEXT NOT NULL UNIQUE,
		started_at TEXT NOT NULL,
		completed_at TEXT NOT NULL DEFAULT '',
		session_ref TEXT NOT NULL DEFAULT '',
		UNIQUE(provider, window_reset)
	);`,
	// 2: implicit queue batches
	`ALTER TABLE batches ADD COLUMN implicit INTEGER NOT NULL DEFAULT 0;`,
	// 3: integer ordering keys.
	//
	// created_at is RFC3339Nano text and RFC3339Nano strips trailing zeros, so
	// "…:00.5Z" sorts after "…:00.45Z" lexicographically but before it
	// chronologically — ORDER BY on the string was simply wrong. batch_number
	// replaces a hash of the batch id into 1..89, whose collisions drove git
	// branch names and destroyed previous batches' work.
	`ALTER TABLE batches ADD COLUMN created_ns INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE batches ADD COLUMN batch_number INTEGER NOT NULL DEFAULT 0;
	UPDATE batches SET created_ns = CAST(strftime('%s', created_at) AS INTEGER) * 1000000000
		WHERE created_ns = 0 AND created_at != '';
	UPDATE batches SET batch_number = (
		SELECT COUNT(*) FROM batches b2 WHERE b2.created_ns < batches.created_ns OR
			(b2.created_ns = batches.created_ns AND b2.id <= batches.id)
	) WHERE batch_number = 0;
	CREATE INDEX idx_batches_created_ns ON batches(created_ns DESC);
	CREATE INDEX idx_events_run_id ON run_events(run_id, id DESC);`,
	// 4: when cancellation was requested, so the watchdog can force a stuck
	// CANCEL_REQUESTED run to CANCELLED instead of leaking its slot forever.
	`ALTER TABLE runs ADD COLUMN cancel_requested_at TEXT NOT NULL DEFAULT '';`,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	for i, m := range migrations {
		v := i + 1
		if v <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, v, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// ---- Runs ----

const runCols = `id,batch_id,row_id,card_index,title,provider,model,effort,permission,staged,attach_secrets,state,stage,
prompt_cipher,prompt_hash,prompt_preview,secret_version_id,worktree_path,branch,provider_session_id,provider_turn_id,
provider_sub_session,provider_title,continuation_count,audit_cycles,verify_cycles,error_code,error_msg,final_outcome,
meaningful_progress,created_at,queued_at,started_at,finished_at,next_eligible_at,queue_wait_ns,active_ns,quota_wait_ns,
dispatch_uuid,attempt,sort_key,cwd,cancel_requested_at`

func scanRun(sc interface{ Scan(...any) error }) (*model.Run, error) {
	r := &model.Run{}
	var staged, attach, progress int
	var createdAt, queuedAt, startedAt, finishedAt, nextElig, cancelReq string
	err := sc.Scan(&r.ID, &r.BatchID, &r.RowID, &r.CardIndex, &r.Title, &r.Provider, &r.Model, &r.Effort, &r.Permission,
		&staged, &attach, &r.State, &r.Stage,
		&r.PromptCipherStore, &r.PromptHash, &r.PromptPreview, &r.SecretVersionID, &r.WorktreePath, &r.Branch,
		&r.ProviderSessionID, &r.ProviderTurnID, &r.ProviderSubSession, &r.ProviderTitle,
		&r.ContinuationCount, &r.AuditCycles, &r.VerifyCycles, &r.ErrorCode, &r.ErrorMsg, &r.FinalOutcome,
		&progress, &createdAt, &queuedAt, &startedAt, &finishedAt, &nextElig,
		&r.QueueWaitNS, &r.ActiveNS, &r.QuotaWaitNS, &r.DispatchUUID, &r.Attempt, &r.SortKey, &r.Cwd, &cancelReq)
	if err != nil {
		return nil, err
	}
	r.Staged = staged != 0
	r.AttachSecrets = attach != 0
	r.MeaningfulProgress = progress != 0
	r.CreatedAt = parseTime(createdAt)
	r.QueuedAt = parseTime(queuedAt)
	r.StartedAt = parseTime(startedAt)
	r.FinishedAt = parseTime(finishedAt)
	r.CancelRequestedAt = parseTime(cancelReq)
	if nextElig != "" {
		t := parseTime(nextElig)
		r.NextEligibleAt = &t
	}
	return r, nil
}

// CreateRun inserts a new run.
// runPlaceholders is derived from runCols so adding a column cannot silently
// desynchronise the INSERT (it used to be a hand-counted strings.Repeat).
var runPlaceholders = placeholdersFor(runCols)

func placeholdersFor(cols string) string {
	n := len(strings.Split(strings.ReplaceAll(cols, "\n", ""), ","))
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func runInsertArgs(r *model.Run) []any {
	return []any{
		r.ID, r.BatchID, r.RowID, r.CardIndex, r.Title, r.Provider, r.Model, r.Effort, r.Permission,
		b2i(r.Staged), b2i(r.AttachSecrets), string(r.State), string(r.Stage),
		r.PromptCipherStore, r.PromptHash, r.PromptPreview, r.SecretVersionID, r.WorktreePath, r.Branch,
		r.ProviderSessionID, r.ProviderTurnID, r.ProviderSubSession, r.ProviderTitle,
		r.ContinuationCount, r.AuditCycles, r.VerifyCycles, r.ErrorCode, r.ErrorMsg, r.FinalOutcome,
		b2i(r.MeaningfulProgress), fmtTime(r.CreatedAt), fmtTime(r.QueuedAt), fmtTime(r.StartedAt), fmtTime(r.FinishedAt), fmtTimeP(r.NextEligibleAt),
		r.QueueWaitNS, r.ActiveNS, r.QuotaWaitNS, r.DispatchUUID, r.Attempt, r.SortKey, r.Cwd,
		fmtTime(r.CancelRequestedAt),
	}
}

func (s *Store) CreateRun(r *model.Run) error {
	_, err := s.db.Exec(`INSERT INTO runs(`+runCols+`) VALUES(`+runPlaceholders+`)`, runInsertArgs(r)...)
	return err
}

// UpdateRun persists the mutable fields of a run.
//
// provider/model/effort/permission/staged/attach_secrets and sort_key were
// missing from this statement, so UpdateRun (queue edits) and ReorderQueue
// (drag to reorder, move to end) silently wrote nothing at all.
func (s *Store) UpdateRun(r *model.Run) error {
	_, err := s.db.Exec(`UPDATE runs SET state=?, stage=?, title=?, provider=?, model=?, effort=?, permission=?,
		staged=?, attach_secrets=?, sort_key=?, worktree_path=?, branch=?, provider_session_id=?,
		provider_turn_id=?, provider_sub_session=?, provider_title=?, continuation_count=?, audit_cycles=?, verify_cycles=?,
		error_code=?, error_msg=?, final_outcome=?, meaningful_progress=?, queued_at=?, started_at=?, finished_at=?,
		next_eligible_at=?, queue_wait_ns=?, active_ns=?, quota_wait_ns=?, dispatch_uuid=?, attempt=?,
		prompt_cipher=?, prompt_hash=?, prompt_preview=?, secret_version_id=?, cwd=?, cancel_requested_at=?
		WHERE id=?`,
		string(r.State), string(r.Stage), r.Title, r.Provider, r.Model, r.Effort, r.Permission,
		b2i(r.Staged), b2i(r.AttachSecrets), r.SortKey, r.WorktreePath, r.Branch, r.ProviderSessionID,
		r.ProviderTurnID, r.ProviderSubSession, r.ProviderTitle, r.ContinuationCount, r.AuditCycles, r.VerifyCycles,
		r.ErrorCode, r.ErrorMsg, r.FinalOutcome, b2i(r.MeaningfulProgress), fmtTime(r.QueuedAt), fmtTime(r.StartedAt), fmtTime(r.FinishedAt),
		fmtTimeP(r.NextEligibleAt), r.QueueWaitNS, r.ActiveNS, r.QuotaWaitNS, r.DispatchUUID, r.Attempt,
		r.PromptCipherStore, r.PromptHash, r.PromptPreview, r.SecretVersionID, r.Cwd, fmtTime(r.CancelRequestedAt),
		r.ID)
	return err
}

// GetRun loads one run by id.
func (s *Store) GetRun(id string) (*model.Run, error) {
	row := s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, id)
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// GetRunByProviderSession finds a run by provider native session id.
func (s *Store) GetRunByProviderSession(provider, nativeID string) (*model.Run, error) {
	row := s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE provider=? AND provider_session_id=? AND provider_session_id!=''`, provider, nativeID)
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ListRunsByStates returns runs in the given states ordered by sort key.
func (s *Store) ListRunsByStates(states ...model.RunState) ([]*model.Run, error) {
	if len(states) == 0 {
		return nil, nil
	}
	q := strings.Repeat("?,", len(states))
	q = q[:len(q)-1]
	args := make([]any, len(states))
	for i, st := range states {
		args[i] = string(st)
	}
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs WHERE state IN (`+q+`) ORDER BY sort_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRunsByBatch returns all runs of a batch ordered by row/card.
func (s *Store) ListRunsByBatch(batchID string) ([]*model.Run, error) {
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs WHERE batch_id=? ORDER BY sort_key`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TransitionRun validates and persists a state transition atomically with an
// audit event. It returns an error for illegal transitions.
func (s *Store) TransitionRun(runID string, to model.RunState, evt *model.RunEvent, mut func(*model.Run)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, runID)
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return fmt.Errorf("run %s not found", runID)
	}
	if err != nil {
		return err
	}
	from := r.State
	if !model.CanTransition(from, to) {
		return fmt.Errorf("illegal transition %s -> %s for run %s", from, to, runID)
	}
	r.State = to
	if mut != nil {
		mut(r)
	}
	if _, err := tx.Exec(`UPDATE runs SET state=?, started_at=?, finished_at=?, queued_at=?, next_eligible_at=?, worktree_path=?, branch=?,
		provider_session_id=?, provider_turn_id=?, provider_title=?, continuation_count=?, error_code=?, error_msg=?, final_outcome=?,
		meaningful_progress=?, stage=?, active_ns=?, quota_wait_ns=?, queue_wait_ns=?, dispatch_uuid=?, attempt=?, cancel_requested_at=? WHERE id=?`,
		string(r.State), fmtTime(r.StartedAt), fmtTime(r.FinishedAt), fmtTime(r.QueuedAt), fmtTimeP(r.NextEligibleAt), r.WorktreePath, r.Branch,
		r.ProviderSessionID, r.ProviderTurnID, r.ProviderTitle, r.ContinuationCount, r.ErrorCode, r.ErrorMsg, r.FinalOutcome,
		b2i(r.MeaningfulProgress), string(r.Stage), r.ActiveNS, r.QuotaWaitNS, r.QueueWaitNS, r.DispatchUUID, r.Attempt, fmtTime(r.CancelRequestedAt), runID); err != nil {
		return err
	}
	if evt != nil {
		if _, err := tx.Exec(`INSERT INTO run_events(run_id, ts, type, data) VALUES(?,?,?,?)`,
			runID, fmtTime(evt.Timestamp), string(evt.Type), evt.Data); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO run_events(run_id, ts, type, data) VALUES(?,?,?,?)`,
		runID, fmtTime(time.Now().UTC()), string(model.EventStateChanged), fmt.Sprintf(`{"from":"%s","to":"%s"}`, from, to)); err != nil {
		return err
	}
	return tx.Commit()
}

// AddRunEvent appends a timeline event.
func (s *Store) AddRunEvent(runID string, typ model.RunEventType, data string) error {
	_, err := s.db.Exec(`INSERT INTO run_events(run_id, ts, type, data) VALUES(?,?,?,?)`,
		runID, fmtTime(time.Now().UTC()), string(typ), data)
	return err
}

// ListRunEvents returns the timeline for a run.
func (s *Store) ListRunEvents(runID string, limit int) ([]*model.RunEvent, error) {
	// The timeline wants the NEWEST `limit` events; `ORDER BY id LIMIT ?`
	// returned the oldest ones, so a long run's timeline froze at its start.
	// Fetched newest-first, then reversed so callers still read chronologically.
	rows, err := s.db.Query(`SELECT id, run_id, ts, type, data FROM run_events WHERE run_id=? ORDER BY id DESC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.RunEvent
	for rows.Next() {
		e := &model.RunEvent{}
		var ts string
		if err := rows.Scan(&e.ID, &e.RunID, &ts, &e.Type, &e.Data); err != nil {
			return nil, err
		}
		e.Timestamp = parseTime(ts)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// SortKeyGap is the spacing between adjacent queue positions. Sparse keys mean
// a run can be inserted between two neighbours without rewriting the queue.
const SortKeyGap int64 = 1024

// queueOrderStates are the states whose runs are still waiting to dispatch and
// therefore participate in queue ordering.
var queueOrderStates = []model.RunState{
	model.StateQueued, model.StateWaitingForSlot,
	model.StateWaitingForExternal, model.StateWaitingForQuota,
}

func stateArgs(states []model.RunState) (string, []any) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := make([]any, 0, len(states))
	for _, st := range states {
		args = append(args, string(st))
	}
	return ph, args
}

// NextSortKey allocates a position at the BACK of the queue.
func (s *Store) NextSortKey() (int64, error) {
	var k sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(sort_key) FROM runs`).Scan(&k); err != nil {
		return 0, err
	}
	if !k.Valid {
		return SortKeyGap, nil
	}
	return k.Int64 + SortKeyGap, nil
}

// FrontSortKey returns a key that sorts ahead of every run still waiting to
// dispatch — what "Run Next" actually means. The previous implementation used
// time.Now().UnixNano(), which is astronomically larger than the small
// sequential keys and therefore sent the run to the BACK of the queue.
func (s *Store) FrontSortKey() (int64, error) {
	ph, args := stateArgs(queueOrderStates)
	var k sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(sort_key) FROM runs WHERE state IN (`+ph+`)`, args...).Scan(&k); err != nil {
		return 0, err
	}
	if !k.Valid {
		return s.NextSortKey()
	}
	return k.Int64 - SortKeyGap, nil
}

// SortKeyBefore returns a key placing a run immediately before target, first
// renormalising the queue if the neighbouring keys have no gap left.
func (s *Store) SortKeyBefore(targetRunID string) (int64, error) {
	key, err := s.sortKeyBefore(targetRunID)
	if err == nil && key != 0 {
		return key, nil
	}
	if err != nil {
		return 0, err
	}
	// No gap: respace the queue and try once more.
	if err := s.RenumberQueue(); err != nil {
		return 0, err
	}
	key, err = s.sortKeyBefore(targetRunID)
	if err != nil {
		return 0, err
	}
	if key == 0 {
		return 0, fmt.Errorf("cannot allocate a queue position before %s", targetRunID)
	}
	return key, nil
}

// sortKeyBefore returns 0 (no error) when the neighbours are adjacent.
func (s *Store) sortKeyBefore(targetRunID string) (int64, error) {
	var target int64
	if err := s.db.QueryRow(`SELECT sort_key FROM runs WHERE id=?`, targetRunID).Scan(&target); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("run %s not found", targetRunID)
		}
		return 0, err
	}
	ph, args := stateArgs(queueOrderStates)
	args = append(args, target)
	var prev sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(sort_key) FROM runs WHERE state IN (`+ph+`) AND sort_key < ?`, args...).Scan(&prev); err != nil {
		return 0, err
	}
	if !prev.Valid {
		return target - SortKeyGap, nil
	}
	if target-prev.Int64 < 2 {
		return 0, nil // exhausted
	}
	return prev.Int64 + (target-prev.Int64)/2, nil
}

// RenumberQueue respaces every waiting run onto fresh SortKeyGap boundaries,
// preserving the current order.
func (s *Store) RenumberQueue() error {
	ph, args := stateArgs(queueOrderStates)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM runs WHERE state IN (`+ph+`) ORDER BY sort_key, created_at, id`, args...)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE runs SET sort_key=? WHERE id=?`, int64(i+1)*SortKeyGap, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- Attempts ----

// RecordAttemptStart inserts an attempt row (idempotency record).
func (s *Store) RecordAttemptStart(runID string, attemptNo int, dispatchUUID string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO run_attempts(run_id, attempt_no, dispatch_uuid, started_at) VALUES(?,?,?,?)`,
		runID, attemptNo, dispatchUUID, now())
	return err
}

// FinishAttempt records the attempt outcome.
func (s *Store) FinishAttempt(runID string, attemptNo int, session, turn, outcome string, progress bool) error {
	_, err := s.db.Exec(`UPDATE run_attempts SET ended_at=?, provider_session=?, provider_turn=?, outcome=?, meaningful_progress=? WHERE run_id=? AND attempt_no=?`,
		now(), session, turn, outcome, b2i(progress), runID, attemptNo)
	return err
}

// GetAttempt returns the attempt record if present.
func (s *Store) GetAttempt(runID string, attemptNo int) (dispatchUUID, session string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT dispatch_uuid, provider_session FROM run_attempts WHERE run_id=? AND attempt_no=?`, runID, attemptNo).
		Scan(&dispatchUUID, &session)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return dispatchUUID, session, err == nil, err
}

// ---- Batches / Rows ----

// CreateBatch inserts a batch, assigning its monotonic number.
func (s *Store) CreateBatch(b *model.Batch) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertBatchTx(tx, b); err != nil {
		return err
	}
	return tx.Commit()
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func insertBatchTx(tx execer, b *model.Batch) error {
	if b.Number == 0 {
		if err := tx.QueryRow(`SELECT COALESCE(MAX(batch_number),0)+1 FROM batches`).Scan(&b.Number); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO batches(id,title,status,base_cwd,base_branch,base_commit,integration_branch,integration_wt_path,created_at,created_ns,applied,implicit,batch_number)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.Title, string(b.Status), b.BaseCwd, b.BaseBranch, b.BaseCommit, b.IntegrationBranch, b.IntegrationWTPath,
		fmtTime(b.CreatedAt), b.CreatedAt.UTC().UnixNano(), b2i(b.Applied), b2i(b.Implicit), b.Number)
	return err
}

// CreateBatchAtomic inserts a batch with all of its rows and runs in a single
// transaction. ApplyBatch used to insert them one at a time, so any failure
// part-way left a half-created batch behind.
func (s *Store) CreateBatchAtomic(b *model.Batch, rows []*model.Row, runs []*model.Run, event func(runID string) (model.RunEventType, string)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertBatchTx(tx, b); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO batch_rows(id,batch_id,position,mode,status) VALUES(?,?,?,?,?)`,
			r.ID, r.BatchID, r.Position, string(r.Mode), string(r.Status)); err != nil {
			return err
		}
	}
	for _, r := range runs {
		if _, err := tx.Exec(`INSERT INTO runs(`+runCols+`) VALUES(`+runPlaceholders+`)`, runInsertArgs(r)...); err != nil {
			return err
		}
		if event != nil {
			typ, data := event(r.ID)
			if _, err := tx.Exec(`INSERT INTO run_events(run_id, ts, type, data) VALUES(?,?,?,?)`,
				r.ID, now(), string(typ), data); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// UpdateBatch persists mutable batch fields.
func (s *Store) UpdateBatch(b *model.Batch) error {
	_, err := s.db.Exec(`UPDATE batches SET status=?, started_at=?, finished_at=?, integration_branch=?, integration_wt_path=?, applied=? WHERE id=?`,
		string(b.Status), fmtTime(b.StartedAt), fmtTime(b.FinishedAt), b.IntegrationBranch, b.IntegrationWTPath, b2i(b.Applied), b.ID)
	return err
}

// GetBatch loads a batch.
func (s *Store) GetBatch(id string) (*model.Batch, error) {
	var b model.Batch
	var status string
	var created, started, finished string
	var applied, implicit int
	err := s.db.QueryRow(`SELECT `+batchCols+` FROM batches WHERE id=?`, id).
		Scan(&b.ID, &b.Title, &status, &b.BaseCwd, &b.BaseBranch, &b.BaseCommit, &b.IntegrationBranch, &b.IntegrationWTPath, &created, &started, &finished, &applied, &implicit, &b.Number)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.Status = model.BatchStatus(status)
	b.CreatedAt = parseTime(created)
	b.StartedAt = parseTime(started)
	b.FinishedAt = parseTime(finished)
	b.Applied = applied != 0
	b.Implicit = implicit != 0
	return &b, nil
}

// ListBatches returns batches newest first, optionally filtered by status.
func (s *Store) ListBatches(limit int) ([]*model.Batch, error) {
	// Ordered by an integer epoch column: created_at is RFC3339Nano text whose
	// trailing zeros are stripped, so lexicographic order is not chronological.
	rows, err := s.db.Query(`SELECT `+batchCols+` FROM batches ORDER BY created_ns DESC, batch_number DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBatches(rows)
}

const batchCols = `id,title,status,base_cwd,base_branch,base_commit,integration_branch,integration_wt_path,created_at,started_at,finished_at,applied,implicit,batch_number`

func scanBatches(rows *sql.Rows) ([]*model.Batch, error) {
	var out []*model.Batch
	for rows.Next() {
		var b model.Batch
		var status, created, started, finished string
		var applied, implicit int
		if err := rows.Scan(&b.ID, &b.Title, &status, &b.BaseCwd, &b.BaseBranch, &b.BaseCommit, &b.IntegrationBranch, &b.IntegrationWTPath, &created, &started, &finished, &applied, &implicit, &b.Number); err != nil {
			return nil, err
		}
		b.Status = model.BatchStatus(status)
		b.CreatedAt = parseTime(created)
		b.StartedAt = parseTime(started)
		b.FinishedAt = parseTime(finished)
		b.Applied = applied != 0
		b.Implicit = implicit != 0
		out = append(out, &b)
	}
	return out, rows.Err()
}

// ListActiveBatches returns QUEUED and RUNNING batches in dispatch order.
//
// Order is the batch's lowest run sort key, so the queue order the user sees
// (and edits with Run Next / drag-to-reorder) is the order the scheduler
// actually dispatches in. Ordering by creation time instead would make sort
// keys meaningless for the normal queue, where every run is its own implicit
// batch. The scheduler must not reuse ListBatches, which is newest-first for
// the UI; that it ever worked depended on identical created_at values tying
// and falling back to insertion order.
func (s *Store) ListActiveBatches() ([]*model.Batch, error) {
	rows, err := s.db.Query(`SELECT ` + prefixed(batchCols, "b") + ` FROM batches b
		LEFT JOIN (SELECT batch_id, MIN(sort_key) AS min_sort FROM runs GROUP BY batch_id) r ON r.batch_id = b.id
		WHERE b.status IN ('QUEUED','RUNNING')
		ORDER BY COALESCE(r.min_sort, 9223372036854775807) ASC, b.batch_number ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBatches(rows)
}

// prefixed qualifies a comma-separated column list with a table alias.
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ",")
}

// CreateRow inserts a batch row.
func (s *Store) CreateRow(r *model.Row) error {
	_, err := s.db.Exec(`INSERT INTO batch_rows(id,batch_id,position,mode,status) VALUES(?,?,?,?,?)`,
		r.ID, r.BatchID, r.Position, string(r.Mode), string(r.Status))
	return err
}

// UpdateRowStatus persists row status.
func (s *Store) UpdateRowStatus(rowID string, st model.RowStatus) error {
	_, err := s.db.Exec(`UPDATE batch_rows SET status=? WHERE id=?`, string(st), rowID)
	return err
}

// ListRows returns the ordered rows of a batch.
func (s *Store) ListRows(batchID string) ([]*model.Row, error) {
	rows, err := s.db.Query(`SELECT id,batch_id,position,mode,status FROM batch_rows WHERE batch_id=? ORDER BY position`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Row
	for rows.Next() {
		var r model.Row
		var mode, status string
		if err := rows.Scan(&r.ID, &r.BatchID, &r.Position, &mode, &status); err != nil {
			return nil, err
		}
		r.Mode = model.RowMode(mode)
		if status == "" {
			status = string(model.RowPending)
		}
		r.Status = model.RowStatus(status)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// GetRow loads a row.
func (s *Store) GetRow(id string) (*model.Row, error) {
	var r model.Row
	var mode, status string
	err := s.db.QueryRow(`SELECT id,batch_id,position,mode,status FROM batch_rows WHERE id=?`, id).
		Scan(&r.ID, &r.BatchID, &r.Position, &mode, &status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Mode = model.RowMode(mode)
	r.Status = model.RowStatus(status)
	return &r, nil
}

// ---- Provider / external sessions ----

// UpsertProviderSession records the latest observation of a native session.
func (s *Store) UpsertProviderSession(p *model.ProviderSession) error {
	_, err := s.db.Exec(`INSERT INTO provider_sessions(provider,native_id,run_id,kind,cwd,model,title,last_event,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(provider,native_id) DO UPDATE SET run_id=excluded.run_id, kind=excluded.kind, cwd=excluded.cwd,
		model=excluded.model, title=excluded.title, last_event=excluded.last_event, last_seen_at=excluded.last_seen_at`,
		p.Provider, p.NativeID, p.RunID, p.Kind, p.Cwd, p.Model, p.Title, p.LastEvent, fmtTime(p.LastSeenAt))
	return err
}

// ListProviderSessions returns sessions observed since the given time.
func (s *Store) ListProviderSessions(provider string) ([]*model.ProviderSession, error) {
	rows, err := s.db.Query(`SELECT provider,native_id,run_id,kind,cwd,model,title,last_event,last_seen_at FROM provider_sessions WHERE provider=? ORDER BY last_seen_at DESC`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ProviderSession
	for rows.Next() {
		var p model.ProviderSession
		var ts string
		if err := rows.Scan(&p.Provider, &p.NativeID, &p.RunID, &p.Kind, &p.Cwd, &p.Model, &p.Title, &p.LastEvent, &ts); err != nil {
			return nil, err
		}
		p.LastSeenAt = parseTime(ts)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// DeleteProviderSession removes one session observation (stale clear).
func (s *Store) DeleteProviderSession(provider, nativeID string) error {
	_, err := s.db.Exec(`DELETE FROM provider_sessions WHERE provider=? AND native_id=?`, provider, nativeID)
	return err
}

// ---- Quota ----

// QuotaWindowRow mirrors the normalized quota model.
type QuotaWindowRow struct {
	Provider    string  `json:"provider"`
	WindowID    string  `json:"windowId"`
	Name        string  `json:"name"`
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    string  `json:"resetsAt"`
	Exhausted   bool    `json:"exhausted"`
	Source      string  `json:"source"`
	Confidence  string  `json:"confidence"`
}

// UpsertQuotaWindow records one quota observation.
func (s *Store) UpsertQuotaWindow(w QuotaWindowRow) error {
	_, err := s.db.Exec(`INSERT INTO quota_windows(provider,window_id,name,used_percent,resets_at,exhausted,source,confidence,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(provider,window_id) DO UPDATE SET name=excluded.name, used_percent=excluded.used_percent,
		resets_at=excluded.resets_at, exhausted=excluded.exhausted, source=excluded.source, confidence=excluded.confidence, updated_at=excluded.updated_at`,
		w.Provider, w.WindowID, w.Name, w.UsedPercent, w.ResetsAt, b2i(w.Exhausted), w.Source, w.Confidence, now())
	return err
}

// ListQuotaWindows returns current observations for a provider.
func (s *Store) ListQuotaWindows(provider string) ([]QuotaWindowRow, error) {
	rows, err := s.db.Query(`SELECT provider,window_id,name,used_percent,resets_at,exhausted,source,confidence FROM quota_windows WHERE provider=?`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaWindowRow
	for rows.Next() {
		var w QuotaWindowRow
		var ex int
		if err := rows.Scan(&w.Provider, &w.WindowID, &w.Name, &w.UsedPercent, &w.ResetsAt, &ex, &w.Source, &w.Confidence); err != nil {
			return nil, err
		}
		w.Exhausted = ex != 0
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---- Settings & secrets ----

// GetSetting returns a JSON setting value.
func (s *Store) GetSetting(key string, into any) (bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if into != nil {
		if err := json.Unmarshal([]byte(v), into); err != nil {
			return true, err
		}
	}
	return true, nil
}

// SetSetting stores a JSON setting.
func (s *Store) SetSetting(key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(b))
	return err
}

// SecretVersion is a stored encrypted secret block version.
type SecretVersion struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Hash      string    `json:"hash"`
	Preview   string    `json:"preview"`
	cipher    string
}

// Cipher returns the encrypted blob.
func (v *SecretVersion) Cipher() string { return v.cipher }

// LatestSecretVersion returns the newest stored secret block version.
func (s *Store) LatestSecretVersion() (*SecretVersion, error) {
	var v SecretVersion
	var created string
	err := s.db.QueryRow(`SELECT id, created_at, cipher, hash, preview FROM secret_versions ORDER BY id DESC LIMIT 1`).
		Scan(&v.ID, &created, &v.cipher, &v.Hash, &v.Preview)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.CreatedAt = parseTime(created)
	return &v, nil
}

// AddSecretVersion stores a new encrypted secret block version.
func (s *Store) AddSecretVersion(cipher, hash, preview string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO secret_versions(created_at,cipher,hash,preview) VALUES(?,?,?,?)`, now(), cipher, hash, preview)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetSecretVersion loads one version by id.
func (s *Store) GetSecretVersion(id int64) (*SecretVersion, error) {
	var v SecretVersion
	var created string
	err := s.db.QueryRow(`SELECT id, created_at, cipher, hash, preview FROM secret_versions WHERE id=?`, id).
		Scan(&v.ID, &created, &v.cipher, &v.Hash, &v.Preview)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.CreatedAt = parseTime(created)
	return &v, nil
}

// ---- Worktrees ----

// WorktreeRecord tracks created worktrees.
type WorktreeRecord struct {
	ID      int64
	BatchID string
	RunID   string
	Path    string
	Branch  string
	Kind    string // card | integration
	Status  string // ACTIVE | PRESERVED | CLEANED
}

// AddWorktree records a worktree.
func (s *Store) AddWorktree(w *WorktreeRecord) error {
	res, err := s.db.Exec(`INSERT INTO worktrees(batch_id,run_id,path,branch,kind,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		w.BatchID, w.RunID, w.Path, w.Branch, w.Kind, w.Status, now())
	if err != nil {
		return err
	}
	w.ID, _ = res.LastInsertId()
	return nil
}

// UpdateWorktreeStatus updates status.
func (s *Store) UpdateWorktreeStatus(path, status string) error {
	_, err := s.db.Exec(`UPDATE worktrees SET status=? WHERE path=?`, status, path)
	return err
}

// ListWorktrees returns worktrees for a batch.
func (s *Store) ListWorktrees(batchID string) ([]*WorktreeRecord, error) {
	rows, err := s.db.Query(`SELECT id,batch_id,run_id,path,branch,kind,status FROM worktrees WHERE batch_id=?`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WorktreeRecord
	for rows.Next() {
		var w WorktreeRecord
		if err := rows.Scan(&w.ID, &w.BatchID, &w.RunID, &w.Path, &w.Branch, &w.Kind, &w.Status); err != nil {
			return nil, err
		}
		out = append(out, &w)
	}
	return out, rows.Err()
}

// ---- Anchors ----

// AnchorRecord is one window-anchor claim.
type AnchorRecord struct {
	ID          int64
	Provider    string
	WindowReset string
	AttemptID   string
	StartedAt   string
	CompletedAt string
	SessionRef  string
}

// ClaimAnchor atomically claims the anchor for a window. Returns false when
// the window was already claimed (exactly-once).
func (s *Store) ClaimAnchor(provider, windowReset, attemptID string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO anchors(provider,window_reset,attempt_id,started_at) VALUES(?,?,?,?)`,
		provider, windowReset, attemptID, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return false, nil
		}
		return false, err
	}
	_ = res
	return true, nil
}

// CompleteAnchor marks the anchor finished.
func (s *Store) CompleteAnchor(provider, windowReset, sessionRef string) error {
	_, err := s.db.Exec(`UPDATE anchors SET completed_at=?, session_ref=? WHERE provider=? AND window_reset=?`, now(), sessionRef, provider, windowReset)
	return err
}

// LatestAnchor returns the newest anchor for a provider.
func (s *Store) LatestAnchor(provider string) (*AnchorRecord, error) {
	var a AnchorRecord
	err := s.db.QueryRow(`SELECT id,provider,window_reset,attempt_id,started_at,completed_at,session_ref FROM anchors WHERE provider=? ORDER BY id DESC LIMIT 1`, provider).
		Scan(&a.ID, &a.Provider, &a.WindowReset, &a.AttemptID, &a.StartedAt, &a.CompletedAt, &a.SessionRef)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ---- Service state ----

// SetServiceState persists a service key/value.
func (s *Store) SetServiceState(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO service_state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// GetServiceState reads a service key.
func (s *Store) GetServiceState(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM service_state WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func fmtTimeP(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtTime(*t)
}

// ---- Bulk reads (snapshot path) ----

// ListRunsForBatches returns every run for the given batches in one query,
// bucketed by batch id. The snapshot handler used to issue one query per
// batch, i.e. ~200 SQLite round-trips for a 100-batch snapshot on a 1 Hz path.
func (s *Store) ListRunsForBatches(batchIDs []string) (map[string][]*model.Run, error) {
	out := map[string][]*model.Run{}
	if len(batchIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(batchIDs)), ",")
	args := make([]any, 0, len(batchIDs))
	for _, id := range batchIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs WHERE batch_id IN (`+ph+`) ORDER BY sort_key, card_index`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out[r.BatchID] = append(out[r.BatchID], r)
	}
	return out, rows.Err()
}

// ListRowsForBatches returns every row for the given batches in one query.
func (s *Store) ListRowsForBatches(batchIDs []string) (map[string][]*model.Row, error) {
	out := map[string][]*model.Row{}
	if len(batchIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(batchIDs)), ",")
	args := make([]any, 0, len(batchIDs))
	for _, id := range batchIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT id,batch_id,position,mode,status FROM batch_rows WHERE batch_id IN (`+ph+`) ORDER BY batch_id, position`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r model.Row
		var mode, status string
		if err := rows.Scan(&r.ID, &r.BatchID, &r.Position, &mode, &status); err != nil {
			return nil, err
		}
		r.Mode = model.RowMode(mode)
		r.Status = model.RowStatus(status)
		out[r.BatchID] = append(out[r.BatchID], &r)
	}
	return out, rows.Err()
}

// ---- Secrets ----

// DeleteAllSecretVersions permanently removes every stored secret block.
//
// "Delete Shared Testing & Sudo Block" used to set an unread `deleted` flag
// while the ciphertext stayed in the table and kept being attached to runs.
// This actually deletes it.
func (s *Store) DeleteAllSecretVersions() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Overwrite the ciphertext before dropping the rows so the bytes are not
	// left behind in a free page for a casual file inspection to recover.
	if _, err := tx.Exec(`UPDATE secret_versions SET cipher='', hash='', preview=''`); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM secret_versions`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// Runs must stop referencing a version that no longer exists.
	if _, err := tx.Exec(`UPDATE runs SET secret_version_id='', attach_secrets=0 WHERE secret_version_id != ''`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// VACUUM cannot run inside a transaction; it reclaims the pages that held
	// the ciphertext.
	_, _ = s.db.Exec(`VACUUM`)
	return n, nil
}

// HasSecretBlock reports whether a block exists, without decrypting it.
// The composer used to call revealSecretBlock on every mount purely to answer
// this boolean, decrypting the secret and shipping it over the socket.
func (s *Store) HasSecretBlock() (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM secret_versions`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- Retention ----

// RetentionPolicy bounds unbounded growth of the event log.
type RetentionPolicy struct {
	// KeepEventsPerRun caps the timeline of any single run.
	KeepEventsPerRun int
	// DropTerminalAfter drops events for runs that finished long ago.
	DropTerminalAfter time.Duration
}

// DefaultRetention keeps timelines useful without letting run_events grow
// forever.
func DefaultRetention() RetentionPolicy {
	return RetentionPolicy{KeepEventsPerRun: 500, DropTerminalAfter: 30 * 24 * time.Hour}
}

// Prune applies the retention policy and reports how many event rows went.
func (s *Store) Prune(p RetentionPolicy, now time.Time) (int64, error) {
	var total int64
	if p.DropTerminalAfter > 0 {
		cutoff := fmtTime(now.Add(-p.DropTerminalAfter))
		res, err := s.db.Exec(`DELETE FROM run_events WHERE run_id IN (
			SELECT id FROM runs WHERE finished_at != '' AND finished_at < ?)`, cutoff)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if p.KeepEventsPerRun > 0 {
		res, err := s.db.Exec(`DELETE FROM run_events WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY id DESC) AS rn
				FROM run_events
			) WHERE rn > ?)`, p.KeepEventsPerRun)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// Compact prunes and then reclaims disk space. Exposed as a maintenance RPC.
func (s *Store) Compact(p RetentionPolicy, now time.Time) (int64, error) {
	n, err := s.Prune(p, now)
	if err != nil {
		return n, err
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return n, err
	}
	return n, nil
}
