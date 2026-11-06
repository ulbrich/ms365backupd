package exclude

import "testing"

func TestExcluded(t *testing.T) {
	m, err := New([]string{"onedrive/Videos", "**/node_modules", "mail/Junk*", "/onedrive/Tmp/", "**/*.iso"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"onedrive/Videos":                  true,
		"onedrive/videos/holiday.mp4":      true, // case-insensitive, prefix match
		"onedrive/VideosOld":               false,
		"onedrive/code/app/node_modules/x": true,
		"mail/Junk E-Mail/2024/a.eml":      true,
		"onedrive/Tmp/a":                   true,
		"onedrive/Documents/report.pdf":    false,
		"onedrive/Images/ubuntu.iso":       true,
		"mail/Inbox/2024/x.eml":            false,
	}
	for p, want := range cases {
		if got := m.Excluded(p); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestBadPattern(t *testing.T) {
	if _, err := New([]string{"onedrive/[abc"}); err == nil {
		t.Fatal("expected error for invalid glob")
	}
}
