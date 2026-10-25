package catalog

import (
	"context"
	"os"
	"testing"
	"time"

	"lxcli/internal/model"
)

// Explicit opt-in: protocol tests never depend on a platform being reachable.
// Run with ALX_CATALOG_LIVE=1 to record current public endpoint availability.
func TestLivePublicEndpoints(t *testing.T) {
	if os.Getenv("ALX_CATALOG_LIVE") != "1" {
		t.Skip("set ALX_CATALOG_LIVE=1 for real public endpoint checks")
	}
	c := NewClient()
	for _, platform := range []string{"wy", "kw", "kg"} {
		t.Run("search_"+platform, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			songs, err := c.Search(ctx, platform, "周杰伦", 1, 3)
			if err != nil {
				t.Fatal(err)
			}
			if len(songs) == 0 {
				t.Fatal("public search returned no songs")
			}
			t.Logf("songs=%d first=%s id=%s mid=%s", len(songs), songs[0].Name, songs[0].SongID, songs[0].Songmid)
		})
	}
	for _, link := range []string{"https://music.163.com/playlist?id=3778678", "https://y.qq.com/n/ryqq/playlist/7217720898"} {
		t.Run(link, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			p, err := c.ImportPlaylist(ctx, link)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("name=%s total=%d songs=%d warnings=%v first=%s id=%s mid=%s", p.Name, p.Total, len(p.Songs), p.Warnings, p.Songs[0].Name, p.Songs[0].SongID, p.Songs[0].Songmid)
		})
	}
	t.Run("lyric_wy", func(t *testing.T) {
		lyric, err := c.Lyric(context.Background(), model.Song{Source: "wy", SongID: "347230"})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("lyrics bytes=%d", len(lyric))
	})
	t.Run("lyric_kw", func(t *testing.T) {
		lyric, err := c.Lyric(context.Background(), model.Song{Source: "kw", SongID: "661608480"})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("lyrics bytes=%d", len(lyric))
	})
}
