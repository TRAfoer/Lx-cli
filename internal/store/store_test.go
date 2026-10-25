package store

import (
	"lxcli/internal/model"
	"path/filepath"
	"testing"
)

func TestImportIsAtomicAndSongsRemainResolvable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	songs := []model.Song{{SongID: "123", Source: "wy", Name: "one"}, {SongID: "456", Source: "tx", Name: "two"}, {SongID: "123", Source: "wy", Name: "one duplicate"}}
	if err = s.ImportPlaylist("共享歌单", "from link", songs); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportPlaylist("共享歌单", "replace", nil); err == nil {
		t.Fatal("duplicate import overwrote playlist")
	}
	got, err := s.PlaylistSongs("共享歌单")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SongID != "123" || got[1].SongID != "456" {
		t.Fatalf("order/dedupe: %#v", got)
	}
	resolved, err := s.Song(got[0].CLIID[:8])
	if err != nil || resolved.Name != "one" {
		t.Fatalf("not cached: %#v %v", resolved, err)
	}
	if err = s.RenamePlaylist("共享歌单", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeletePlaylist("renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlaylistSongs("renamed"); err == nil {
		t.Fatal("deleted playlist survived")
	}
	if err = s.DeletePlaylist("Favorites"); err == nil {
		t.Fatal("Favorites deleted")
	}
}

func TestStoreSurvivesReopenAndRejectsInvalidImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ImportPlaylist("bad", "", []model.Song{{Name: "missing identity"}}); err == nil {
		t.Fatal("invalid song accepted")
	}
	if _, err = s.PlaylistSongs("bad"); err == nil {
		t.Fatal("partial playlist persisted")
	}
	if err = s.CacheSongs([]model.Song{{Source: "kw", SongID: "7", Name: "persist"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Song("kw:7")
	if err != nil || got.Name != "persist" {
		t.Fatalf("%#v %v", got, err)
	}
}
