// Package disk inspects the backup target: is it mounted, which disk is it, how much space is left.
package disk

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DataDir is the directory on the disk that holds all backup data.
const DataDir = "ms365backupd"

// MetaDir holds the disk identity marker, inside DataDir.
const MetaDir = ".ms365backupd"

// Info describes the state of the target disk.
type Info struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Reason  string `json:"reason,omitempty"` // why not present
	DiskID  string `json:"disk_id,omitempty"`
	Account string `json:"account,omitempty"`
	Free    uint64 `json:"free"`
	Total   uint64 `json:"total"`
}

// Marker is stored at <target>/ms365backupd/.ms365backupd/disk.json.
type Marker struct {
	DiskID  string    `json:"disk_id"`
	Created time.Time `json:"created"`
	Account string    `json:"account,omitempty"`
	Tool    string    `json:"tool"`
}

// IsMountPoint reports whether path is the root of a mounted filesystem
// (its device differs from its parent's).
func IsMountPoint(path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	var st, pst syscall.Stat_t
	if err := syscall.Stat(abs, &st); err != nil {
		return false, err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return true, nil // "/"
	}
	if err := syscall.Stat(parent, &pst); err != nil {
		return false, err
	}
	if uint64(st.Dev) != uint64(pst.Dev) {
		return true, nil
	}
	// Same device: still a mount point if it's the same inode as its parent (bind/root).
	return st.Ino == pst.Ino, nil
}

// Space returns free (available to unprivileged users) and total bytes.
func Space(path string) (free, total uint64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	bs := uint64(s.Bsize)
	return uint64(s.Bavail) * bs, uint64(s.Blocks) * bs, nil
}

// Probe checks whether the target is usable. It never creates anything.
func Probe(target string, requireMount bool) Info {
	info := Info{Path: target}
	st, err := os.Stat(target)
	if err != nil {
		info.Reason = "target not found"
		return info
	}
	if !st.IsDir() {
		info.Reason = "target is not a directory"
		return info
	}
	if requireMount {
		ok, err := IsMountPoint(target)
		if err != nil {
			info.Reason = "cannot stat target: " + err.Error()
			return info
		}
		if !ok {
			info.Reason = "target is not a mount point (disk not connected?)"
			return info
		}
	}
	// Reading the directory triggers autofs mounts and detects dead/ejected media.
	if _, err := os.ReadDir(target); err != nil {
		info.Reason = "target not readable: " + err.Error()
		return info
	}
	info.Free, info.Total, _ = Space(target)
	if m, err := ReadMarker(target); err == nil {
		info.DiskID = m.DiskID
		info.Account = m.Account
	}
	info.Present = true
	return info
}

func markerPath(target string) string {
	return filepath.Join(target, DataDir, MetaDir, "disk.json")
}

// ReadMarker reads the disk identity marker.
func ReadMarker(target string) (*Marker, error) {
	b, err := os.ReadFile(markerPath(target))
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("corrupt disk marker: %w", err)
	}
	if m.DiskID == "" {
		return nil, errors.New("disk marker without id")
	}
	return &m, nil
}

// EnsureMarker returns the disk's marker, creating one (= a new disk) if missing.
// It also verifies the disk is writable.
func EnsureMarker(target, account string) (*Marker, bool, error) {
	if m, err := ReadMarker(target); err == nil {
		return m, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(markerPath(target)), 0o755); err != nil {
		return nil, false, fmt.Errorf("disk not writable: %w", err)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, false, err
	}
	m := &Marker{DiskID: hex.EncodeToString(b[:]), Created: time.Now().UTC(), Account: account, Tool: "ms365backupd"}
	data, _ := json.MarshalIndent(m, "", "  ")
	p := markerPath(target)
	if err := writeSync(p+".tmp", data); err != nil {
		return nil, false, fmt.Errorf("disk not writable: %w", err)
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// UpdateMarkerAccount stores the account on an existing marker.
func UpdateMarkerAccount(target string, m *Marker, account string) error {
	m.Account = account
	data, _ := json.MarshalIndent(m, "", "  ")
	p := markerPath(target)
	if err := writeSync(p+".tmp", data); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// CheckWritable writes and removes a probe file.
func CheckWritable(dir string) error {
	p := filepath.Join(dir, ".write-test")
	if err := writeSync(p, []byte("ok")); err != nil {
		return err
	}
	return os.Remove(p)
}

func writeSync(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
