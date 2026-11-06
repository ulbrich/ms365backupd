package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// NextDaily returns the next time after now at hour:minute local time.
func NextDaily(now time.Time, hour, minute int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, now.Location())
	}
	return t
}

// NextWeekly returns the next time after now on weekday at hour:minute.
func NextWeekly(now time.Time, wd time.Weekday, hour, minute int) time.Time {
	days := (int(wd) - int(now.Weekday()) + 7) % 7
	t := time.Date(now.Year(), now.Month(), now.Day()+days, hour, minute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 7)
	}
	return t
}

// PrevWeekly returns the most recent occurrence at or before now.
func PrevWeekly(now time.Time, wd time.Weekday, hour, minute int) time.Time {
	return NextWeekly(now, wd, hour, minute).AddDate(0, 0, -7)
}

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("locked by another ms365backupd process")

// Lock takes an exclusive, non-blocking flock on <dir>/<name>. The lock is
// released automatically if the process dies.
func Lock(dir, name string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", p, err)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
