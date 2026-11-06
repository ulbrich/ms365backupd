package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/quickxor"
	"github.com/ulbrich/ms365backupd/internal/status"
)

// fakeGraph is a tiny stateful Microsoft Graph imitation: one drive and one mail folder.
type fakeGraph struct {
	mu        sync.Mutex
	srv       *httptest.Server
	ver       int
	items     map[string]*fakeItem
	msgs      map[string]*fakeMsg
	downloads map[string]int
	hits      map[string]int // request counts per path
	expire    bool           // delta tokens answer 410 Gone
}

type fakeItem struct {
	id, name, parent string
	folder, deleted  bool
	content          string
	ver              int
}

type fakeMsg struct {
	id, subject string
	deleted     bool
	ver         int
}

var mtime = time.Date(2025, 3, 4, 10, 0, 0, 0, time.UTC)

func newFakeGraph(t *testing.T) *fakeGraph {
	f := &fakeGraph{items: map[string]*fakeItem{}, msgs: map[string]*fakeMsg{}, downloads: map[string]int{}, hits: map[string]int{}}
	f.items["root"] = &fakeItem{id: "root", folder: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) put(it fakeItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ver++
	it.ver = f.ver
	f.items[it.id] = &it
}

func (f *fakeGraph) edit(id string, fn func(*fakeItem)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ver++
	fn(f.items[id])
	f.items[id].ver = f.ver
}

func (f *fakeGraph) putMsg(id, subj string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ver++
	f.msgs[id] = &fakeMsg{id: id, subject: subj, ver: f.ver}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.0/")
	f.hits[p]++
	q := r.URL.Query()
	since := -1
	if tok := q.Get("token"); tok != "" {
		if f.expire {
			w.WriteHeader(http.StatusGone)
			writeJSON(w, map[string]any{"error": map[string]string{"code": "resyncRequired", "message": "gone"}})
			return
		}
		since, _ = strconv.Atoi(tok)
	}
	deltaLink := func(path string) string {
		return fmt.Sprintf("%s/v1.0/%s?token=%d", f.srv.URL, path, f.ver)
	}
	switch {
	case p == "me":
		writeJSON(w, map[string]string{"userPrincipalName": "Test@Example.com"})
	case p == "me/drive":
		writeJSON(w, map[string]string{"id": "d1"})
	case p == "drives/d1/root/delta":
		var out []map[string]any
		for _, it := range f.items {
			if since < 0 && it.deleted || since >= 0 && it.ver <= since {
				continue
			}
			m := map[string]any{"id": it.id, "name": it.name, "lastModifiedDateTime": mtime,
				"fileSystemInfo": map[string]any{"lastModifiedDateTime": mtime}}
			if it.id == "root" {
				m["root"] = map[string]any{}
				m["folder"] = map[string]any{}
			} else {
				m["parentReference"] = map[string]string{"id": it.parent}
			}
			switch {
			case it.deleted:
				m["deleted"] = map[string]any{}
			case it.folder:
				m["folder"] = map[string]any{}
			default:
				h := quickxor.New()
				h.Write([]byte(it.content))
				m["size"] = len(it.content)
				m["file"] = map[string]any{"hashes": map[string]string{"quickXorHash": quickxor.Base64(h)}}
			}
			out = append(out, m)
		}
		writeJSON(w, map[string]any{"value": out, "@odata.deltaLink": deltaLink("drives/d1/root/delta")})
	case strings.HasPrefix(p, "drives/d1/items/") && strings.HasSuffix(p, "/content"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "drives/d1/items/"), "/content")
		it, ok := f.items[id]
		if !ok || it.deleted {
			http.NotFound(w, r)
			return
		}
		f.downloads[id]++
		_, _ = io.WriteString(w, it.content)
	// SharePoint: a root site, group g1 with a team site, group g2 without a site.
	case p == "sites/root":
		writeJSON(w, map[string]string{"id": "s-root", "displayName": "Communication site"})
	case p == "me/memberOf/microsoft.graph.group":
		writeJSON(w, map[string]any{"value": []map[string]string{{"id": "g1"}, {"id": "g2"}}})
	case p == "groups/g1/sites/root", p == "sites/example.sharepoint.com:/sites/Team":
		writeJSON(w, map[string]string{"id": "s-team", "displayName": "Team"})
	case p == "sites/s-root/drives", p == "sites/s-team/drives":
		writeJSON(w, map[string]any{"value": []any{}})
	case p == "me/mailFolders":
		writeJSON(w, map[string]any{"value": []map[string]any{{"id": "inbox", "displayName": "Inbox", "childFolderCount": 0}}})
	case p == "me/mailFolders/inbox/messages/delta":
		var out []map[string]any
		for _, m := range f.msgs {
			if since < 0 && m.deleted || since >= 0 && m.ver <= since {
				continue
			}
			o := map[string]any{"id": m.id, "subject": m.subject, "receivedDateTime": mtime}
			if m.deleted {
				o["@removed"] = map[string]string{"reason": "deleted"}
			}
			out = append(out, o)
		}
		writeJSON(w, map[string]any{"value": out, "@odata.deltaLink": deltaLink("me/mailFolders/inbox/messages/delta")})
	case strings.HasPrefix(p, "me/messages/") && strings.HasSuffix(p, "/$value"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "me/messages/"), "/$value")
		m, ok := f.msgs[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.downloads["msg:"+id]++
		fmt.Fprintf(w, "Subject: %s\r\nFrom: a@example.com\r\n\r\nbody of %s\r\n", m.subject, id)
	default:
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"error": map[string]string{"code": "itemNotFound", "message": p}})
	}
}

