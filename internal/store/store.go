// Package store keeps per-disk sync state (delta tokens, item index) in a bbolt database.
//
// The database lives on the local machine under <state_dir>/disks/<disk-id>/state.db,
// keyed by the identity marker on the backup disk. Connecting a different disk
// therefore automatically starts a fresh full backup, while reconnecting a known
// disk continues incrementally. Keeping the DB off the external disk avoids
// mmap/flock quirks on exFAT/NTFS.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bDelta = []byte("delta")
	bMeta  = []byte("meta")
)

// Item is the stored state of one backed-up object.
type Item struct {
	Path     string    `json:"p"`            // backup-relative path ("" for not-materialised folders)
	Name     string    `json:"n,omitempty"`  // drive items: name
	ParentID string    `json:"pa,omitempty"` // drive items: parent id
	Folder   bool      `json:"f,omitempty"`
	Size     int64     `json:"s,omitempty"`
	ModTime  time.Time `json:"m,omitempty"`
	Hash     string    `json:"h,omitempty"` // remote content hash or local content hash
	Skipped  bool      `json:"sk,omitempty"`
}

// Store wraps the database.
type Store struct {
	db     *bolt.DB
	DiskID string
	Path   string
}

// Open opens (or creates) the state DB for diskID.
func Open(stateDir, diskID string) (*Store, error) {
	if diskID == "" || strings.ContainsAny(diskID, `/\.`) {
		return nil, fmt.Errorf("invalid disk id %q", diskID)
	}
	dir := filepath.Join(stateDir, "disks", diskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "state.db")
	db, err := bolt.Open(p, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, errors.New("state database is locked by another ms365backupd process")
		}
		return nil, fmt.Errorf("open state db: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bDelta, bMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, DiskID: diskID, Path: p}, nil
}

// Close closes the DB.
func (s *Store) Close() error { return s.db.Close() }

// Delta returns the stored delta link for key.
func (s *Store) Delta(key string) string {
	var v string
	_ = s.db.View(func(tx *bolt.Tx) error {
		v = string(tx.Bucket(bDelta).Get([]byte(key)))
		return nil
	})
	return v
}

// SetDelta stores (or with "" removes) a delta link.
func (s *Store) SetDelta(key, link string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if link == "" {
			return tx.Bucket(bDelta).Delete([]byte(key))
		}
		return tx.Bucket(bDelta).Put([]byte(key), []byte(link))
	})
}

// Meta returns a metadata value.
func (s *Store) Meta(key string) string {
	var v string
	_ = s.db.View(func(tx *bolt.Tx) error {
		v = string(tx.Bucket(bMeta).Get([]byte(key)))
		return nil
	})
	return v
}

// SetMeta stores a metadata value.
func (s *Store) SetMeta(key, val string) error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bMeta).Put([]byte(key), []byte(val)) })
}

func collName(c string) []byte { return []byte("c:" + c) }

// Get returns an item from a collection.
func (s *Store) Get(coll, id string) (Item, bool) {
	var it Item
	var ok bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(collName(coll))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(id))
		if v == nil {
			return nil
		}
		ok = json.Unmarshal(v, &it) == nil
		return nil
	})
	return it, ok
}

// Put stores an item.
func (s *Store) Put(coll, id string, it Item) error {
	return s.PutMany(coll, map[string]Item{id: it})
}

// PutMany stores many items in one transaction.
func (s *Store) PutMany(coll string, items map[string]Item) error {
	if len(items) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(collName(coll))
		if err != nil {
			return err
		}
		for id, it := range items {
			v, err := json.Marshal(it)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(id), v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete removes items.
func (s *Store) Delete(coll string, ids ...string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(collName(coll))
		if b == nil {
			return nil
		}
		for _, id := range ids {
			if err := b.Delete([]byte(id)); err != nil {
				return err
			}
		}
		return nil
	})
}

// All loads a whole collection into memory.
func (s *Store) All(coll string) (map[string]Item, error) {
	out := map[string]Item{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(collName(coll))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var it Item
			if err := json.Unmarshal(v, &it); err != nil {
				return nil // skip corrupt entry; it will be re-synced
			}
			out[string(k)] = it
			return nil
		})
	})
	return out, err
}

// DropCollection removes a collection entirely.
func (s *Store) DropCollection(coll string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		err := tx.DeleteBucket(collName(coll))
		if errors.Is(err, bolt.ErrBucketNotFound) {
			return nil
		}
		return err
	})
}

// Collections lists collection names with the given prefix.
func (s *Store) Collections(prefix string) ([]string, error) {
	var out []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			n := string(name)
			if strings.HasPrefix(n, "c:"+prefix) {
				out = append(out, strings.TrimPrefix(n, "c:"))
			}
			return nil
		})
	})
	return out, err
}
