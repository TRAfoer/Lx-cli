package catalog

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"lxcli/internal/model"
)

type rewriteTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = r.target.Scheme, r.target.Host
	cloned.URL = &u
	cloned.Header.Set("X-Test-Original-Host", req.URL.Host)
	resp, err := r.base.RoundTrip(cloned)
	if resp != nil {
		resp.Request = req
	}
	return resp, err
}
func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	c := NewClient()
	c.HTTPClient = &http.Client{Transport: rewriteTransport{http.DefaultTransport, u}}
	return c
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func wySong(id int) map[string]any {
	return map[string]any{"id": id, "name": fmt.Sprintf("歌%d", id), "ar": []any{map[string]any{"name": "歌手甲"}, map[string]any{"name": "歌手乙"}}, "al": map[string]any{"id": 70, "name": "专辑", "picUrl": "https://p.example/cover.jpg"}, "dt": 205000}
}
func txSong(id int) map[string]any {
	return map[string]any{"id": id, "mid": fmt.Sprintf("MID%d", id), "title": fmt.Sprintf("QQ歌%d", id), "singer": []any{map[string]any{"name": "歌手", "mid": "singer1"}}, "album": map[string]any{"id": 7, "mid": "ALBUM7", "name": "专辑"}, "interval": 205, "file": map[string]any{"media_mid": fmt.Sprintf("MEDIA%d", id), "size_128mp3": 321}}
}

func TestNetEaseImportFetchesAllTrackIDsInOrderAndDeduplicates(t *testing.T) {
	// Catches importing only the initial preview, trusting detail response order, or losing original metadata.
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v6/playlist/detail":
			if r.URL.Query().Get("id") != "123" {
				t.Errorf("wrong playlist id: %s", r.URL.RawQuery)
			}
			respond(w, map[string]any{"code": 200, "playlist": map[string]any{"name": "测试歌单", "description": "说明", "trackCount": 4, "trackIds": []any{map[string]any{"id": 3}, map[string]any{"id": 1}, map[string]any{"id": 3}, map[string]any{"id": 2}}, "tracks": []any{wySong(1)}}})
		case "/api/song/detail/":
			var ids []int
			if err := json.Unmarshal([]byte(r.URL.Query().Get("ids")), &ids); err != nil {
				t.Errorf("invalid ids query: %v", err)
			}
			if fmt.Sprint(ids) != "[3 2]" {
				t.Errorf("missing details requested: %v", ids)
			}
			respond(w, map[string]any{"code": 200, "songs": []any{wySong(2), wySong(3)}})
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	})
	p, err := c.ImportPlaylist(context.Background(), "分享歌单《测试》 https://music.163.com/#/playlist?id=123&userid=1，快来听")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "测试歌单" || p.Source != "wy" || p.ID != "123" || p.Total != 4 || p.Description != "说明" {
		t.Fatalf("playlist = %+v", p)
	}
	if len(p.Songs) != 3 || p.Songs[0].SongID != "3" || p.Songs[1].SongID != "1" || p.Songs[2].SongID != "2" {
		t.Fatalf("order/dedup = %+v", p.Songs)
	}
	s := p.Songs[0]
	if s.Singer != "歌手甲、歌手乙" || s.AlbumID != "70" || s.AlbumName != "专辑" || s.Interval != "03:25" || s.PicURL != "https://p.example/cover.jpg" || s.CLIID == "" || !json.Valid(s.Extra) {
		t.Fatalf("metadata = %+v", s)
	}
	if len(p.Warnings) == 0 {
		t.Fatal("duplicate removal should be reported separately from missing tracks")
	}
}

func TestNetEaseImportBatchesMissingDetailsAndReportsMissingCount(t *testing.T) {
	var requests int
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v6/playlist/detail" {
			ids := []any{}
			for n := 1; n <= 205; n++ {
				ids = append(ids, map[string]any{"id": n})
			}
			respond(w, map[string]any{"code": 200, "playlist": map[string]any{"name": "大歌单", "trackCount": 207, "trackIds": ids, "tracks": []any{wySong(1)}}})
			return
		}
		requests++
		var ids []int
		_ = json.Unmarshal([]byte(r.URL.Query().Get("ids")), &ids)
		if len(ids) > 200 {
			t.Errorf("unbounded batch: %d", len(ids))
		}
		songs := []any{}
		for _, id := range ids {
			if id != 205 {
				songs = append(songs, wySong(id))
			}
		}
		respond(w, map[string]any{"code": 200, "songs": songs})
	})
	p, err := c.ImportPlaylist(context.Background(), "https://music.163.com/m/playlist?id=321")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(p.Songs) != 204 || p.Total != 207 {
		t.Fatalf("requests=%d songs=%d total=%d", requests, len(p.Songs), p.Total)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "3") {
		t.Fatalf("missing count absent: %v", p.Warnings)
	}
}

