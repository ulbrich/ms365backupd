package backup

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/quickxor"
)

const partialSuffix = ".ms365partial"

// abs converts a backup-relative slash path to an absolute OS path.
func (e *Engine) abs(rel string) string { return filepath.Join(e.root, filepath.FromSlash(rel)) }

// sanitizeName makes a single path component safe for exFAT/NTFS/APFS/ext4.
func sanitizeName(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "_")
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteRune('_')
		case strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	if out == "" || out == "." || out == ".." {
		out = "_"
	}
	// Reserved Windows device names would break NTFS/exFAT disks read on Windows.
	base := strings.ToUpper(strings.SplitN(out, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		out = "_" + out
	}
	return truncateUTF8(out, 200)
}

// slug shortens free text (subjects, names) for use in file names.
func slug(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = sanitizeName(s)
	return truncateUTF8(s, max)
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return strings.TrimRight(s[:max], ". ")
}

// shortHash is a stable 8-hex-char id for file names.
func shortHash(id string) string {
	h := sha1.Sum([]byte(id))
	return hex.EncodeToString(h[:4])
}

func joinRel(parts ...string) string { return path.Join(parts...) }

// isUnder reports whether rel is p or below p.
func isUnder(rel, p string) bool {
	return rel == p || strings.HasPrefix(rel, p+"/")
}

// fileMatches reports whether the file at p has the given size and (approximately) mtime.
func fileMatches(p string, size int64, mtime time.Time) bool {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if st.Size() != size {
		return false
	}
	if mtime.IsZero() {
		return true
	}
	d := st.ModTime().Sub(mtime)
	return d < 2*time.Second && d > -2*time.Second // FAT timestamp granularity
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// writeFileAtomic writes data to rel via temp file + fsync + rename.
func (e *Engine) writeFileAtomic(rel string, data []byte, mtime time.Time) error {
	p := e.abs(rel)
	if err := ensureDir(filepath.Dir(p)); err != nil {
		return err
	}
	tmp := p + partialSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return &graph.DiskError{Err: err}
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return &graph.DiskError{Err: err}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return &graph.DiskError{Err: err}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return &graph.DiskError{Err: err}
	}
	if !mtime.IsZero() {
		_ = os.Chtimes(tmp, mtime, mtime)
	}
	if err := os.Rename(tmp, p); err != nil {
		return &graph.DiskError{Err: err}
	}
	return nil
}

// errHashMismatch is retryable; the next run downloads again.
var errHashMismatch = errors.New("content hash mismatch after download")

// download fetches url into rel atomically. Partial downloads from interrupted
// runs are resumed. If wantQuickXor is set the content is verified.
func (e *Engine) download(ctx context.Context, url, rel string, wantSize int64, mtime time.Time, wantQuickXor string, hdr ...graph.Option) (int64, error) {
	if err := e.checkFree(); err != nil {
		return 0, err
	}
	p := e.abs(rel)
	if err := ensureDir(filepath.Dir(p)); err != nil {
		return 0, err
	}
	tmp := p + partialSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, &graph.DiskError{Err: err}
	}
	var offset int64
	if st, err := f.Stat(); err == nil && wantSize > 0 && st.Size() > 0 && st.Size() < wantSize {
		offset = st.Size()
	}
	n, err := e.g.Download(ctx, url, f, offset, hdr...)
	if err != nil {
		f.Close()
		var de *graph.DiskError
		if !errors.As(err, &de) && offset == 0 && n == 0 {
			os.Remove(tmp)
		}
		return n, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return n, &graph.DiskError{Err: err}
	}
	st, _ := f.Stat()
	if wantSize >= 0 && st != nil && st.Size() != wantSize {
		f.Close()
		os.Remove(tmp)
		return n, fmt.Errorf("size mismatch: got %d, want %d", st.Size(), wantSize)
	}
	if wantQuickXor != "" {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return n, &graph.DiskError{Err: err}
		}
		h := quickxor.New()
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return n, &graph.DiskError{Err: err}
		}
		if got := quickxor.Base64(h); got != wantQuickXor {
			f.Close()
			os.Remove(tmp)
			return n, fmt.Errorf("%w (got %s, want %s)", errHashMismatch, got, wantQuickXor)
		}
	}
	if err := f.Close(); err != nil {
		return n, &graph.DiskError{Err: err}
	}
	if !mtime.IsZero() {
		_ = os.Chtimes(tmp, mtime, mtime)
	}
	// Keep the previous version (protects against e.g. ransomware-encrypted files
	// syncing over good copies).
	if exists(p) {
		if err := e.retire(rel); err != nil {
			return n, err
		}
	}
	if err := os.Rename(tmp, p); err != nil {
		return n, &graph.DiskError{Err: err}
	}
	return n, nil
}

// retire removes a backed-up path because it was deleted in Microsoft 365.
// With keep_deleted it is moved to .deleted/<date>/<rel> instead.
func (e *Engine) retire(rel string) error {
	if rel == "" {
		return nil
	}
	p := e.abs(rel)
	if !exists(p) {
		return nil
	}
	if !e.cfg.KeepDeleted {
		if err := os.RemoveAll(p); err != nil {
			return &graph.DiskError{Err: err}
		}
		return nil
	}
	dst := e.abs(joinRel(".deleted", e.today, rel))
	if exists(dst) {
		for i := 1; ; i++ {
			c := fmt.Sprintf("%s~%d", dst, i)
			if !exists(c) {
				dst = c
				break
			}
		}
	}
	if err := ensureDir(filepath.Dir(dst)); err != nil {
		return err
	}
	if err := os.Rename(p, dst); err != nil {
		return &graph.DiskError{Err: err}
	}
	return nil
}

// move renames oldRel to newRel on disk if possible. It returns false if there
// was nothing to move or the destination already exists.
func (e *Engine) move(oldRel, newRel string) (bool, error) {
	if oldRel == "" || oldRel == newRel {
		return false, nil
	}
	op, np := e.abs(oldRel), e.abs(newRel)
	if !exists(op) {
		return false, nil
	}
	if exists(np) {
		// Case-only rename on a case-insensitive filesystem: same file.
		if !strings.EqualFold(oldRel, newRel) {
			return false, nil
		}
	}
	if err := ensureDir(filepath.Dir(np)); err != nil {
		return false, err
	}
	if err := os.Rename(op, np); err != nil {
		return false, &graph.DiskError{Err: err}
	}
	return true, nil
}
