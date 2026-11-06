package backup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/status"
	"github.com/ulbrich/ms365backupd/internal/store"
)

type driveItem struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Size            int64     `json:"size"`
	LastModified    time.Time `json:"lastModifiedDateTime"`
	ParentReference struct {
		ID string `json:"id"`
	} `json:"parentReference"`
	FileSystemInfo struct {
		LastModified time.Time `json:"lastModifiedDateTime"`
	} `json:"fileSystemInfo"`
	File       *driveFile `json:"file"`
	Folder     *struct{}  `json:"folder"`
	Package    *struct{}  `json:"package"`
	Root       *struct{}  `json:"root"`
	Deleted    *struct{}  `json:"deleted"`
	RemoteItem *struct{}  `json:"remoteItem"`
}

const driveSelect = "id,name,size,lastModifiedDateTime,parentReference,fileSystemInfo,file,folder,package,root,deleted,remoteItem"

func (e *Engine) backupOneDrive(ctx context.Context) error {
	var d struct {
		ID string `json:"id"`
	}
	if err := e.g.GetJSON(ctx, "me/drive?$select=id", &d); err != nil {
		return fmt.Errorf("get drive: %w", err)
	}
	return e.syncDrive(ctx, SvcOneDrive, d.ID, SvcOneDrive)
}

type spSite struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
}

func (s spSite) label() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}