func TestQQImportPaginatesAndPreservesSongMIDAndMediaMID(t *testing.T) {
	var offsets []int
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Req struct {
				Module string `json:"module"`
				Method string `json:"method"`
				Param  struct {
					ID    int64 `json:"disstid"`
					Begin int   `json:"song_begin"`
					Count int   `json:"song_num"`
				} `json:"param"`
			} `json:"req_1"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Req.Module != "music.srfDissInfo.aiDissInfo" || body.Req.Method != "uniform_get_Dissinfo" || body.Req.Param.ID != 456 {
			t.Errorf("wrong QQ request: %+v", body)
		}
		offset := body.Req.Param.Begin
		offsets = append(offsets, offset)
		if body.Req.Param.Count < 1 || body.Req.Param.Count > 200 {
			t.Errorf("bad page size %d", body.Req.Param.Count)
		}
		var songs []any
		switch offset {
		case 0:
			songs = []any{txSong(2), txSong(1)}
		case 2:
			songs = []any{txSong(2), txSong(3)}
		default:
			t.Errorf("bad offset %d", offset)
		}
		respond(w, map[string]any{"code": 0, "req_1": map[string]any{"code": 0, "data": map[string]any{"dirinfo": map[string]any{"title": "QQ &amp; 歌单", "desc": "第一行<br>第二行"}, "total_song_num": 4, "songlist": songs}}})
	})
	p, err := c.ImportPlaylist(context.Background(), "https://i.y.qq.com/n2/m/share/details/taoge.html?id=456")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(offsets) != "[0 2]" || len(p.Songs) != 3 || p.Total != 4 || p.Name != "QQ & 歌单" || p.Description != "第一行\n第二行" {
		t.Fatalf("offsets=%v result=%+v", offsets, p)
	}
	s := p.Songs[0]
	if s.SongID != "2" || s.Songmid != "MID2" || s.AlbumID != "ALBUM7" || s.Interval != "03:25" || s.PicURL != "https://y.gtimg.cn/music/photo_new/T002R500x500M000ALBUM7.jpg" || !strings.Contains(string(s.Extra), "MEDIA2") {
		t.Fatalf("QQ metadata: %+v", s)
	}
}

func TestQQImportReportsTruncatedPageAndNeverLoops(t *testing.T) {
	requests := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		respond(w, map[string]any{"code": 0, "req_1": map[string]any{"code": 0, "data": map[string]any{"dirinfo": map[string]any{"title": "截断"}, "total_song_num": 8, "songlist": []any{txSong(1), txSong(2)}}}})
	})
	p, err := c.ImportPlaylist(context.Background(), "https://y.qq.com/n/ryqq/playlist/456")
	if err != nil {
		t.Fatal(err)
	}
	if requests > 3 || len(p.Songs) != 2 || !strings.Contains(strings.Join(p.Warnings, " "), "6") {
		t.Fatalf("requests=%d result=%+v", requests, p)
	}
}

func TestImportResolvesShortLinksAndURLForms(t *testing.T) {
	forms := []string{"https://music.163.com/playlist?id=123", "https://music.163.com/#/playlist?id=123", "https://y.music.163.com/m/playlist?id=123", "https://music.163.com/playlist/123/1/", "https://163cn.tv/abc", "https://y.qq.com/n/yqq/playlist/456.html", "https://y.qq.com/n/yqq/playsquare/456.html", "https://i.y.qq.com/n2/m/share/details/taoge.html?disstid=456", "https://c6.y.qq.com/base/fcgi-bin/u?__=abc"}
	for _, link := range forms {
		t.Run(link, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Header.Get("X-Test-Original-Host") {
				case "163cn.tv":
					http.Redirect(w, r, "https://music.163.com/#/playlist?id=123", http.StatusFound)
				case "c6.y.qq.com":
					http.Redirect(w, r, "https://y.qq.com/n/ryqq/playlist/456", http.StatusFound)
				default:
					if r.URL.Path == "/api/v6/playlist/detail" {
						respond(w, map[string]any{"code": 200, "playlist": map[string]any{"name": "网易", "trackCount": 1, "tracks": []any{wySong(1)}}})
					} else if r.URL.Path == "/cgi-bin/musicu.fcg" {
						respond(w, map[string]any{"code": 0, "req_1": map[string]any{"code": 0, "data": map[string]any{"dirinfo": map[string]any{"title": "QQ"}, "total_song_num": 1, "songlist": []any{txSong(1)}}}})
					} else {
						_, _ = io.WriteString(w, "landing page")
					}
				}
			})
			p, err := c.ImportPlaylist(context.Background(), link)
			if err != nil || len(p.Songs) != 1 {
				t.Fatalf("%+v %v", p, err)
			}
		})
	}
}

func TestImportRejectsPrivateInvalidAndEmptyPlaylists(t *testing.T) {
	for _, fixture := range []string{`{"code":403,"message":"private"}`, `{"code":200,"playlist":null}`, `{"code":200,"playlist":{"name":"空","trackCount":0,"trackIds":[],"tracks":[]}}`, `{"code":200,"playlist":{"name":"不可用","trackCount":1,"tracks":[{"id":0,"name":"bad"}]}}`} {
		t.Run(fixture, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, fixture) })
			if p, err := c.ImportPlaylist(context.Background(), "https://music.163.com/playlist?id=123"); err == nil {
				t.Fatalf("false success: %+v", p)
			}
		})
	}
	for _, fixture := range []string{`{"code":0,"req_1":{"code":20001}}`, `{"code":0,"req_1":{"code":0,"data":{"dirinfo":{"title":"空"},"total_song_num":0,"songlist":[]}}}`} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, fixture) })
		if p, err := c.ImportPlaylist(context.Background(), "https://y.qq.com/n/ryqq/playlist/123"); err == nil {
			t.Fatalf("false QQ success: %+v", p)
		}
	}
}

func TestImportRejectsNonPlaylistAndUnsafeURLs(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network request %s", r.URL)
		http.Error(w, "forbidden", 400)
	})
	for _, input := range []string{"file:///secret", "ftp://music.163.com/playlist?id=123", "https://music.163.com/song?id=123", "https://y.qq.com/n/ryqq/songDetail/MID123", "https://music.163.com.evil.example/playlist?id=123", "https://example.com/playlist?id=123", "https://user:secret@music.163.com/playlist?id=123"} {
		if _, err := c.ImportPlaylist(context.Background(), input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestImportLimitsRedirectsAndResponseSizeAndHonorsCancellation(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://163cn.tv/again", http.StatusFound)
	})
	if _, err := c.ImportPlaylist(context.Background(), "https://163cn.tv/loop"); err == nil {
		t.Fatal("redirect loop succeeded")
	}
	c = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "file:///secret", http.StatusFound) })
	if _, err := c.ImportPlaylist(context.Background(), "https://163cn.tv/unsafe"); err == nil {
		t.Fatal("unsafe redirect succeeded")
	}
	c = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat(" ", 9<<20)) })
	if _, err := c.ImportPlaylist(context.Background(), "https://music.163.com/playlist?id=123"); err == nil {
		t.Fatal("oversize response succeeded")
	}
	c = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := c.ImportPlaylist(ctx, "https://music.163.com/playlist?id=123"); err == nil || time.Since(started) > time.Second {
		t.Fatalf("cancel err=%v elapsed=%v", err, time.Since(started))
	}
}

func TestSearchNativePlatformsRetainIdentityMetadataAndPagination(t *testing.T) {
	for _, platform := range []string{"wy", "kw", "kg"} {
		t.Run(platform, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				switch platform {
				case "wy":
					if q.Get("offset") != "20" || q.Get("s") != "测试 & 音乐" {
						t.Errorf("query %v", q)
					}
					respond(w, map[string]any{"code": 200, "result": map[string]any{"songCount": 1, "songs": []any{wySong(101)}}})
				case "kw":
					if q.Get("pn") != "1" || q.Get("all") != "测试 & 音乐" {
						t.Errorf("query %v", q)
					}
					respond(w, map[string]any{"TOTAL": "1", "abslist": []any{map[string]any{"MUSICRID": "MUSIC_101", "SONGNAME": "歌101", "ARTIST": "歌手", "ALBUM": "专辑", "ALBUMID": "70", "DURATION": "205", "web_albumpic_short": "cover.jpg"}}})
				case "kg":
					if q.Get("page") != "2" || q.Get("keyword") != "测试 & 音乐" {
						t.Errorf("query %v", q)
					}
					respond(w, map[string]any{"status": 1, "error_code": 0, "data": map[string]any{"total": 1, "lists": []any{map[string]any{"FileHash": "HASH101", "SongName": "<em>歌</em>101 &amp; 你", "SingerName": "歌手", "AlbumName": "专辑", "AlbumID": "70", "Duration": 205, "Image": "https://img.example/{size}/cover.jpg"}}}})
				}
			})
			songs, err := c.Search(context.Background(), platform, "测试 & 音乐", 2, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(songs) != 1 || songs[0].CLIID == "" || songs[0].Source != platform || songs[0].Interval != "03:25" || songs[0].AlbumID != "70" || songs[0].PicURL == "" {
				t.Fatalf("songs=%+v", songs)
			}
			if platform == "kg" && (songs[0].Hash != "HASH101" || songs[0].Name != "歌101 & 你") {
				t.Fatalf("kg=%+v", songs[0])
			}
		})
	}
}

func TestSearchFailureIsNotAnEmptySuccess(t *testing.T) {
	for _, platform := range []string{"wy", "kw", "kg"} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"code":403,"status":0,"message":"blocked"}`)
		})
		if _, err := c.Search(context.Background(), platform, "test", 1, 20); err == nil {
			t.Errorf("%s returned false success", platform)
		}
	}
	for _, platform := range []string{"tx", "mg", "unknown"} {
		if _, err := NewClient().Search(context.Background(), platform, "test", 1, 20); err == nil {
			t.Errorf("%s should explicitly report unsupported", platform)
		}
	}
}

