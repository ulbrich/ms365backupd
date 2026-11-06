// Package exclude matches backup-relative paths against exclusion patterns.
//
// Paths look like "onedrive/Videos/holiday.mp4", "mail/Inbox/Newsletters",
// "sharepoint/Team Site/Documents/Archive". A pattern excludes the path it
// matches and everything below it. Patterns support doublestar globs
// ("**/node_modules", "onedrive/*/tmp"). Matching is case-insensitive because
// OneDrive, Exchange and most backup filesystems are.
package exclude

import (
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Matcher holds compiled patterns.
type Matcher struct{ patterns []string }

// New validates and normalises patterns.
func New(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	for _, p := range patterns {
		n := normalize(p)
		if n == "" || n == "." {
			continue
		}
		if !doublestar.ValidatePattern(n) {
			return nil, fmt.Errorf("invalid exclude pattern %q", p)
		}
		m.patterns = append(m.patterns, n)
	}
	return m, nil
}

func normalize(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

// Excluded reports whether rel (or one of its parent directories) matches a pattern.
func (m *Matcher) Excluded(rel string) bool {
	if m == nil || len(m.patterns) == 0 {
		return false
	}
	rel = normalize(rel)
	if rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		for _, p := range m.patterns {
			if ok, _ := doublestar.Match(p, prefix); ok {
				return true
			}
		}
	}
	return false
}

// Patterns returns the normalised patterns.
func (m *Matcher) Patterns() []string { return append([]string(nil), m.patterns...) }