type harness struct {
	t        *testing.T
	fg       *fakeGraph
	stateDir string
	sites    []string // sharepoint_sites; nil = none
}

func (h *harness) run(target string, full bool) *status.RunResult {
	h.t.Helper()
	cfg := config.Default()
	cfg.Target = target
	cfg.RequireMount = false
	cfg.MinFreeMB = 0
	cfg.Services = config.Services{OneDrive: true, Mail: true}
	cfg.Exclude = []string{"onedrive/Skip"}
	cfg.SharePointSites = h.sites
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := graph.New(h.fg.srv.URL+"/v1.0", oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}), log)
	g.MaxRetries = 1
	e, err := New(Options{Config: cfg, Graph: g, StateDir: h.stateDir, Log: log, Trigger: "test", FullResync: full})
	if err != nil {
		h.t.Fatal(err)
	}
	res := e.Run(context.Background())
	if res.Status != status.StatusSuccess {
		h.t.Fatalf("run failed: %s %v", res.Error, res.ErrorSamples)
	}
	return res
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("expected file %s: %v", p, err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err == nil {
		t.Fatalf("%s should not exist", p)
	}
}

func countEML(t *testing.T, root string) int {
	n := 0
	_ = filepath.WalkDir(filepath.Join(root, "mail"), func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".eml") {
			n++
		}
		return nil
	})
	return n
}