func TestKuwoSearchDecodesSingleQuotedStringsWithoutCorruptingApostrophes(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{'TOTAL':'1','abslist':[{'MUSICRID':'MUSIC_101','SONGNAME':'Don\'t say "no"','ARTIST':'A &amp; B','DURATION':'205'}]}`)
	})
	songs, err := c.Search(context.Background(), "kw", "test", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != 1 || songs[0].Name != `Don't say "no"` || songs[0].Singer != "A & B" {
		t.Fatalf("Kuwo quoting corrupted: %+v", songs)
	}
}

func TestLyricRejectsFakeValuesAndFormatsKuwoSeconds(t *testing.T) {
	for _, value := range []string{"", "null", "undefined", "[object Object]"} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { respond(w, map[string]any{"lyric": value}) })
		if _, err := c.Lyric(context.Background(), model.Song{SongID: "1", Source: "wy"}); err == nil {
			t.Errorf("accepted fake lyric %q", value)
		}
	}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { respond(w, map[string]any{"lyric": "[00:01.00]你好"}) })
	if lyric, err := c.Lyric(context.Background(), model.Song{SongID: "1", Source: "wy"}); err != nil || lyric != "[00:01.00]你好" {
		t.Fatalf("wy lyric=%q %v", lyric, err)
	}
	c = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mid") != "123" {
			t.Errorf("id %s", r.URL)
		}
		respond(w, map[string]any{"lrclist": []any{map[string]any{"time": "65.25", "lineinfo": "你 &amp; 我"}}})
	})
	if lyric, err := c.Lyric(context.Background(), model.Song{SongID: "123", Source: "kw"}); err != nil || lyric != "[01:05.25]你 & 我\n" {
		t.Fatalf("kw lyric=%q %v", lyric, err)
	}
}

