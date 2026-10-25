package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lxcli/internal/catalog"
	"lxcli/internal/config"
	"lxcli/internal/model"
	"lxcli/internal/source"
	"lxcli/internal/store"
)

type Engine struct {
	Config  *config.Config
	Store   *store.Store
	Catalog *catalog.Client
	Runtime *source.Runtime
	HTTP    *http.Client
}

func Open(path string) (*Engine, error) {
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	s, err := store.Open(c.DBPath)
	if err != nil {
		return nil, err
	}
	return &Engine{Config: c, Store: s, Catalog: catalog.NewClient(), Runtime: source.NewRuntime(), HTTP: &http.Client{Timeout: 90 * time.Second}}, nil
}
func (e *Engine) Close() error { return e.Store.Close() }

func (e *Engine) AddSource(ctx context.Context, path, name string) (model.Source, error) {
	var v model.Source
	var b []byte
	var err error
	if isHTTP(path) {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			return v, err
		}
		var resp *http.Response
		resp, err = e.HTTP.Do(req)
		if err != nil {
			return v, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return v, fmt.Errorf("音源下载失败 HTTP %d", resp.StatusCode)
		}
		b, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
		v.SourceURL = path
	} else {
		path = config.Expand(path)
		var info os.FileInfo
		info, err = os.Stat(path)
		if err == nil && info.Size() > 8<<20 {
			return v, errors.New("音源脚本超过 8 MiB")
		}
		if err == nil {
			b, err = os.ReadFile(path)
		}
	}
	if err != nil {
		return v, err
	}
	if len(b) > 8<<20 {
		return v, errors.New("音源脚本超过 8 MiB")
	}
	m, err := e.Runtime.Inspect(ctx, string(b))
	if err != nil {
		return v, fmt.Errorf("音源初始化失败: %w", err)
	}
	if len(m.Platforms) == 0 {
		return v, errors.New("音源没有注册可用平台")
	}
	h := sha256.Sum256(b)
	v.ID = hex.EncodeToString(h[:])[:16]
	v.ContentHash = hex.EncodeToString(h[:])
	v.Name = m.Name
	if name != "" {
		v.Name = name
	}
	if v.Name == "" {
		v.Name = filepath.Base(path)
	}
	v.Author = m.Author
	v.Version = m.Version
	v.Homepage = m.Homepage
	v.Enabled = true
	v.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	v.UpdatedAt = v.CreatedAt
	p, _ := json.Marshal(m.Platforms)
	q, _ := json.Marshal(m.Qualities)
	v.Platforms = string(p)
	v.Qualities = string(q)
	v.ScriptPath = filepath.Join(e.Config.SourcesDir, v.ID+".js")
	if err = config.AtomicWrite(v.ScriptPath, b, 0o600); err != nil {
		return v, err
	}
	if err = e.Store.PutSource(v); err != nil {
		return v, err
	}
	return v, nil
}
func (e *Engine) Sources() ([]model.Source, error) {
	all, err := e.Store.Sources()
	if err != nil {
		return nil, err
	}
	rank := func(v model.Source) int {
		for i, p := range e.Config.Source.Priority {
			if p == v.ID || p == v.Name {
				return i
			}
		}
		return len(e.Config.Source.Priority)
	}
	sort.SliceStable(all, func(i, j int) bool { return rank(all[i]) < rank(all[j]) })
	return all, nil
}
func supports(s model.Source, platform string) bool {
	var platforms []string
	_ = json.Unmarshal([]byte(s.Platforms), &platforms)
	for _, v := range platforms {
		if v == platform {
			return true
		}
	}
	return false
}
func isHTTP(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func musicInfo(s model.Song) map[string]any {
	m := map[string]any{}
	raw := s.Extra
	var text string
	if json.Unmarshal(raw, &text) == nil {
		raw = []byte(text)
	}
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]any{}
	}
	m["id"] = s.SongID
	m["songId"] = s.SongID
	m["songmid"] = s.Songmid
	if s.Songmid == "" {
		m["songmid"] = s.SongID
	}
	m["name"] = s.Name
	m["singer"] = s.Singer
	m["source"] = s.Source
	m["albumName"] = s.AlbumName
	m["albumId"] = s.AlbumID
	m["interval"] = s.Interval
	m["pic"] = s.PicURL
	m["hash"] = s.Hash
	return m
}
func resultString(raw json.RawMessage, keys ...string) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range keys {
			if r := m[k]; len(r) > 0 {
				if v := resultString(r, keys...); v != "" {
					return v
				}
			}
		}
	}
	return ""
}
func (e *Engine) Resolve(ctx context.Context, song model.Song, quality string) (string, error) {
	if song.Source == "local" {
		p := config.Expand(song.SongID)
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return filepath.Abs(p)
	}
	if song.Source == "url" && isHTTP(song.SongID) {
		return song.SongID, nil
	}
	all, err := e.Sources()
	if err != nil {
		return "", err
	}
	if quality == "" {
		quality = e.Config.Source.DefaultQuality
	}
	failures := []string{}
	qualities := []string{quality}
	for _, q := range e.Config.Source.QualityFallback {
		if q != quality {
			qualities = append(qualities, q)
		}
	}
	for _, src := range all {
		if !src.Enabled || !supports(src, song.Source) {
			continue
		}
		script, err := os.ReadFile(src.ScriptPath)
		if err != nil {
			failures = append(failures, src.Name+": "+err.Error())
			continue
		}
		var supported map[string][]string
		_ = json.Unmarshal([]byte(src.Qualities), &supported)
		for _, q := range qualities {
			if list := supported[song.Source]; len(list) > 0 {
				found := false
				for _, v := range list {
					if q == v {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			raw, err := e.Runtime.Call(ctx, string(script), source.Request{Action: "musicUrl", Source: song.Source, Info: map[string]any{"type": q, "musicInfo": musicInfo(song)}})
			if err == nil {
				v := resultString(raw, "url", "data")
				if isHTTP(v) {
					return v, nil
				}
				err = errors.New("音源没有返回有效 HTTP(S) 音频地址")
			}
			failures = append(failures, src.Name+"/"+q+": "+err.Error())
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
		}
	}
	if len(failures) == 0 {
		return "", errors.New("没有可用 JS 音源，请先 source add <脚本路径> 注册支持该平台的音源")
	}
	return "", fmt.Errorf("音频地址解析失败: %s", strings.Join(failures, "; "))
}

func (e *Engine) Search(ctx context.Context, keyword, platform string, page, limit int) ([]model.Song, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, errors.New("搜索关键词不能为空")
	}
	if platform == "" {
		platform = e.Config.Source.DefaultSource
	}
	platforms := []string{platform}
	if platform == "all" {
		platforms = e.Config.Source.PlatformPriority
	}
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	type response struct {
		songs []model.Song
		err   error
	}
	ch := make(chan response, 64)
	var wg sync.WaitGroup
	all, err := e.Sources()
	if err != nil {
		return nil, err
	}
	for _, p := range platforms {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			songs, err := e.Catalog.Search(ctx, p, keyword, page, limit)
			ch <- response{songs, err}
		}()
		for _, src := range all {
			src := src
			if !src.Enabled || !supports(src, p) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				script, err := os.ReadFile(src.ScriptPath)
				if err != nil {
					ch <- response{err: err}
					return
				}
				raw, err := e.Runtime.Call(ctx, string(script), source.Request{Action: "musicSearch", Source: p, Info: map[string]any{"text": keyword, "page": page, "limit": limit}})
				if err != nil {
					ch <- response{err: err}
					return
				}
				songs, err := parseJSSongs(raw, p)
				ch <- response{songs, err}
			}()
		}
	}
	go func() { wg.Wait(); close(ch) }()
	songs := []model.Song{}
	seen := map[string]bool{}
	failures := []string{}
	success := false
	for r := range ch {
		if r.err != nil {
			failures = append(failures, r.err.Error())
			continue
		}
		success = true
		for _, s := range r.songs {
			s.Normalize()
			if s.SongID == "" || seen[s.CLIID] {
				continue
			}
			seen[s.CLIID] = true
			songs = append(songs, s)
		}
	}
	if len(songs) == 0 && !success {
		return nil, fmt.Errorf("所有搜索接口失败: %s", strings.Join(failures, "; "))
	}
	sort.SliceStable(songs, func(i, j int) bool {
		if songs[i].Source == songs[j].Source {
			return songs[i].CLIID < songs[j].CLIID
		}
		return songs[i].Source < songs[j].Source
	})
	if err = e.Store.CacheSongs(songs); err != nil {
		return nil, err
	}
	return songs, nil
}

