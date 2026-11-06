package daemon

import (
	"errors"
	"testing"
	"time"
)

func TestNextDaily(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 3, 10, 1, 0, 0, 0, loc)
	if got := NextDaily(now, 2, 30); !got.Equal(time.Date(2026, 3, 10, 2, 30, 0, 0, loc)) {
		t.Errorf("same day: %v", got)
	}
	now = time.Date(2026, 3, 10, 2, 30, 0, 0, loc)
	if got := NextDaily(now, 2, 30); !got.Equal(time.Date(2026, 3, 11, 2, 30, 0, 0, loc)) {
		t.Errorf("exactly at time should be next day: %v", got)
	}
}

func TestNextPrevWeekly(t *testing.T) {
	loc := time.Local
	tue := time.Date(2026, 3, 10, 9, 0, 0, 0, loc) // Tuesday
	next := NextWeekly(tue, time.Monday, 8, 0)
	if next.Weekday() != time.Monday || !next.After(tue) || next.Sub(tue) > 7*24*time.Hour {
		t.Errorf("next weekly: %v", next)
	}
	prev := PrevWeekly(tue, time.Monday, 8, 0)
	if !prev.Equal(time.Date(2026, 3, 9, 8, 0, 0, 0, loc)) {
		t.Errorf("prev weekly: %v", prev)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir, "x.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir, "x.lock"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	u2, err := Lock(dir, "x.lock")
	if err != nil {
		t.Fatal(err)
	}
	u2()
}
