package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigIsolatedAndExplicitPathPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ALX_HOME", home)
	for _, key := range []string{"ALX_CONFIG", "ALX_DATA", "ALX_CACHE"} {
		t.Setenv(key, "")
	}
	path := filepath.Join(home, "custom", "music.toml")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Set("source.default_quality", "flac"); err != nil {
		t.Fatal(err)
	}
	d, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Source.DefaultQuality != "flac" || d.Path != path {
		t.Fatalf("bad config %#v", d)
	}
	if _, err = os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("explicit config wrote default")
	}
	if err = d.Set("player.default_volume", "120"); err == nil {
		t.Fatal("invalid volume accepted")
	}
}