func TestPicReturnsValidCachedImageAndFetchesMissingNetEaseImage(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ids") != "[123]" {
			t.Errorf("query %s", r.URL)
		}
		respond(w, map[string]any{"code": 200, "songs": []any{wySong(123)}})
	})
	if pic, err := c.Pic(context.Background(), model.Song{PicURL: "https://example.com/a.jpg"}); err != nil || pic != "https://example.com/a.jpg" {
		t.Fatalf("cached pic %s %v", pic, err)
	}
	if pic, err := c.Pic(context.Background(), model.Song{SongID: strconv.Itoa(123), Source: "wy"}); err != nil || pic != "https://p.example/cover.jpg" {
		t.Fatalf("fetched pic %s %v", pic, err)
	}
}

func TestUnsupportedNativeLyricsReturnPlatformDiagnostic(t *testing.T) {
	c := NewClient()
	for _, song := range []model.Song{{Source: "kg", SongID: "HASH123"}, {Source: "tx", Songmid: "MID123"}, {Source: "mg", SongID: "1"}} {
		if _, err := c.Lyric(context.Background(), song); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("unsupported %s diagnostic: %v", song.Source, err)
		}
	}
}

func TestKuwoLyricFallsBackToBoundedCompressedLyrics(t *testing.T) {
	// The original mobile endpoint currently serves an HTML application page.
	// Its replacement wraps a zlib-compressed, base64/XOR lyric payload.
	plain := []byte("[00:01.00]<100,200>你 &amp; 我\n")
	for i := range plain {
		plain[i] ^= []byte("yeelion")[i%7]
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write([]byte(base64.StdEncoding.EncodeToString(plain)))
	_ = zw.Close()
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mobi.s" {
			_, _ = io.WriteString(w, "<html>mobile application</html>")
			return
		}
		if r.URL.Query().Get("rid") != "123" || r.URL.Query().Get("lrcx") != "1" {
			t.Errorf("wrong lyric query %s", r.URL)
		}
		_, _ = w.Write(append([]byte("TP=content\r\nlrcx=1\r\n\r\n"), compressed.Bytes()...))
	})
	lyric, err := c.Lyric(context.Background(), model.Song{Source: "kw", SongID: "123"})
	if err != nil || lyric != "[00:01.00]你 & 我\n" {
		t.Fatalf("compressed lyric=%q err=%v", lyric, err)
	}
}

