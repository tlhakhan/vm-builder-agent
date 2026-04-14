package jobs

import (
	"strings"
	"sync"
	"time"
)

// Phase describes where in the lifecycle a job currently is.
type Phase string

const (
	PhaseCloning    Phase = "cloning"
	PhaseInit       Phase = "init"
	PhaseApplying   Phase = "applying"
	PhaseDestroying Phase = "destroying"
	PhaseDone       Phase = "done"
	PhaseFailed     Phase = "failed"
)

const (
	ErrorCodeDuplicate    = "duplicate"
	ErrorCodeCreateFailed = "create_failed"
	ErrorCodeDeleteFailed = "delete_failed"
)

// Job holds the full state of one in-flight or completed terraform operation.
type Job struct {
	ID        string     `json:"job_id"`
	VMName    string     `json:"vm_name"`
	Action    string     `json:"action,omitempty"`
	Phase     Phase      `json:"phase"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	Err       string     `json:"error,omitempty"`
	ErrorCode string     `json:"error_code,omitempty"`

	mu  sync.Mutex
	log strings.Builder
}

// AppendLog appends a line to the job's log buffer (thread-safe).
func (j *Job) AppendLog(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log.WriteString(line)
	if len(line) > 0 && line[len(line)-1] != '\n' {
		j.log.WriteByte('\n')
	}
}

// Log returns a snapshot of the accumulated log output (thread-safe).
func (j *Job) Log() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.log.String()
}

// SetPhase updates the job's current phase (thread-safe).
func (j *Job) SetPhase(p Phase) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Phase = p
}

// Finish marks the job as done or failed and records the end time (thread-safe).
func (j *Job) Finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	j.EndTime = &now
	if err != nil {
		j.Phase = PhaseFailed
		j.Err = err.Error()
	} else {
		j.Phase = PhaseDone
	}
}

// FinishWithCode marks the job as done or failed and records a machine-readable
// error code when available.
func (j *Job) FinishWithCode(err error, code string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	j.EndTime = &now
	if err != nil {
		j.Phase = PhaseFailed
		j.Err = err.Error()
		j.ErrorCode = code
	} else {
		j.Phase = PhaseDone
		j.ErrorCode = ""
	}
}

// Snapshot returns a copy safe for JSON serialisation.
func (j *Job) Snapshot() JobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	snap := JobSnapshot{
		ID:        j.ID,
		VMName:    j.VMName,
		Action:    j.Action,
		Phase:     j.Phase,
		StartTime: j.StartTime,
		EndTime:   j.EndTime,
		Err:       j.Err,
		ErrorCode: j.ErrorCode,
		Log:       j.log.String(),
	}
	return snap
}

// JobSnapshot is a point-in-time, lock-free view of a Job for serialisation.
type JobSnapshot struct {
	ID        string     `json:"job_id"`
	VMName    string     `json:"vm_name"`
	Action    string     `json:"action,omitempty"`
	Phase     Phase      `json:"phase"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	Err       string     `json:"error,omitempty"`
	ErrorCode string     `json:"error_code,omitempty"`
	Log       string     `json:"log"`
}

// Tracker is a thread-safe registry of all jobs.
type Tracker struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

// NewTracker returns an initialised Tracker.
func NewTracker() *Tracker {
	return &Tracker{jobs: make(map[string]*Job)}
}

// Add registers a new job and returns it.
func (t *Tracker) Add(j *Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.jobs[j.ID] = j
}

// Get retrieves a job by ID.  Returns nil, false if not found.
func (t *Tracker) Get(id string) (*Job, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	j, ok := t.jobs[id]
	return j, ok
}

// ActiveCount returns the number of jobs that have not yet completed.
func (t *Tracker) ActiveCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := 0
	for _, j := range t.jobs {
		j.mu.Lock()
		if j.Phase != PhaseDone && j.Phase != PhaseFailed {
			n++
		}
		j.mu.Unlock()
	}
	return n
}