// backupSharePoint backs up the configured sites. "*" expands to every site the
// account can reach (see discoverSites); explicit entries may be mixed in. Each site
// is backed up once even if it is listed several times.
func (e *Engine) backupSharePoint(ctx context.Context) error {
	var errs []string
	var sites []spSite
	for _, ref := range e.cfg.SharePointSites {
		if strings.TrimSpace(ref) == config.AllSites {
			found, err := e.discoverSites(ctx)
			if err != nil {
				if isFatal(err) {
					return err
				}
				errs = append(errs, fmt.Sprintf("discover sites: %v", err))
			}
			sites = append(sites, found...)
			continue
		}
		s, err := e.resolveSite(ctx, ref)
		if err != nil {
			if isFatal(err) {
				return err
			}
			errs = append(errs, fmt.Sprintf("%s: %v", ref, err))
			continue
		}
		sites = append(sites, s)
	}
	seen := map[string]bool{}
	folders := map[string]string{} // lower-case folder name -> site id
	for _, s := range sites {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		name := sanitizeName(s.label())
		if id, ok := folders[strings.ToLower(name)]; ok && id != s.ID {
			name += "_" + shortHash(s.ID) // two sites with the same display name
		}
		folders[strings.ToLower(name)] = s.ID
		if e.ex.Excluded(joinRel(SvcSharePoint, name)) {
			e.log.Debug("sharepoint site excluded", "site", s.label())
			continue
		}
		if err := e.backupSite(ctx, s, name); err != nil {
			if isFatal(err) {
				return err
			}
			errs = append(errs, fmt.Sprintf("%s: %v", s.label(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// discoverSites finds the root site and the team site of every Microsoft 365 group
// the account is a direct member of. Needs only User.Read and Files.Read.All.
// Groups without a site (security/distribution groups) answer 404 and are skipped.
// Standalone sites that do not belong to a group are not found; list them explicitly.
func (e *Engine) discoverSites(ctx context.Context) ([]spSite, error) {
	var sites []spSite
	var root spSite
	switch err := e.g.GetJSON(ctx, "sites/root?$select=id,displayName,name", &root); {
	case err == nil:
		sites = append(sites, root)
	case isNoSite(err):
		e.log.Debug("no SharePoint root site", "err", err)
	default:
		return nil, fmt.Errorf("root site: %w", err)
	}
	var groups []string
	_, err := graph.Iterate(ctx, e.g, "me/memberOf/microsoft.graph.group?$select=id&$top=100", func(v []struct {
		ID string `json:"id"`
	}) error {
		for _, g := range v {
			groups = append(groups, g.ID)
		}
		return nil
	})
	if err != nil {
		return sites, fmt.Errorf("list groups: %w", err)
	}
	for _, id := range groups {
		var s spSite
		err := e.g.GetJSON(ctx, "groups/"+url.PathEscape(id)+"/sites/root?$select=id,displayName,name", &s)
		switch {
		case err == nil:
			sites = append(sites, s)
		case isNoSite(err):
			// group without a SharePoint site
		default:
			if isFatal(err) {
				return sites, err
			}
			return sites, fmt.Errorf("site of group %s: %w", id, err)
		}
	}
	e.log.Info("sharepoint sites discovered", "count", len(sites), "groups", len(groups))
	return sites, nil
}

// isNoSite reports a 404/403 answer: the site does not exist or is not accessible.
func isNoSite(err error) bool {
	var ae *graph.APIError
	return errors.As(err, &ae) && (ae.Status == 404 || ae.Status == 403)
}

// resolveSite accepts "contoso.sharepoint.com:/sites/Team", "contoso.sharepoint.com/sites/Team",
// a full https URL, "contoso.sharepoint.com" (root site) or a Graph site id.
func (e *Engine) resolveSite(ctx context.Context, site string) (spSite, error) {
	ref := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(site), "https://"), "http://"), "/")
	if !strings.Contains(ref, ":/") && strings.Contains(ref, "/") {
		i := strings.Index(ref, "/")
		ref = ref[:i] + ":" + ref[i:]
	}
	var s spSite
	if err := e.g.GetJSON(ctx, "sites/"+ref+"?$select=id,displayName,name", &s); err != nil {
		return s, fmt.Errorf("resolve site: %w", err)
	}
	return s, nil
}

// backupSite backs up all document libraries of a site.
func (e *Engine) backupSite(ctx context.Context, s spSite, name string) error {
	var drives []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_, err := graph.Iterate(ctx, e.g, "sites/"+s.ID+"/drives?$select=id,name", func(v []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}) error {
		drives = append(drives, v...)
		return nil
	})
	if err != nil {
		return fmt.Errorf("list libraries: %w", err)
	}
	var errs []string
	for _, d := range drives {
		prefix := joinRel(SvcSharePoint, sanitizeName(name), sanitizeName(d.Name))
		if e.ex.Excluded(prefix) {
			continue
		}
		if err := e.syncDrive(ctx, SvcSharePoint, d.ID, prefix); err != nil {
			if isFatal(err) {
				return err
			}
			errs = append(errs, fmt.Sprintf("%s: %v", d.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (e *Engine) fetchDriveDelta(ctx context.Context, u string) ([]driveItem, string, error) {
	var all []driveItem
	pages := 0
	link, err := graph.Iterate(ctx, e.g, u, func(v []driveItem) error {
		all = append(all, v...)
		pages++
		e.setCurrent(fmt.Sprintf("listing changes: %d items", len(all)))
		return nil
	})
	return all, link, err
}

// syncDrive mirrors one drive (OneDrive or SharePoint library) into prefix.
//
// Algorithm:
//  1. Fetch all changes since the stored delta link (or everything on first run / resync).
//  2. Merge them into the item tree loaded from the state DB and recompute every path
//     (handles renames/moves of folders, whose children don't appear in the delta).
//  3. Apply deletions, then moves (renaming on disk instead of re-downloading),
//     then download new/changed files with bounded concurrency.
//  4. Persist the tree and, only if every item succeeded, the new delta link.
func (e *Engine) syncDrive(ctx context.Context, svc, driveID, prefix string) error {
	coll := "drive:" + driveID
	e.setPhase(svc, "listing changes "+prefix)
	initial := fmt.Sprintf("drives/%s/root/delta?$select=%s", url.PathEscape(driveID), driveSelect)

	link := e.st.Delta(coll)
	full := link == "" || e.o.FullResync || e.excludeChanged(coll)
	if full {
		link = initial
	}
	changes, newLink, err := e.fetchDriveDelta(ctx, link)
	if err != nil && graph.IsResync(err) && !full {
		e.log.Warn("delta token expired, full resync", "drive", prefix)
		full = true
		changes, newLink, err = e.fetchDriveDelta(ctx, initial)
	}
	if err != nil {
		return fmt.Errorf("list changes: %w", err)
	}
	if full {
		e.markFullSync(prefix)
	}

	known, err := e.st.All(coll)
	if err != nil {
		return err
	}
	rootID := e.st.Meta(coll + ":root")

	// cur = where each known item currently lives on disk.
	cur := make(map[string]string, len(known))
	for id, it := range known {
		cur[id] = it.Path
	}
	nodes := known
	changed := map[string]bool{}
	deleted := map[string]bool{}
	seen := map[string]bool{}
	for _, c := range changes {
		seen[c.ID] = true
		if c.Deleted != nil {
			deleted[c.ID] = true
			delete(changed, c.ID)
			continue
		}
		if c.RemoteItem != nil && c.Root == nil {
			continue // item shared from another drive; backed up by its owner
		}
		delete(deleted, c.ID)
		n := nodes[c.ID]
		n.Name = c.Name
		n.ParentID = c.ParentReference.ID
		n.Folder = c.Folder != nil || c.Package != nil || c.Root != nil
		if c.Root != nil {
			rootID = c.ID
			n.Name, n.ParentID = "", ""
		}
		if !n.Folder {
			mt := c.FileSystemInfo.LastModified
			if mt.IsZero() {
				mt = c.LastModified
			}
			newHash := c.File.hash()
			if n.Hash != newHash || n.Size != c.Size || !n.ModTime.Equal(mt) {
				n.Skipped = false
			}
			n.Size, n.ModTime, n.Hash = c.Size, mt, newHash
		}
		nodes[c.ID] = n
		changed[c.ID] = true
	}
	if full {
		for id := range known {
			if !seen[id] {
				deleted[id] = true
			}
		}
	}

	if rootID == "" && len(nodes) > 0 {
		// Without the root we cannot place anything; never guess (it could trash everything).
		_ = e.st.SetDelta(coll, "")
		return fmt.Errorf("drive root unknown for %s; a full resync will run next time", prefix)
	}

	// Recompute all paths.
	paths := make(map[string]string, len(nodes))
	var resolve func(id string, depth int) (string, bool)
	resolve = func(id string, depth int) (string, bool) {
		if id == rootID {
			return prefix, true
		}
		if p, ok := paths[id]; ok {
			return p, p != ""
		}
		n, ok := nodes[id]
		if !ok || depth > 512 || deleted[id] {
			return "", false
		}
		pp, ok := resolve(n.ParentID, depth+1)
		if !ok {
			paths[id] = ""
			return "", false
		}
		p := joinRel(pp, sanitizeName(n.Name))
		paths[id] = p
		return p, true
	}
	for id := range nodes {
		if id == rootID {
			paths[id] = prefix
			continue
		}
		resolve(id, 0)
	}

	// 1) Deletions (before moves/downloads so a re-created file with the same name survives).
	e.setPhase(svc, "applying deletions "+prefix)
	var delIDs []string
	for id := range deleted {
		delIDs = append(delIDs, id)
	}
	for id, p := range paths {
		if p == "" && id != rootID && !deleted[id] {
			delIDs = append(delIDs, id) // orphan: parent vanished
		}
	}
	livePaths := map[string]bool{}
	for id, p := range paths {
		if p != "" && !deleted[id] {
			livePaths[strings.ToLower(p)] = true
		}
	}
	for _, id := range delIDs {
		old := cur[id]
		if old != "" && !livePaths[strings.ToLower(old)] && !e.ex.Excluded(old) {
			if err := e.retire(old); err != nil {
				return err
			}
			if !known[id].Folder {
				e.count(svc, func(c *status.Counters) { c.Deleted++ })
			}
		}
		delete(nodes, id)
		delete(paths, id)
	}

	// 2) Moves: folders shallow-first, then files.
	e.setPhase(svc, "applying moves "+prefix)
	var movedFolders []string
	byOldPath := map[string]string{}
	for id, n := range nodes {
		if n.Folder && cur[id] != "" && paths[id] != "" && cur[id] != paths[id] {
			movedFolders = append(movedFolders, cur[id])
			byOldPath[cur[id]] = id
		}
	}
	sortByDepth(movedFolders)
	for _, oldPath := range movedFolders {
		id := byOldPath[oldPath]
		from, to := cur[id], paths[id]
		if from == to {
			continue
		}
		ok, err := e.move(from, to)
		if err != nil {
			return err
		}
		if ok {
			e.count(svc, func(c *status.Counters) { c.Moved++ })
			for oid, p := range cur {
				if isUnder(p, from) {
					cur[oid] = to + strings.TrimPrefix(p, from)
				}
			}
		}
	}
	for id, n := range nodes {
		if n.Folder || cur[id] == "" || paths[id] == "" || cur[id] == paths[id] {
			continue
		}
		ok, err := e.move(cur[id], paths[id])
		if err != nil {
			return err
		}
		if ok {
			e.count(svc, func(c *status.Counters) { c.Moved++ })
			cur[id] = paths[id]
		}
	}

	// 3) Folders and downloads.
	e.setPhase(svc, "downloading "+prefix)
	type job struct {
		id string
		n  store.Item
		p  string
	}
	var jobs []job
	update := map[string]store.Item{}
	for id, n := range nodes {
		p := paths[id]
		if p == "" {
			continue
		}
		pathChanged := known[id].Path != p
		n.Path = p
		if n.Folder {
			if (changed[id] || full) && !e.ex.Excluded(p) {
				if err := ensureDir(e.abs(p)); err != nil {
					return err
				}
			}
			if changed[id] || pathChanged {
				update[id] = n
			}
			continue
		}
		if !changed[id] && !pathChanged && !full {
			continue
		}
		if e.ex.Excluded(p) {
			n.Skipped = true
			update[id] = n
			e.count(svc, func(c *status.Counters) { c.Skipped++ })
			continue
		}
		if fileMatches(e.abs(p), n.Size, n.ModTime) && (known[id].Hash == n.Hash || known[id].Hash == "") && !known[id].Skipped {
			update[id] = n
			e.count(svc, func(c *status.Counters) { c.Unchanged++ })
			continue
		}
		jobs = append(jobs, job{id, n, p})
	}
	if err := e.st.PutMany(coll, update); err != nil {
		return err
	}
	if err := e.st.Delete(coll, delIDs...); err != nil {
		return err
	}
	if rootID != "" {
		_ = e.st.SetMeta(coll+":root", rootID)
	}

	var dirty sync.Mutex
	isDirty := false
	e.addQueued(int64(len(jobs)))
	err = forEach(ctx, e.cfg.Concurrency, jobs, func(ctx context.Context, j job) error {
		defer e.addQueued(-1)
		e.setCurrent(j.p)
		existed := exists(e.abs(j.p))
		u := fmt.Sprintf("drives/%s/items/%s/content", url.PathEscape(driveID), url.PathEscape(j.id))
		qx := ""
		if strings.HasPrefix(j.n.Hash, "qx:") {
			qx = strings.TrimPrefix(j.n.Hash, "qx:")
		}
		n, err := e.download(ctx, u, j.p, j.n.Size, j.n.ModTime, qx)
		e.count(svc, func(c *status.Counters) { c.Bytes += n })
		if err != nil {
			retry, fatal := e.itemError(svc, j.p, err)
			if fatal != nil {
				return fatal
			}
			if retry {
				dirty.Lock()
				isDirty = true
				dirty.Unlock()
				return nil
			}
			j.n.Skipped = true
		}
		if err == nil {
			e.count(svc, func(c *status.Counters) {
				if existed {
					c.Updated++
				} else {
					c.New++
				}
			})
		}
		return e.st.Put(coll, j.id, j.n)
	})
	if err != nil {
		return err
	}
	if isDirty {
		e.log.Warn("some files failed; delta position not advanced, they will be retried", "drive", prefix)
		return nil
	}
	if newLink != "" {
		if err := e.st.SetDelta(coll, newLink); err != nil {
			return err
		}
	}
	return nil
}

type driveFile struct {
	Hashes struct {
		QuickXor string `json:"quickXorHash"`
		SHA1     string `json:"sha1Hash"`
		SHA256   string `json:"sha256Hash"`
	} `json:"hashes"`
}

func (f *driveFile) hash() string {
	if f == nil {
		return ""
	}
	switch {
	case f.Hashes.QuickXor != "":
		return "qx:" + f.Hashes.QuickXor
	case f.Hashes.SHA256 != "":
		return "sha256:" + f.Hashes.SHA256
	case f.Hashes.SHA1 != "":
		return "sha1:" + f.Hashes.SHA1
	}
	return ""
}
