package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The fixtures preserve the public endpoints' field names and nesting. Removing
// the native requests, pagination, strict result checks or mapping breaks these.
const wyPlaylistSearchFixture = `{"result":{"playlists":[{"id":6792103822,"name":"Jay &amp; friends","coverImgUrl":"https://p.example/wy.jpg","creator":{"nickname":"Buradarrr"},"trackCount":144,"playCount":34149068,"description":"精选<br>歌曲"}],"playlistCount":462},"code":200}`
const txPlaylistSearchFixture = `{"code":0,"subcode":0,"data":{"display_num":46454,"list":[{"creator":{"name":"歌单狂魔"},"dissid":"3805603854","dissname":"周杰伦 &amp; friends","imgurl":"https://p.example/tx.jpg","introduction":"精选&amp;lt;br&amp;gt;歌曲","listennum":3000000000,"song_count":223}],"num_per_page":2,"page_no":1,"sum":46454}}`

func TestPlaylistSearchNetEasePaginationAndMetadata(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/search/get/web" || q.Get("s") != "周杰伦 & friends" || q.Get("type") != "1000" || q.Get("offset") != "2" || q.Get("limit") != "2" {
			t.Errorf("wrong NetEase search: %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(wyPlaylistSearchFixture))
	})
	r, err := c.SearchPlaylists(context.Background(), "wy", "周杰伦 & friends", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Page != 2 || r.Limit != 2 || r.Total != 462 || !r.HasMore || len(r.Playlists) != 1 {
		t.Fatalf("result = %+v", r)
	}
	p := r.Playlists[0]
	if p.ID != "6792103822" || p.Name != "Jay & friends" || p.Source != "wy" || p.Link != "https://music.163.com/playlist?id=6792103822" || p.Author != "Buradarrr" || p.PicURL != "https://p.example/wy.jpg" || p.Count != 144 || p.PlayCount != 34149068 || p.Description != "精选\n歌曲" {
		t.Fatalf("playlist = %+v", p)
	}
}

func TestPlaylistSearchQQPaginationAndMetadata(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/soso/fcgi-bin/client_music_search_songlist" || q.Get("query") != "周杰伦" || q.Get("page_no") != "1" || q.Get("num_per_page") != "2" || q.Get("format") != "json" {
			t.Errorf("wrong QQ search: %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(txPlaylistSearchFixture))
	})
	r, err := c.SearchPlaylists(context.Background(), "tx", "周杰伦", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Page != 2 || r.Limit != 2 || r.Total != 46454 || !r.HasMore || len(r.Playlists) != 1 {
		t.Fatalf("result = %+v", r)
	}
	p := r.Playlists[0]
	if p.ID != "3805603854" || p.Name != "周杰伦 & friends" || p.Source != "tx" || p.Link != "https://y.qq.com/n/ryqq/playlist/3805603854" || p.Author != "歌单狂魔" || p.PicURL != "https://p.example/tx.jpg" || p.Count != 223 || p.PlayCount != 3000000000 || p.Description != "精选\n歌曲" {
		t.Fatalf("playlist = %+v", p)
	}
}

func TestPlaylistSearchDefaultAllOrderingTotalsAndAnySourceHasMore(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search/get/web" {
			_, _ = w.Write([]byte(strings.Replace(wyPlaylistSearchFixture, `"playlistCount":462`, `"playlistCount":1`, 1)))
		} else {
			_, _ = w.Write([]byte(txPlaylistSearchFixture))
		}
	})
	r, err := c.SearchPlaylists(context.Background(), "", "周杰伦", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Playlists) != 2 || r.Playlists[0].Source != "wy" || r.Playlists[1].Source != "tx" || r.Total != 46455 || !r.HasMore || r.Limit != 2 || len(r.Warnings) != 0 {
		t.Fatalf("result = %+v", r)
	}
}

func TestPlaylistSearchPartialFailuresAndAllFailed(t *testing.T) {
	for _, failed := range []string{"wy", "tx", "all"} {
		t.Run(failed, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				source := "tx"
				fixture := txPlaylistSearchFixture
				if r.URL.Path == "/api/search/get/web" {
					source = "wy"
					fixture = wyPlaylistSearchFixture
				}
				if failed == source || failed == "all" {
					http.Error(w, "failure", 503)
					return
				}
				_, _ = w.Write([]byte(fixture))
			})
			r, err := c.SearchPlaylists(context.Background(), "all", "周杰伦", 1, 2)
			if failed == "all" {
				if err == nil {
					t.Fatal("all failed sources accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Playlists) != 1 || r.Playlists[0].Source == failed || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], failed) || !r.HasMore {
				t.Fatalf("partial result = %+v", r)
			}
		})
	}
}

