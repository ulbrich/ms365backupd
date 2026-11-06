// Package history stores one JSON line per backup run in <state_dir>/runs.jsonl.
// It is independent of the backup disk so weekly statistics survive disk swaps.
package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ulbrich/ms365backupd/internal/status"
)

const maxRuns = 2000

// History is safe for concurrent use within one process.
type History struct {
	mu   sync.Mutex
	path string
}

// Open returns the history in stateDir.
func Open(stateDir string) *History {
	return &History{path: filepath.Join(stateDir, "runs.jsonl")}
}

// Append adds a run, trimming old entries occasionally.
func (h *History) Append(r *status.RunResult) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(h.path); err == nil && st.Size() > 8<<20 {
		return h.trimLocked()
	}
	return nil
}

func (h *History) trimLocked() error {
	runs, err := h.readLocked()
	if err != nil {
		return err
	}
	if len(runs) > maxRuns {
		runs = runs[len(runs)-maxRuns:]
	}
	tmp := h.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, r := range runs {
		b, _ := json.Marshal(r)
		w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, h.path)
}

// All returns all runs, oldest first. Corrupt lines (e.g. after power loss) are skipped.
func (h *History) All() ([]status.RunResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readLocked()
}

func (h *History) readLocked() ([]status.RunResult, error) {
	f, err := os.Open(h.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []status.RunResult
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r status.RunResult
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.ID != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// Since returns runs started at or after t.
func (h *History) Since(t time.Time) ([]status.RunResult, error) {
	all, err := h.All()
	if err != nil {
		return nil, err
	}
	var out []status.RunResult
	for _, r := range all {
		if !r.Started.Before(t) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Last returns the n most recent runs, newest first.
func (h *History) Last(n int) ([]status.RunResult, error) {
	all, err := h.All()
	if err != nil {
		return nil, err
	}
	var out []status.RunResult
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out, nil
}

// LastSuccess returns the most recent successful run, optionally for one disk.
func (h *History) LastSuccess(diskID string) *status.RunResult {
	all, _ := h.All()
	for i := len(all) - 1; i >= 0; i-- {
		r := all[i]
		if r.Status == status.StatusSuccess && (diskID == "" || r.DiskID == diskID) {
			return &r
		}
	}
	return nil
}
