package backup

import (
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"normal.txt": "normal.txt",
		"a:b*c?.txt": "a_b_c_.txt",
		"CON":        "_CON",
		"trailing. ": "trailing",
		"":           "_",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := sanitizeName(strings.Repeat("ä", 300) + ".txt")
	if len(long) > 200 {
		t.Errorf("long name has %d bytes", len(long))
	}
}

func TestSlug(t *testing.T) {
	if s := slug("Re: Meeting / Q3 *budget*", 60); strings.ContainsAny(s, `:/*\`) {
		t.Errorf("slug has unsafe chars: %q", s)
	}
	if s := slug(strings.Repeat("x", 100), 10); len(s) > 10 {
		t.Errorf("slug too long: %q", s)
	}
}