func TestPlaylistSearchEmptyResultsAndJSONContract(t *testing.T) {
	for _, source := range []string{"wy", "tx", "all"} {
		t.Run(source, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/search/get/web" {
					_, _ = w.Write([]byte(`{"code":200,"result":{"playlists":[],"playlistCount":0}}`))
				} else {
					_, _ = w.Write([]byte(`{"code":0,"data":{"list":[],"sum":0}}`))
				}
			})
			r, err := c.SearchPlaylists(context.Background(), source, "no matches", 1, 20)
			if err != nil {
				t.Fatal(err)
			}
			if r.Playlists == nil || len(r.Playlists) != 0 || r.Total != 0 || r.HasMore {
				t.Fatalf("empty result = %+v", r)
			}
			encoded, _ := json.Marshal(r)
			if !strings.Contains(string(encoded), `"playlists":[]`) || !strings.Contains(string(encoded), `"has_more":false`) {
				t.Fatalf("JSON = %s", encoded)
			}
		})
	}
}

func TestPlaylistSearchBoundsAndInvalidParameters(t *testing.T) {
	requests := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"code":200,"result":{"playlists":[],"playlistCount":0}}`))
	})
	for _, tt := range []struct {
		source, keyword string
		page, limit     int
	}{{"kg", "music", 1, 20}, {"wy", " \t", 1, 20}, {"wy", "music", 1000001, 20}} {
		if _, err := c.SearchPlaylists(context.Background(), tt.source, tt.keyword, tt.page, tt.limit); err == nil {
			t.Errorf("invalid parameters accepted: %+v", tt)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid parameters made %d requests", requests)
	}
	r, err := c.SearchPlaylists(context.Background(), "wy", "music", 0, 0)
	if err != nil || r.Page != 1 || r.Limit != 20 {
		t.Fatalf("defaults: %+v, %v", r, err)
	}
	r, err = c.SearchPlaylists(context.Background(), "wy", "music", 1000000, 101)
	if err != nil || r.Page != 1000000 || r.Limit != 100 {
		t.Fatalf("bounds: %+v, %v", r, err)
	}
}

func TestPlaylistSearchRejectsMalformedPlatformResults(t *testing.T) {
	for _, source := range []string{"wy", "tx"} {
		bodies := []string{`null`, `not JSON`, `{"code":500}`, `{"code":200}`, `{"code":0}`}
		if source == "wy" {
			bodies = append(bodies, `{"code":200,"result":{}}`, `{"code":200,"result":{"playlistCount":1}}`, `{"code":200,"result":{"playlists":null,"playlistCount":0}}`, `{"code":200,"result":{"playlists":{},"playlistCount":0}}`, `{"code":200,"result":{"playlists":[],"playlistCount":"broken"}}`, `{"code":200,"result":{"playlists":[{}],"playlistCount":1}}`)
		} else {
			bodies = append(bodies, `{"code":0,"data":{}}`, `{"code":0,"data":{"sum":0}}`, `{"code":0,"data":{"list":null,"sum":0}}`, `{"code":0,"data":{"list":{},"sum":0}}`, `{"code":0,"data":{"list":[],"sum":"broken"}}`, `{"code":0,"data":{"list":[{}],"sum":1}}`)
		}
		for _, body := range bodies {
			t.Run(source+body, func(t *testing.T) {
				c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
				if _, err := c.SearchPlaylists(context.Background(), source, "music", 1, 20); err == nil {
					t.Fatalf("accepted malformed result: %s", body)
				}
			})
		}
	}
}

func TestPlaylistSearchNetEaseNoHitsOmitsArray(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"playlistCount":0},"code":200}`))
	})
	r, err := c.SearchPlaylists(context.Background(), "wy", "alxplaylistnomatch987654321xyz", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Playlists == nil || len(r.Playlists) != 0 || r.HasMore || r.Total != 0 {
		t.Fatalf("no-hit result = %+v", r)
	}
}

func TestPlaylistSearchCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request reached server") })
	for _, source := range []string{"wy", "tx", "all"} {
		if _, err := c.SearchPlaylists(ctx, source, "music", 1, 20); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: error = %v", source, err)
		}
	}
}

// This explicit opt-in keeps the fixture suite independent of availability.
func TestPlaylistSearchLive(t *testing.T) {
	if os.Getenv("ALX_CATALOG_LIVE") != "1" {
		t.Skip("set ALX_CATALOG_LIVE=1 for real public playlist search checks")
	}
	c := NewClient()
	for _, source := range []string{"wy", "tx"} {
		t.Run(source, func(t *testing.T) {
			var previousID string
			for page := 1; page <= 2; page++ {
				r, err := c.SearchPlaylists(context.Background(), source, "周杰伦", page, 2)
				if err != nil {
					t.Fatal(err)
				}
				if len(r.Playlists) == 0 || r.Total == 0 || !r.HasMore {
					t.Fatalf("unexpected public search result: %+v", r)
				}
				if r.Playlists[0].ID == previousID {
					t.Fatal("public endpoint repeated the same page")
				}
				previousID = r.Playlists[0].ID
				t.Logf("page=%d playlists=%d total=%d first=%s id=%s", page, len(r.Playlists), r.Total, r.Playlists[0].Name, previousID)
			}
		})
	}
}