func parseJSSongs(raw json.RawMessage, platform string) ([]model.Song, error) {
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return nil, errors.New("JS 搜索结果格式无效")
		}
		for _, key := range []string{"list", "songs", "data"} {
			if b := m[key]; len(b) > 0 {
				return parseJSSongs(b, platform)
			}
		}
		return nil, errors.New("该 JS 音源未实现搜索")
	}
	out := []model.Song{}
	str := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
		}
		return ""
	}
	for _, r := range rows {
		s := model.Song{Source: platform, SongID: str(r, "song_id", "songmid", "id", "hash"), Songmid: str(r, "songmid", "id"), Name: str(r, "name", "title"), Singer: str(r, "singer", "artist"), AlbumName: str(r, "albumName", "album_name"), AlbumID: str(r, "albumId", "album_id"), Interval: str(r, "interval"), PicURL: str(r, "pic", "pic_url", "picUrl"), Hash: str(r, "hash")}
		s.Extra, _ = json.Marshal(r)
		s.Normalize()
		out = append(out, s)
	}
	return out, nil
}

func cleanLyric(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "null") || strings.EqualFold(s, `Null"null"`) || isHTTP(s) {
		return ""
	}
	return s
}
func (e *Engine) Lyric(ctx context.Context, song model.Song) (string, error) {
	var cached string
	if e.Store.Get("lyric:"+song.CLIID, &cached) == nil && cleanLyric(cached) != "" {
		return cached, nil
	}
	all, err := e.Sources()
	if err != nil {
		return "", err
	}
	for _, src := range all {
		if !src.Enabled || !supports(src, song.Source) {
			continue
		}
		script, err := os.ReadFile(src.ScriptPath)
		if err != nil {
			continue
		}
		raw, err := e.Runtime.Call(ctx, string(script), source.Request{Action: "lyric", Source: song.Source, Info: map[string]any{"musicInfo": musicInfo(song)}})
		if err == nil {
			if text := cleanLyric(resultString(raw, "lyric", "lrc", "data")); text != "" {
				_ = e.Store.Set("lyric:"+song.CLIID, text)
				return text, nil
			}
		}
	}
	text, err := e.Catalog.Lyric(ctx, song)
	text = cleanLyric(text)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", errors.New("暂无歌词")
	}
	_ = e.Store.Set("lyric:"+song.CLIID, text)
	return text, nil
}
