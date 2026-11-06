package store

import "testing"

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "disk1")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetDelta("drive:x", "https://delta")
	_ = s.Put("drive:x", "1", Item{Path: "onedrive/a", Size: 3})
	_ = s.PutMany("drive:x", map[string]Item{"2": {Path: "onedrive/b"}, "3": {Path: "onedrive/c"}})
	_ = s.Delete("drive:x", "3")
	s.Close()

	s, err = Open(dir, "disk1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Delta("drive:x") != "https://delta" {
		t.Error("delta not persisted")
	}
	all, _ := s.All("drive:x")
	if len(all) != 2 || all["1"].Size != 3 {
		t.Errorf("items = %+v", all)
	}
	if cs, _ := s.Collections("drive:"); len(cs) != 1 {
		t.Errorf("collections = %v", cs)
	}
	_ = s.DropCollection("drive:x")
	if all, _ := s.All("drive:x"); len(all) != 0 {
		t.Error("collection not dropped")
	}

	other, err := Open(dir, "disk2")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Delta("drive:x") != "" {
		t.Error("state leaked between disks")
	}
}