func TestNetEaseImportWarnsWhenPlatformOmitsTotal(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"code": 200, "playlist": map[string]any{"name": "不确定完整性", "tracks": []any{wySong(1)}}})
	})
	p, err := c.ImportPlaylist(context.Background(), "https://music.163.com/playlist?id=1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Songs) != 1 || !strings.Contains(strings.Join(p.Warnings, " "), "总曲目数") {
		t.Fatalf("unknown total silently treated as complete: %+v", p)
	}
}

func TestImportPageFailurePreservesAvailableSongsAndReportsMissing(t *testing.T) {
	for _, platform := range []string{"wy", "tx"} {
		t.Run(platform, func(t *testing.T) {
			var requests int
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests > 1 {
					http.Error(w, "temporary endpoint failure", 503)
					return
				}
				if platform == "wy" {
					respond(w, map[string]any{"code": 200, "playlist": map[string]any{"name": "部分可用", "trackCount": 3, "trackIds": []any{map[string]any{"id": 1}, map[string]any{"id": 2}, map[string]any{"id": 3}}, "tracks": []any{wySong(1)}}})
				} else {
					respond(w, map[string]any{"code": 0, "req_1": map[string]any{"code": 0, "data": map[string]any{"dirinfo": map[string]any{"title": "部分可用"}, "total_song_num": 3, "songlist": []any{txSong(1)}}}})
				}
			})
			link := "https://music.163.com/playlist?id=1"
			if platform == "tx" {
				link = "https://y.qq.com/n/ryqq/playlist/1"
			}
			p, err := c.ImportPlaylist(context.Background(), link)
			if err != nil {
				t.Fatal(err)
			}
			warnings := strings.Join(p.Warnings, " ")
			if len(p.Songs) != 1 || p.Total != 3 || !strings.Contains(warnings, "2 首歌曲无法获取") || !strings.Contains(warnings, "失败") {
				t.Fatalf("partial result: %+v", p)
			}
		})
	}
}

func TestKuwoLyricRejectsDecompressionBomb(t *testing.T) {
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(bytes.Repeat([]byte("A"), 2<<20))
	_ = zw.Close()
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mobi.s" {
			_, _ = io.WriteString(w, "<html>mobile application</html>")
			return
		}
		_, _ = w.Write(append([]byte("TP=content\r\nlrcx=1\r\n\r\n"), compressed.Bytes()...))
	})
	if _, err := c.Lyric(context.Background(), model.Song{Source: "kw", SongID: "1"}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("decompression bomb result: %v", err)
	}
}
