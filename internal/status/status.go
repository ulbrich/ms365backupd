// Package status defines the data shared between the backup engine, the daemon,
// the control socket clients (CLI/TUI) and the run history.
package status

import "time"

// Counters summarise work done for one service (or in total).
type Counters struct {
	New       int64 `json:"new"`
	Updated   int64 `json:"updated"`
	Unchanged int64 `json:"unchanged"`
	Deleted   int64 `json:"deleted"`
	Moved     int64 `json:"moved"`
	Skipped   int64 `json:"skipped"` // excluded or permanently unavailable items
	Errors    int64 `json:"errors"`  // retryable item failures (retried next run)
	Bytes     int64 `json:"bytes"`   // bytes downloaded
}

// Add accumulates o into c.
func (c *Counters) Add(o Counters) {
	c.New += o.New
	c.Updated += o.Updated
	c.Unchanged += o.Unchanged
	c.Deleted += o.Deleted
	c.Moved += o.Moved
	c.Skipped += o.Skipped
	c.Errors += o.Errors
	c.Bytes += o.Bytes
}

// Run statuses.
const (
	StatusSuccess = "success"
	StatusPartial = "partial" // completed, but some items failed and will be retried
	StatusFailed  = "failed"
)

// RunResult is the outcome of one backup run. One JSON line per run in history.
type RunResult struct {
	ID           string              `json:"id"`
	Trigger      string              `json:"trigger"` // schedule, retry, disk-connected, manual, startup-catchup
	Started      time.Time           `json:"started"`
	Finished     time.Time           `json:"finished"`
	Status       string              `json:"status"`
	Error        string              `json:"error,omitempty"`
	NeedsLogin   bool                `json:"needs_login,omitempty"`
	DiskMissing  bool                `json:"disk_missing,omitempty"`
	Account      string              `json:"account,omitempty"`
	DiskID       string              `json:"disk_id,omitempty"`
	NewDisk      bool                `json:"new_disk,omitempty"`
	FullSync     []string            `json:"full_sync,omitempty"` // collections synced from scratch
	Services     map[string]Counters `json:"services,omitempty"`
	Totals       Counters            `json:"totals"`
	ErrorSamples []string            `json:"error_samples,omitempty"`
	DiskFree     uint64              `json:"disk_free,omitempty"`
	DiskTotal    uint64              `json:"disk_total,omitempty"`
}

// Duration of the run.
func (r *RunResult) Duration() time.Duration {
	if r.Finished.IsZero() {
		return time.Since(r.Started)
	}
	return r.Finished.Sub(r.Started)
}

// OK reports a fully successful run.
func (r *RunResult) OK() bool { return r.Status == StatusSuccess }

// Progress is a live snapshot of a running backup.
type Progress struct {
	RunID    string              `json:"run_id"`
	Trigger  string              `json:"trigger"`
	Started  time.Time           `json:"started"`
	Service  string              `json:"service"`
	Phase    string              `json:"phase"`
	Current  string              `json:"current,omitempty"`
	Queued   int64               `json:"queued"`
	Services map[string]Counters `json:"services"`
	Totals   Counters            `json:"totals"`
}

// DiskState mirrors disk.Info without importing it (keeps this package dependency free).
type DiskState struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Reason  string `json:"reason,omitempty"`
	DiskID  string `json:"disk_id,omitempty"`
	Account string `json:"account,omitempty"`
	Free    uint64 `json:"free"`
	Total   uint64 `json:"total"`
}

// Daemon is the full status returned by the control socket.
type Daemon struct {
	PID         int        `json:"pid"`
	Version     string     `json:"version"`
	Started     time.Time  `json:"started"`
	Account     string     `json:"account,omitempty"`
	Running     *Progress  `json:"running,omitempty"`
	Last        *RunResult `json:"last,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	NextRun     time.Time  `json:"next_run"`
	NextRunWhy  string     `json:"next_run_why,omitempty"`
	NextReport  *time.Time `json:"next_report,omitempty"`
	Disk        DiskState  `json:"disk"`
	AuthError   string     `json:"auth_error,omitempty"`
	ConfigPath  string     `json:"config_path"`
	Target      string     `json:"target"`
}
