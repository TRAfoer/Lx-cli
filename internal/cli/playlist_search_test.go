package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"lxcli/internal/engine"
	"lxcli/internal/model"
)

type playlistCLITransport func(*http.Request) (*http.Response, error)

func (f playlistCLITransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func playlistCLIEngine(t *testing.T, reply func(*http.Request) string) *engine.Engine {
	t.Helper()
	setTestHome(t, t.TempDir())
	e, err := engine.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	e.Catalog.HTTPClient = &http.Client{Transport: playlistCLITransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(reply(r))), Request: r}, nil
	})}
	return e
}

func TestPlaylistSearchCommandForwardsWordsAndPagination(t *testing.T) {
	requests := 0
	e := playlistCLIEngine(t, func(r *http.Request) string {
		requests++
		q := r.URL.Query()
		if r.URL.Host != "music.163.com" || q.Get("s") != "夜 跑" || q.Get("type") != "1000" || q.Get("offset") != "3" || q.Get("limit") != "3" {
			t.Errorf("wrong playlist query: %s", r.URL)
		}
		return `{"code":200,"result":{"playlistCount":8,"playlists":[{"id":123,"name":"夜跑歌单","creator":{"nickname":"作者"},"trackCount":5}]}}`
	})
	o, err := parse([]string{"playlist", "search", "夜", "跑", "--source", "wy", "--page", "2", "--limit", "3"})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := execute(context.Background(), e, o)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Page, Limit, Total int
		HasMore            bool `json:"has_more"`
		Playlists          []struct{ ID, Name, Source, Link string }
	}
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || got.Page != 2 || got.Limit != 3 || got.Total != 8 || !got.HasMore || len(got.Playlists) != 1 || got.Playlists[0].ID != "123" || got.Playlists[0].Source != "wy" || got.Playlists[0].Link != "https://music.163.com/playlist?id=123" {
		t.Fatalf("search = %s, requests = %d", data, requests)
	}
	playlists, err := e.Store.ListPlaylists()
	if err != nil || len(playlists) != 1 || playlists[0].Name != "Favorites" {
		t.Fatalf("search persisted a playlist: %+v, %v", playlists, err)
	}
}

func TestPlaylistSearchCommandRejectsInvalidInputBeforeHTTP(t *testing.T) {
	requests := 0
	e := playlistCLIEngine(t, func(r *http.Request) string {
		requests++
		return `{}`
	})
	for _, args := range [][]string{
		{"playlist", "search"},
		{"playlist", "search", "  "},
		{"playlist", "search", "x", "--source", "kg"},
		{"playlist", "search", "x", "--page", "0"},
		{"playlist", "search", "x", "--page", "1000001"},
		{"playlist", "search", "x", "--page", "oops"},
		{"playlist", "search", "x", "--limit", "0"},
		{"playlist", "search", "x", "--limit", "101"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			o, err := parse(args)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = execute(context.Background(), e, o); err == nil {
				t.Fatalf("accepted %v", args)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("invalid inputs made %d HTTP requests", requests)
	}
}

func TestPlaylistSearchCommandDefaultsToAllSources(t *testing.T) {
	seen := map[string]bool{}
	e := playlistCLIEngine(t, func(r *http.Request) string {
		seen[r.URL.Host] = true
		if r.URL.Host == "music.163.com" {
			return `{"code":200,"result":{"playlistCount":0,"playlists":[]}}`
		}
		return `{"code":0,"data":{"sum":0,"list":[]}}`
	})
	o, err := parse([]string{"playlist", "search", "没有结果"})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := execute(context.Background(), e, o)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !seen["music.163.com"] || !seen["c.y.qq.com"] || !strings.Contains(string(data), `"playlists":[]`) || !strings.Contains(string(data), `"limit":20`) {
		t.Fatalf("default search = %s, requested hosts = %v", data, seen)
	}
}

func TestPlaylistInspectCachesSongsWithoutCreatingPlaylist(t *testing.T) {
	e := playlistCLIEngine(t, func(r *http.Request) string {
		if r.URL.Path != "/api/v6/playlist/detail" || r.URL.Query().Get("id") != "123" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		return `{"code":200,"playlist":{"name":"远程歌单","description":"说明","trackCount":1,"trackIds":[{"id":7}],"tracks":[{"id":7,"name":"歌曲","ar":[{"name":"歌手"}],"al":{"id":9,"name":"专辑"},"dt":180000}]}}`
	})
	for i := 0; i < 2; i++ {
		o, err := parse([]string{"playlist", "inspect", "https://music.163.com/playlist?id=123"})
		if err != nil {
			t.Fatal(err)
		}
		result, _, err := execute(context.Background(), e, o)
		if err != nil {
			t.Fatal(err)
		}
		p, ok := result.(model.ImportedPlaylist)
		if !ok || p.ID != "123" || p.Name != "远程歌单" || len(p.Songs) != 1 || p.Songs[0].CLIID == "" {
			t.Fatalf("inspect = %+v", result)
		}
		cached, err := e.Store.Song(p.Songs[0].CLIID)
		if err != nil || cached.Name != "歌曲" || cached.SongID != "7" {
			t.Fatalf("preview song unavailable for playback: %+v %v", cached, err)
		}
	}
	playlists, err := e.Store.ListPlaylists()
	if err != nil || len(playlists) != 1 || playlists[0].Name != "Favorites" {
		t.Fatalf("inspection persisted a playlist: %+v %v", playlists, err)
	}
}

func TestPlaylistInspectFailureDoesNotCreatePlaylist(t *testing.T) {
	e := playlistCLIEngine(t, func(r *http.Request) string { return `{"code":403,"message":"private"}` })
	o, err := parse([]string{"playlist", "inspect", "https://music.163.com/playlist?id=123"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = execute(context.Background(), e, o); err == nil || !strings.Contains(fmt.Sprint(err), "403") {
		t.Fatalf("expected platform failure, got %v", err)
	}
	playlists, err := e.Store.ListPlaylists()
	if err != nil || len(playlists) != 1 {
		t.Fatalf("failed preview changed playlists: %+v %v", playlists, err)
	}
}
