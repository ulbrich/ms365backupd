package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/status"
	"github.com/ulbrich/ms365backupd/internal/store"
)

// Contacts and calendars are small; they are snapshotted completely on every
// run (no delta tokens to expire) and only changed items are rewritten.

type folderRef struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
}

func (f folderRef) title() string {
	if f.DisplayName != "" {
		return f.DisplayName
	}
	return f.Name
}

type snapshotSpec struct {
	svc     string
	coll    string
	dir     string // backup-relative dir
	listURL string
	opts    []graph.Option
	// fileBase returns the file base name (without extension) for an object.
	fileBase func(obj map[string]any) string
	// extra returns additional renderings, e.g. ".vcf" → data.
	extra func(obj map[string]any) map[string][]byte
}

func (e *Engine) syncSnapshot(ctx context.Context, sp snapshotSpec) error {
	if e.ex.Excluded(sp.dir) {
		return nil
	}
	e.setPhase(sp.svc, "reading "+sp.dir)
	var objs []json.RawMessage
	_, err := graph.Iterate(ctx, e.g, sp.listURL, func(v []json.RawMessage) error {
		objs = append(objs, v...)
		e.setCurrent(fmt.Sprintf("%s: %d items", sp.dir, len(objs)))
		return nil
	}, sp.opts...)
	if err != nil {
		// Incomplete listing: never delete anything based on it.
		return err
	}
	known, err := e.st.All(sp.coll)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	update := map[string]store.Item{}
	for _, raw := range objs {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		id, _ := obj["id"].(string)
		if id == "" {
			continue
		}
		delete(obj, "@odata.etag")
		seen[id] = true
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(obj); err != nil {
			continue
		}
		sum := sha256.Sum256(buf.Bytes())
		h := hex.EncodeToString(sum[:])
		base := joinRel(sp.dir, sp.fileBase(obj)+"_"+shortHash(id))
		rel := base + ".json"
		old, had := known[id]
		if had && old.Hash == h && old.Path == rel && exists(e.abs(rel)) {
			e.count(sp.svc, func(c *status.Counters) { c.Unchanged++ })
			continue
		}
		var mt time.Time
		if s, ok := obj["lastModifiedDateTime"].(string); ok {
			mt, _ = time.Parse(time.RFC3339Nano, s)
		}
		if err := e.writeFileAtomic(rel, buf.Bytes(), mt); err != nil {
			return err
		}
		if sp.extra != nil {
			for ext, data := range sp.extra(obj) {
				if err := e.writeFileAtomic(base+ext, data, mt); err != nil {
					return err
				}
			}
		}
		if had && old.Path != "" && old.Path != rel {
			e.removeRenditions(old.Path, sp)
		}
		update[id] = store.Item{Path: rel, Hash: h, ModTime: mt}
		e.count(sp.svc, func(c *status.Counters) {
			if had {
				c.Updated++
			} else {
				c.New++
			}
		})
		e.count(sp.svc, func(c *status.Counters) { c.Bytes += int64(buf.Len()) })
	}
	if err := e.st.PutMany(sp.coll, update); err != nil {
		return err
	}
	var gone []string
	for id, it := range known {
		if seen[id] {
			continue
		}
		gone = append(gone, id)
		if err := e.retire(it.Path); err != nil {
			return err
		}
		if sp.extra != nil {
			b := strings.TrimSuffix(it.Path, ".json")
			for ext := range sp.extra(map[string]any{}) {
				if err := e.retire(b + ext); err != nil {
					return err
				}
			}
		}
		e.count(sp.svc, func(c *status.Counters) { c.Deleted++ })
	}
	return e.st.Delete(sp.coll, gone...)
}

// removeRenditions deletes the files of an item whose name changed (not a deletion).
func (e *Engine) removeRenditions(oldJSON string, sp snapshotSpec) {
	paths := []string{oldJSON}
	if sp.extra != nil {
		b := strings.TrimSuffix(oldJSON, ".json")
		for ext := range sp.extra(map[string]any{}) {
			paths = append(paths, b+ext)
		}
	}
	for _, p := range paths {
		_ = removeFile(e.abs(p))
	}
}

func str(obj map[string]any, k string) string {
	s, _ := obj[k].(string)
	return s
}

// ---- contacts ----

