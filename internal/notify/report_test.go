package notify

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"strings"
	"testing"

	"github.com/ulbrich/ms365backupd/internal/status"
)

func init() { host = func() string { return "ms365backup" } }

// The text version must stay exactly as it was; the golden files hold it.
func TestTextUnchanged(t *testing.T) {
	cases := map[string]Message{
		"failure":      FailureMessage(fxFailure(), 3, nil),
		"weekly":       WeeklyMessage(fxRuns(), fxT0, fxT1, fxDisk, 123<<30, 45678),
		"weekly-empty": WeeklyMessage(nil, fxT0, fxT1, status.DiskState{Path: "/mnt/backup", Reason: "target is not a mount point"}, 0, 0),
	}
	for name, m := range cases {
		want, err := os.ReadFile("testdata/" + name + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Subject + "\n\n" + m.Text; got != string(want) {
			t.Errorf("%s: text changed:\n%s", name, got)
		}
		if os.Getenv("WRITE_HTML") != "" {
			_ = os.WriteFile(os.Getenv("WRITE_HTML")+"/"+name+".html", []byte(m.HTML), 0o644)
		}
	}
}

func TestHTML(t *testing.T) {
	m := FailureMessage(fxFailure(), 3, nil)
	for _, want := range []string{"<!DOCTYPE html>", "Backup did not complete", "FAILED: 2 items failed &lt;will retry&gt;",
		"a &amp; b.pdf", "3398", "journalctl -u ms365backupd -e", "never"} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("failure HTML lacks %q", want)
		}
	}
	if strings.Contains(m.HTML, "ZgotmplZ") {
		t.Error("failure HTML has a sanitized (rejected) value")
	}
	w := WeeklyMessage(fxRuns(), fxT0, fxT1, fxDisk, 123<<30, 45678)
	for _, want := range []string{"Weekly backup report", "ATTENTION: 1 partial, 1 failed of 3 runs", "sharepoint", "5.6 GiB", "Problems",
		"target is not a mount point"} {
		if !strings.Contains(w.HTML, want) {
			t.Errorf("weekly HTML lacks %q", want)
		}
	}
	if strings.Contains(w.HTML, "ZgotmplZ") {
		t.Error("weekly HTML has a sanitized (rejected) value")
	}
	if e := WeeklyMessage(nil, fxT0, fxT1, status.DiskState{Path: "/mnt/backup"}, 0, 0); !strings.Contains(e.HTML, "NO SUCCESSFUL BACKUP") {
		t.Error("empty week not flagged")
	}
	if tm := TestMessage(); tm.HTML == "" || !strings.HasPrefix(tm.Text, "This is a test message") {
		t.Error("test message")
	}
}

func TestBuildMessageMultipart(t *testing.T) {
	m := WeeklyMessage(fxRuns(), fxT0, fxT1, fxDisk, 123<<30, 45678)
	raw := buildMessage("Backup <backup@example.com>", []string{"a@example.com"}, m)
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q: %v", mt, err)
	}
	r := multipart.NewReader(msg.Body, params["boundary"])
	var types []string
	var bodies []string
	for {
		p, err := r.NextPart() // decodes quoted-printable
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		types = append(types, strings.SplitN(p.Header.Get("Content-Type"), ";", 2)[0])
		bodies = append(bodies, strings.ReplaceAll(string(b), "\r\n", "\n"))
	}
	if strings.Join(types, ",") != "text/plain,text/html" {
		t.Fatalf("parts %v", types)
	}
	if bodies[0] != m.Text || bodies[1] != m.HTML {
		t.Error("part bodies differ from the message")
	}

	plain := buildMessage("b@example.com", []string{"a@example.com"}, Message{Subject: "s", Text: "hi\n"})
	if !bytes.Contains(plain, []byte("Content-Type: text/plain; charset=utf-8")) || bytes.Contains(plain, []byte("multipart")) {
		t.Errorf("text-only message:\n%s", plain)
	}
}
