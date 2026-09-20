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
dispatch_uuid,attempt,sort_key,cwd`

func scanRun(sc interface{ Scan(...any) error }) (*model.Run, error) {
	r := &model.Run{}
	var staged, attach, progress int
	var createdAt, queuedAt, startedAt, finishedAt, nextElig string
	err := sc.Scan(&r.ID, &r.BatchID, &r.RowID, &r.CardIndex, &r.Title, &r.Provider, &r.Model, &r.Effort, &r.Permission,
		&staged, &attach, &r.State, &r.Stage,
		&r.PromptCipherStore, &r.PromptHash, &r.PromptPreview, &r.SecretVersionID, &r.WorktreePath, &r.Branch,
		&r.ProviderSessionID, &r.ProviderTurnID, &r.ProviderSubSession, &r.ProviderTitle,
		&r.ContinuationCount, &r.AuditCycles, &r.VerifyCycles, &r.ErrorCode, &r.ErrorMsg, &r.FinalOutcome,
		&progress, &createdAt, &queuedAt, &startedAt, &finishedAt, &nextElig,
		&r.QueueWaitNS, &r.ActiveNS, &r.QuotaWaitNS, &r.DispatchUUID, &r.Attempt, &r.SortKey, &r.Cwd)
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
	if nextElig != "" {
		t := parseTime(nextElig)
		r.NextEligibleAt = &t
	}
	return r, nil
}

// CreateRun inserts a new run.
func (s *Store) CreateRun(r *model.Run) error {
	_, err := s.db.Exec(`INSERT INTO runs(`+runCols+`) VALUES(`+strings.Repeat("?,", 41)+`?)`,
		r.ID, r.BatchID, r.RowID, r.CardIndex, r.Title, r.Provider, r.Model, r.Effort, r.Permission,
		b2i(r.Staged), b2i(r.AttachSecrets), string(r.State), string(r.Stage),
		r.PromptCipherStore, r.PromptHash, r.PromptPreview, r.SecretVersionID, r.WorktreePath, r.Branch,
		r.ProviderSessionID, r.ProviderTurnID, r.ProviderSubSession, r.ProviderTitle,
		r.ContinuationCount, r.AuditCycles, r.VerifyCycles, r.ErrorCode, r.ErrorMsg, r.FinalOutcome,
		b2i(r.MeaningfulProgress), fmtTime(r.CreatedAt), fmtTime(r.QueuedAt), fmtTime(r.StartedAt), fmtTime(r.FinishedAt), fmtTimeP(r.NextEligibleAt),
		r.QueueWaitNS, r.ActiveNS, r.QuotaWaitNS, r.DispatchUUID, r.Attempt, r.SortKey, r.Cwd)
	return err
}

// UpdateRun persists the mutable scheduling fields of a run.
func (s *Store) UpdateRun(r *model.Run) error {
	_, err := s.db.Exec(`UPDATE runs SET state=?, stage=?, title=?, worktree_path=?, branch=?, provider_session_id=?,
		provider_turn_id=?, provider_sub_session=?, provider_title=?, continuation_count=?, audit_cycles=?, verify_cycles=?,
		error_code=?, error_msg=?, final_outcome=?, meaningful_progress=?, queued_at=?, started_at=?, finished_at=?,
		next_eligible_at=?, queue_wait_ns=?, active_ns=?, quota_wait_ns=?, dispatch_uuid=?, attempt=?, prompt_cipher=?, secret_version_id=?, cwd=?
		WHERE id=?`,
		string(r.State), string(r.Stage), r.Title, r.WorktreePath, r.Branch, r.ProviderSessionID,
		r.ProviderTurnID, r.ProviderSubSession, r.ProviderTitle, r.ContinuationCount, r.AuditCycles, r.VerifyCycles,
		r.ErrorCode, r.ErrorMsg, r.FinalOutcome, b2i(r.MeaningfulProgress), fmtTime(r.QueuedAt), fmtTime(r.StartedAt), fmtTime(r.FinishedAt),
		fmtTimeP(r.NextEligibleAt), r.QueueWaitNS, r.ActiveNS, r.QuotaWaitNS, r.DispatchUUID, r.Attempt, r.PromptCipherStore, r.SecretVersionID, r.Cwd,
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
		meaningful_progress=?, stage=?, active_ns=?, quota_wait_ns=?, queue_wait_ns=?, dispatch_uuid=?, attempt=? WHERE id=?`,
		string(r.State), fmtTime(r.StartedAt), fmtTime(r.FinishedAt), fmtTime(r.QueuedAt), fmtTimeP(r.NextEligibleAt), r.WorktreePath, r.Branch,
		r.ProviderSessionID, r.ProviderTurnID, r.ProviderTitle, r.ContinuationCount, r.ErrorCode, r.ErrorMsg, r.FinalOutcome,
		b2i(r.MeaningfulProgress), string(r.Stage), r.ActiveNS, r.QuotaWaitNS, r.QueueWaitNS, r.DispatchUUID, r.Attempt, runID); err != nil {
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
	rows, err := s.db.Query(`SELECT id, run_id, ts, type, data FROM run_events WHERE run_id=? ORDER BY id LIMIT ?`, runID, limit)
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
	return out, rows.Err()
}

// NextSortKey allocates a monotonically increasing queue position.
func (s *Store) NextSortKey() (int64, error) {
	var k int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(sort_key),0)+1 FROM runs`).Scan(&k); err != nil {
		return 0, err
	}
	return k, nil
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

// CreateBatch inserts a batch.
func (s *Store) CreateBatch(b *model.Batch) error {
	_, err := s.db.Exec(`INSERT INTO batches(id,title,status,base_cwd,base_branch,base_commit,integration_branch,integration_wt_path,created_at,applied,implicit)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.Title, string(b.Status), b.BaseCwd, b.BaseBranch, b.BaseCommit, b.IntegrationBranch, b.IntegrationWTPath, fmtTime(b.CreatedAt), b2i(b.Applied), b2i(b.Implicit))
	return err
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
	err := s.db.QueryRow(`SELECT id,title,status,base_cwd,base_branch,base_commit,integration_branch,integration_wt_path,created_at,started_at,finished_at,applied,implicit FROM batches WHERE id=?`, id).
		Scan(&b.ID, &b.Title, &status, &b.BaseCwd, &b.BaseBranch, &b.BaseCommit, &b.IntegrationBranch, &b.IntegrationWTPath, &created, &started, &finished, &applied, &implicit)
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
	rows, err := s.db.Query(`SELECT id,title,status,base_cwd,base_branch,base_commit,integration_branch,integration_wt_path,created_at,started_at,finished_at,applied,implicit FROM batches ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Batch
	for rows.Next() {
		var b model.Batch
		var status, created, started, finished string
		var applied, implicit int
		if err := rows.Scan(&b.ID, &b.Title, &status, &b.BaseCwd, &b.BaseBranch, &b.BaseCommit, &b.IntegrationBranch, &b.IntegrationWTPath, &created, &started, &finished, &applied, &implicit); err != nil {
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