func (e *Engine) backupContacts(ctx context.Context) error {
	const sel = "$top=100"
	type cf struct {
		folderRef
		path string
	}
	folders := []cf{{folderRef{ID: "", DisplayName: "Contacts"}, joinRel(SvcContacts, "Contacts")}}
	var walk func(u, parent string, depth int) error
	walk = func(u, parent string, depth int) error {
		var level []folderRef
		if _, err := graph.Iterate(ctx, e.g, u, func(v []folderRef) error { level = append(level, v...); return nil }); err != nil {
			return err
		}
		for _, f := range level {
			p := joinRel(parent, sanitizeName(f.title()))
			folders = append(folders, cf{f, p})
			if depth < 16 {
				cu := fmt.Sprintf("me/contactFolders/%s/childFolders?$top=100", url.PathEscape(f.ID))
				if err := walk(cu, p, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	e.setPhase(SvcContacts, "listing folders")
	if err := walk("me/contactFolders?$top=100", SvcContacts, 0); err != nil {
		return fmt.Errorf("list contact folders: %w", err)
	}
	live := map[string]bool{}
	for _, f := range folders {
		u := "me/contacts?" + sel
		coll := "contacts:default"
		if f.ID != "" {
			u = fmt.Sprintf("me/contactFolders/%s/contacts?%s", url.PathEscape(f.ID), sel)
			coll = "contacts:" + f.ID
		}
		live[coll] = true
		if err := e.syncSnapshot(ctx, snapshotSpec{
			svc: SvcContacts, coll: coll, dir: f.path, listURL: u,
			fileBase: func(o map[string]any) string {
				n := str(o, "displayName")
				if n == "" {
					n = strings.TrimSpace(str(o, "givenName") + " " + str(o, "surname"))
				}
				if n == "" {
					n = "unnamed"
				}
				return slug(n, 80)
			},
			extra: func(o map[string]any) map[string][]byte {
				return map[string][]byte{".vcf": vcard(o)}
			},
		}); err != nil {
			return err
		}
	}
	return e.dropStaleSnapshots("contacts:", live)
}

// ---- calendar ----

func (e *Engine) backupCalendar(ctx context.Context) error {
	e.setPhase(SvcCalendar, "listing calendars")
	var cals []folderRef
	if _, err := graph.Iterate(ctx, e.g, "me/calendars?$top=100&$select=id,name", func(v []folderRef) error {
		cals = append(cals, v...)
		return nil
	}); err != nil {
		return fmt.Errorf("list calendars: %w", err)
	}
	live := map[string]bool{}
	used := map[string]int{}
	for _, c := range cals {
		name := sanitizeName(c.title())
		if used[strings.ToLower(name)]++; used[strings.ToLower(name)] > 1 {
			name = fmt.Sprintf("%s (%s)", name, shortHash(c.ID))
		}
		coll := "calendar:" + c.ID
		live[coll] = true
		if err := e.syncSnapshot(ctx, snapshotSpec{
			svc: SvcCalendar, coll: coll, dir: joinRel(SvcCalendar, name),
			listURL: fmt.Sprintf("me/calendars/%s/events?$top=100", url.PathEscape(c.ID)),
			opts:    []graph.Option{graph.WithHeader("Prefer", `outlook.timezone="UTC", odata.maxpagesize=100`)},
			fileBase: func(o map[string]any) string {
				day := "0000-00-00"
				if st, ok := o["start"].(map[string]any); ok {
					if dt := str(st, "dateTime"); len(dt) >= 10 {
						day = dt[:10]
					}
				}
				s := slug(str(o, "subject"), 60)
				if s == "" || s == "_" {
					s = "no-subject"
				}
				return day + "_" + s
			},
		}); err != nil {
			return err
		}
	}
	return e.dropStaleSnapshots("calendar:", live)
}

// dropStaleSnapshots retires folders/calendars that no longer exist.
func (e *Engine) dropStaleSnapshots(prefix string, live map[string]bool) error {
	colls, err := e.st.Collections(prefix)
	if err != nil {
		return err
	}
	for _, coll := range colls {
		if live[coll] {
			continue
		}
		items, err := e.st.All(coll)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := e.retire(it.Path); err != nil {
				return err
			}
			if strings.HasSuffix(it.Path, ".json") {
				_ = e.retire(strings.TrimSuffix(it.Path, ".json") + ".vcf")
			}
		}
		_ = e.st.DropCollection(coll)
	}
	return nil
}