func TestEngineIncremental(t *testing.T) {
	fg := newFakeGraph(t)
	h := &harness{t: t, fg: fg, stateDir: t.TempDir()}
	target := t.TempDir()
	sibling := filepath.Join(target, "other-stuff", "keep.txt")
	_ = os.MkdirAll(filepath.Dir(sibling), 0o755)
	_ = os.WriteFile(sibling, []byte("not ours"), 0o644)

	fg.put(fakeItem{id: "a", name: "Docs", parent: "root", folder: true})
	fg.put(fakeItem{id: "x", name: "x.txt", parent: "a", content: "hello x"})
	fg.put(fakeItem{id: "y", name: "y.txt", parent: "root", content: "hello y"})
	fg.put(fakeItem{id: "s", name: "Skip", parent: "root", folder: true})
	fg.put(fakeItem{id: "s1", name: "big.bin", parent: "s", content: "skip me"})
	fg.putMsg("m1", "First message")

	res := h.run(target, false)
	root := filepath.Join(target, "ms365backupd")
	if !res.NewDisk {
		t.Error("first run should report a new disk")
	}
	if got := readFile(t, filepath.Join(root, "onedrive", "Docs", "x.txt")); got != "hello x" {
		t.Errorf("x.txt = %q", got)
	}
	readFile(t, filepath.Join(root, "onedrive", "y.txt"))
	mustNotExist(t, filepath.Join(root, "onedrive", "Skip"))
	if fg.downloads["s1"] != 0 {
		t.Error("excluded file was downloaded")
	}
	if n := countEML(t, root); n != 1 {
		t.Errorf("eml files = %d, want 1", n)
	}

	// Rename folder, delete a file, change a file, add a file and a message.
	fg.edit("a", func(i *fakeItem) { i.name = "Documents" })
	fg.edit("y", func(i *fakeItem) { i.deleted = true })
	fg.put(fakeItem{id: "z", name: "z.txt", parent: "a", content: "new z"})
	fg.edit("x", func(i *fakeItem) { i.content = "hello x v2" })
	fg.putMsg("m2", "Second message")

	res = h.run(target, false)
	if res.NewDisk {
		t.Error("second run should not be a new disk")
	}
	mustNotExist(t, filepath.Join(root, "onedrive", "Docs"))
	if got := readFile(t, filepath.Join(root, "onedrive", "Documents", "x.txt")); got != "hello x v2" {
		t.Errorf("x.txt after update = %q", got)
	}
	readFile(t, filepath.Join(root, "onedrive", "Documents", "z.txt"))
	mustNotExist(t, filepath.Join(root, "onedrive", "y.txt"))
	if m, _ := filepath.Glob(filepath.Join(root, ".deleted", "*", "onedrive", "Documents", "x.txt")); len(m) != 1 || readFile(t, m[0]) != "hello x" {
		t.Errorf("previous version of x.txt not kept: %v", m)
	}
	if m, _ := filepath.Glob(filepath.Join(root, ".deleted", "*", "onedrive", "y.txt")); len(m) != 1 {
		t.Errorf("deleted y.txt not kept in .deleted: %v", m)
	}
	if fg.downloads["y"] != 1 || fg.downloads["z"] != 1 {
		t.Errorf("downloads = %v", fg.downloads)
	}
	if n := countEML(t, root); n != 2 {
		t.Errorf("eml files = %d, want 2", n)
	}
	if fg.downloads["msg:m1"] != 1 {
		t.Errorf("m1 downloaded %d times", fg.downloads["msg:m1"])
	}
	od := res.Services[SvcOneDrive]
	if od.Moved < 1 || od.Deleted != 1 || od.New != 1 || od.Updated != 1 {
		t.Errorf("onedrive counters = %+v", od)
	}

	// Expired delta token → full resync without re-downloading unchanged files.
	fg.mu.Lock()
	fg.expire = true
	fg.mu.Unlock()
	h.run(target, false)
	fg.mu.Lock()
	fg.expire = false
	fg.mu.Unlock()
	if fg.downloads["z"] != 1 || fg.downloads["x"] != 2 || fg.downloads["msg:m2"] != 1 {
		t.Errorf("resync re-downloaded files: %v", fg.downloads)
	}
	readFile(t, filepath.Join(root, "onedrive", "Documents", "z.txt"))

	// Sibling directories on the disk are never touched.
	if readFile(t, sibling) != "not ours" {
		t.Error("sibling file changed")
	}

	// A new (empty) disk gets a complete backup from scratch.
	target2 := t.TempDir()
	res = h.run(target2, false)
	if !res.NewDisk {
		t.Error("second disk should be reported as new")
	}
	root2 := filepath.Join(target2, "ms365backupd")
	readFile(t, filepath.Join(root2, "onedrive", "Documents", "x.txt"))
	readFile(t, filepath.Join(root2, "onedrive", "Documents", "z.txt"))
	if n := countEML(t, root2); n != 2 {
		t.Errorf("new disk eml files = %d, want 2", n)
	}

	// A locally deleted file on the old disk is restored by --full.
	_ = os.Remove(filepath.Join(root, "onedrive", "Documents", "z.txt"))
	h.run(target, true)
	readFile(t, filepath.Join(root, "onedrive", "Documents", "z.txt"))
}

// "*" discovers the root site and the sites of the user's groups; an explicitly listed
// site that was also discovered is backed up once.
func TestSharePointDiscovery(t *testing.T) {
	fg := newFakeGraph(t)
	h := &harness{t: t, fg: fg, stateDir: t.TempDir(), sites: []string{"*", "https://example.sharepoint.com/sites/Team/"}}
	res := h.run(t.TempDir(), false)
	if _, ok := res.Services[SvcSharePoint]; !ok {
		t.Fatalf("sharepoint not run: %+v", res.Services)
	}
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if fg.hits["sites/s-root/drives"] != 1 || fg.hits["sites/s-team/drives"] != 1 {
		t.Fatalf("expected each site once: %v", fg.hits)
	}
	if fg.hits["groups/g2/sites/root"] != 1 {
		t.Fatal("group g2 not checked")
	}
}
