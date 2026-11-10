package backup

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/status"
	"github.com/ulbrich/ms365backupd/internal/store"
)

type mailFolder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ChildFolderCount int    `json:"childFolderCount"`
	path             string
}

type mailMessage struct {
	ID           string    `json:"id"`
	Subject      string    `json:"subject"`
	Received     time.Time `json:"receivedDateTime"`
	LastModified time.Time `json:"lastModifiedDateTime"`
	Removed      *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

// Immutable ids stay stable when messages move between folders.
var mailPrefer = graph.WithHeader("Prefer", `IdType="ImmutableId", odata.maxpagesize=200`)

// mailSource is one Graph mailbox: the account's own (service mail) or an
// additional mailbox with its own sign-in.
type mailSource struct {
	g    *graph.Client
	dir  string // backup-relative root, e.g. "mail" or "mailboxes/work"
	coll string // state collection prefix, e.g. "mail:"
	svc  string // counters and progress
}

func (e *Engine) ownMail() mailSource {
	return mailSource{g: e.g, dir: SvcMail, coll: "mail:", svc: SvcMail}
}

func (e *Engine) backupMail(ctx context.Context) error { return e.backupGraphMail(ctx, e.ownMail()) }

func (e *Engine) listMailFolders(ctx context.Context, src mailSource) ([]mailFolder, error) {
	var out []mailFolder
	var walk func(u, parent string, depth int) error
	walk = func(u, parent string, depth int) error {
		var level []mailFolder
		_, err := graph.Iterate(ctx, src.g, u, func(v []mailFolder) error {
			level = append(level, v...)
			return nil
		}, mailPrefer)
		if err != nil {
			return err
		}
		for _, f := range level {
			f.path = joinRel(parent, sanitizeName(f.DisplayName))
			out = append(out, f)
			if f.ChildFolderCount > 0 && depth < 64 {
				cu := fmt.Sprintf("me/mailFolders/%s/childFolders?$top=100&$select=id,displayName,childFolderCount", url.PathEscape(f.ID))
				if err := walk(cu, f.path, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := walk("me/mailFolders?$top=100&$select=id,displayName,childFolderCount", src.dir, 0)
	return out, err
}

func (e *Engine) backupGraphMail(ctx context.Context, src mailSource) error {
	e.setPhase(src.svc, "listing folders of "+src.dir)
	folders, err := e.listMailFolders(ctx, src)
	if err != nil {
		return fmt.Errorf("list mail folders: %w", err)
	}
	live := map[string]bool{}
	var errs []string
	for _, f := range folders {
		coll := src.coll + f.ID
		live[coll] = true
		if e.ex.Excluded(f.path) {
			e.log.Debug("mail folder excluded", "folder", f.path)
			continue
		}
		if err := e.syncMailFolder(ctx, src, f); err != nil {
			if isFatal(err) {
				return err
			}
			errs = append(errs, fmt.Sprintf("%s: %v", f.path, err))
			e.sample(fmt.Sprintf("mail folder %s: %v", f.path, err))
		}
	}
	if err := e.retireFolders(src.coll, live); err != nil {
		return err
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d folder(s) failed: %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

// retireFolders removes the backup of mail folders (collections with prefix) that
// no longer exist in the mailbox.
func (e *Engine) retireFolders(prefix string, live map[string]bool) error {
	colls, err := e.st.Collections(prefix)
	if err != nil {
		return err
	}
	for _, coll := range colls {
		if live[coll] {
			continue
		}
		old := e.st.Meta("path:" + coll)
		if old != "" && !e.ex.Excluded(old) {
			e.log.Info("mail folder removed", "folder", old)
			if err := e.retire(old); err != nil {
				return err
			}
		}
		_ = e.st.DropCollection(coll)
		_ = e.st.SetDelta(coll, "")
	}
	return nil
}

func messageRel(folder string, m mailMessage) string {
	t := m.Received
	if t.IsZero() {
		t = m.LastModified
	}
	if t.IsZero() {
		t = time.Unix(0, 0)
	}
	t = t.Local()
	subj := slug(m.Subject, 60)
	if subj == "_" || subj == "" {
		subj = "no-subject"
	}
	name := fmt.Sprintf("%s_%s_%s.eml", t.Format("2006-01-02_150405"), subj, shortHash(m.ID))
	return joinRel(folder, t.Format("2006"), name)
}

// relocateFolder moves the directory of a renamed/moved mail folder and updates
// the stored paths of its messages.
func (e *Engine) relocateFolder(coll, path string) error {
	pathKey := "path:" + coll
	if old := e.st.Meta(pathKey); old != "" && old != path {
		if _, err := e.move(old, path); err != nil {
			return err
		}
		items, err := e.st.All(coll)
		if err != nil {
			return err
		}
		for id, it := range items {
			if isUnder(it.Path, old) {
				it.Path = path + strings.TrimPrefix(it.Path, old)
				items[id] = it
			}
		}
		if err := e.st.PutMany(coll, items); err != nil {
			return err
		}
	}
	return e.st.SetMeta(pathKey, path)
}

func (e *Engine) syncMailFolder(ctx context.Context, src mailSource, f mailFolder) error {
	coll := src.coll + f.ID
	svc := src.svc
	e.setPhase(svc, "listing "+f.path)

	// Folder renamed/moved: move its directory.
	if err := e.relocateFolder(coll, f.path); err != nil {
		return err
	}

	initial := fmt.Sprintf("me/mailFolders/%s/messages/delta?$select=id,subject,receivedDateTime,lastModifiedDateTime", url.PathEscape(f.ID))
	link := e.st.Delta(coll)
	full := link == "" || e.o.FullResync
	if full {
		link = initial
	}
	fetch := func(u string) ([]mailMessage, string, error) {
		var all []mailMessage
		dl, err := graph.Iterate(ctx, src.g, u, func(v []mailMessage) error {
			all = append(all, v...)
			e.setCurrent(fmt.Sprintf("%s: %d changes", f.path, len(all)))
			return nil
		}, mailPrefer)
		return all, dl, err
	}
	msgs, newLink, err := fetch(link)
	if err != nil && graph.IsResync(err) && !full {
		e.log.Warn("mail delta expired, full resync", "folder", f.path)
		full = true
		msgs, newLink, err = fetch(initial)
	}
	if err != nil {
		return err
	}
	if full {
		e.markFullSync(f.path)
	}

	known, err := e.st.All(coll)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var removed []string
	var todo []mailMessage
	for _, m := range msgs {
		seen[m.ID] = true
		if m.Removed != nil {
			removed = append(removed, m.ID)
			continue
		}
		todo = append(todo, m)
	}
	if full {
		for id := range known {
			if !seen[id] {
				removed = append(removed, id)
			}
		}
	}
	for _, id := range removed {
		if it, ok := known[id]; ok {
			if err := e.retire(it.Path); err != nil {
				return err
			}
			e.count(svc, func(c *status.Counters) { c.Deleted++ })
		}
	}
	if err := e.st.Delete(coll, removed...); err != nil {
		return err
	}

	// Messages already backed up only change flags/read state; MIME content is immutable.
	var jobs []mailMessage
	for _, m := range todo {
		if it, ok := known[m.ID]; ok && it.Path != "" && exists(e.abs(it.Path)) {
			e.count(svc, func(c *status.Counters) { c.Unchanged++ })
			continue
		}
		jobs = append(jobs, m)
	}

	e.setPhase(svc, "downloading "+f.path)
	var mu sync.Mutex
	dirty := false
	e.addQueued(int64(len(jobs)))
	err = forEach(ctx, e.cfg.Concurrency, jobs, func(ctx context.Context, m mailMessage) error {
		defer e.addQueued(-1)
		rel := messageRel(f.path, m)
		e.setCurrent(rel)
		u := fmt.Sprintf("me/messages/%s/$value", url.PathEscape(m.ID))
		mt := m.Received
		if mt.IsZero() {
			mt = m.LastModified
		}
		n, err := e.download(ctx, src.g, u, rel, -1, mt, "", mailPrefer)
		e.count(svc, func(c *status.Counters) { c.Bytes += n })
		if err != nil {
			retry, fatal := e.itemError(svc, rel, err)
			if fatal != nil {
				return fatal
			}
			if retry {
				mu.Lock()
				dirty = true
				mu.Unlock()
				return nil
			}
			return e.st.Put(coll, m.ID, store.Item{Path: "", Skipped: true})
		}
		e.count(svc, func(c *status.Counters) { c.New++ })
		return e.st.Put(coll, m.ID, store.Item{Path: rel, ModTime: mt})
	})
	if err != nil {
		return err
	}
	if dirty {
		return nil // retry next run from the same delta position
	}
	if newLink != "" {
		return e.st.SetDelta(coll, newLink)
	}
	return nil
}
