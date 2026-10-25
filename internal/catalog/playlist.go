package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lxcli/internal/model"
)

const maxPlaylistTracks = 20000

var shareURL = regexp.MustCompile(`(?i)https?://[^\s<>"'“”]+`)
var playlistPathID = regexp.MustCompile(`/(?:playlist|playsquare)/(\d+)(?:[/.]|$)`)

func playlistPlatform(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if host == "music.163.com" || strings.HasSuffix(host, ".music.163.com") {
		return "wy"
	}
	if host == "y.qq.com" || strings.HasSuffix(host, ".y.qq.com") {
		return "tx"
	}
	return ""
}
func shortLink(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return host == "163cn.tv" || host == "url.cn" || ((host == "y.qq.com" || strings.HasSuffix(host, ".y.qq.com")) && strings.HasPrefix(u.Path, "/base/fcgi-bin/u"))
}
func playlistIdentity(u *url.URL) (string, string) {
	platform := playlistPlatform(u)
	if platform == "" {
		return "", ""
	}
	path, q := u.Path, u.Query()
	if u.Fragment != "" {
		fragment, err := url.Parse(strings.TrimPrefix(u.Fragment, "!"))
		if err == nil && strings.Contains(fragment.Path, "playlist") {
			path, q = fragment.Path, fragment.Query()
		}
	}
	if match := playlistPathID.FindStringSubmatch(path); len(match) > 1 && validID(match[1]) {
		return platform, match[1]
	}
	if platform == "wy" {
		if strings.HasSuffix(strings.TrimRight(path, "/"), "/playlist") && validID(q.Get("id")) {
			return platform, q.Get("id")
		}
	} else {
		if validID(q.Get("disstid")) {
			return platform, q.Get("disstid")
		}
		if (strings.HasSuffix(path, "/taoge.html") || strings.HasSuffix(strings.TrimRight(path, "/"), "/playlist")) && validID(q.Get("id")) {
			return platform, q.Get("id")
		}
	}
	return "", ""
}

func (c *Client) resolvePlaylistLink(ctx context.Context, input string) (string, string, *url.URL, error) {
	raw := shareURL.FindString(input)
	if raw == "" {
		return "", "", nil, errors.New("no HTTP(S) playlist link found in input")
	}
	if i := strings.IndexAny(raw, "，。；！？）】》"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimRight(raw, ".,;!)]}")
	u, err := url.Parse(raw)
	if err != nil || !safeHTTPURL(u) {
		return "", "", nil, errors.New("invalid playlist URL; only HTTP(S) without credentials is allowed")
	}
	if platform, id := playlistIdentity(u); id != "" {
		return platform, id, u, nil
	}
	if !shortLink(u) {
		return "", "", nil, errors.New("unsupported playlist link; use a public NetEase or QQ Music playlist link")
	}
	// Resolve HTTP redirects with the same response and timeout limits as API
	// requests. Validate every hop before the next request is issued.
	base := c.HTTPClient
	if base == nil {
		base = NewClient().HTTPClient
	}
	copy := *base
	previousCheck := copy.CheckRedirect
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !safeHTTPURL(req.URL) || (playlistPlatform(req.URL) == "" && !shortLink(req.URL)) {
			return errors.New("short link redirected outside supported music platforms")
		}
		if previousCheck != nil {
			return previousCheck(req, via)
		}
		return nil
	}
	resolver := Client{HTTPClient: &copy}
	_, final, err := resolver.request(ctx, http.MethodGet, u.String(), nil, nil)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve playlist short link: %w", err)
	}
	platform, id := playlistIdentity(final)
	if id == "" {
		return "", "", nil, errors.New("short link did not resolve to a supported playlist")
	}
	return platform, id, final, nil
}

// ImportPlaylist resolves public NetEase and QQ playlists without guessing song
// identities through search. Incomplete public responses carry explicit counts
// in Warnings; inaccessible, invalid and empty playlists return an error.
func (c *Client) ImportPlaylist(ctx context.Context, link string) (model.ImportedPlaylist, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	platform, id, final, err := c.resolvePlaylistLink(ctx, strings.TrimSpace(link))
	if err != nil {
		return model.ImportedPlaylist{}, err
	}
	var p model.ImportedPlaylist
	switch platform {
	case "wy":
		p, err = c.importNetEase(ctx, id)
	case "tx":
		p, err = c.importQQ(ctx, id)
	default:
		err = errors.New("unsupported playlist platform")
	}
	if err != nil {
		return model.ImportedPlaylist{}, err
	}
	if len(p.Songs) == 0 {
		return model.ImportedPlaylist{}, errors.New("playlist is empty, private, unavailable, or contains no usable tracks")
	}
	p.ID, p.Source, p.Link = id, platform, final.String()
	return p, nil
}

func (c *Client) importNetEase(ctx context.Context, id string) (model.ImportedPlaylist, error) {
	q := url.Values{"id": {id}, "n": {"100000"}, "s": {"0"}}
	body, err := c.json(ctx, http.MethodGet, queryURL("https://music.163.com/api/v6/playlist/detail", q), nil, map[string]string{"Referer": "https://music.163.com/", "Origin": "https://music.163.com"})
	if err != nil {
		return model.ImportedPlaylist{}, err
	}
	if err := apiCode(body, 200); err != nil {
		return model.ImportedPlaylist{}, err
	}
	data := object(body["playlist"])
	if data == nil || text(data["name"]) == "" {
		return model.ImportedPlaylist{}, errors.New("NetEase playlist is private, unavailable, or missing its title")
	}
	p := model.ImportedPlaylist{Name: text(data["name"]), Description: text(data["description"]), Total: number(data["trackCount"])}
	if _, ok := data["trackCount"]; !ok {
		p.Warnings = append(p.Warnings, "平台未提供总曲目数，无法确认歌单是否完整")
	}
	details := map[string]model.Song{}
	var previewIDs []string
	for _, value := range list(data["tracks"]) {
		song := netEaseSong(object(value))
		if song.SongID != "" && song.Name != "" {
			details[song.SongID] = song
			previewIDs = append(previewIDs, song.SongID)
		}
	}
	var ids []string
	for _, value := range list(data["trackIds"]) {
		id := str(object(value)["id"])
		if validID(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		ids = previewIDs
	}
	if len(ids) > p.Total {
		p.Total = len(ids)
	}
	if len(ids) == 0 {
		return model.ImportedPlaylist{}, errors.New("NetEase playlist is empty or its tracks are inaccessible")
	}
	if len(ids) > maxPlaylistTracks {
		ids = ids[:maxPlaylistTracks]
		p.Warnings = append(p.Warnings, fmt.Sprintf("歌单超过 %d 首处理上限，已截断", maxPlaylistTracks))
	}
	var missing []string
	requested := map[string]bool{}
	for _, id := range ids {
		if _, ok := details[id]; !ok && !requested[id] {
			missing = append(missing, id)
			requested[id] = true
		}
	}
	for begin := 0; begin < len(missing); begin += 200 {
		end := begin + 200
		if end > len(missing) {
			end = len(missing)
		}
		q := url.Values{"ids": {"[" + strings.Join(missing[begin:end], ",") + "]"}}
		detail, fetchErr := c.json(ctx, http.MethodGet, queryURL("https://music.163.com/api/song/detail/", q), nil, map[string]string{"Referer": "https://music.163.com/"})
		if fetchErr == nil {
			fetchErr = apiCode(detail, 200)
		}
		if fetchErr != nil {
			if ctx.Err() != nil {
				return model.ImportedPlaylist{}, ctx.Err()
			}
			p.Warnings = append(p.Warnings, fmt.Sprintf("获取第 %d–%d 个缺失条目的详情失败：%v", begin+1, end, fetchErr))
			continue
		}
		for _, value := range list(detail["songs"]) {
			song := netEaseSong(object(value))
			if song.SongID != "" && song.Name != "" {
				details[song.SongID] = song
			}
		}
	}
	seen := map[string]bool{}
	available, duplicates := 0, 0
	for _, id := range ids {
		song, ok := details[id]
		if !ok {
			continue
		}
		available++
		if seen[id] {
			duplicates++
			continue
		}
		seen[id] = true
		p.Songs = append(p.Songs, song)
	}
	appendImportWarnings(&p, p.Total-available, duplicates)
	return p, nil
}

func appendImportWarnings(p *model.ImportedPlaylist, missing, duplicates int) {
	if missing > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("原歌单共 %d 首，%d 首歌曲无法获取或无权限；导入结果不完整", p.Total, missing))
	}
	if duplicates > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("已移除 %d 个重复条目，保留原始顺序", duplicates))
	}
}

func qqSong(item map[string]any) model.Song {
	id := str(first(item["id"], item["songid"]))
	mid := str(first(item["mid"], item["songmid"]))
	if id == "" || id == "0" {
		id = mid
	}
	if id == "" {
		return model.Song{}
	}
	album := object(item["album"])
	albumID := str(first(album["mid"], item["albummid"]))
	albumName := text(first(album["name"], item["albumname"]))
	pic := ""
	if albumID != "" {
		pic = "https://y.gtimg.cn/music/photo_new/T002R500x500M000" + url.PathEscape(albumID) + ".jpg"
	}
	if (albumName == "" || albumName == "空") && len(list(item["singer"])) > 0 {
		if singerMID := str(object(list(item["singer"])[0])["mid"]); singerMID != "" {
			pic = "https://y.gtimg.cn/music/photo_new/T001R500x500M000" + url.PathEscape(singerMID) + ".jpg"
		}
	}
	mediaMID := str(first(object(item["file"])["media_mid"], item["strMediaMid"]))
	song := model.Song{SongID: id, Songmid: mid, Name: text(first(item["title"], item["songname"])), Singer: names(item["singer"]), Source: "tx", Interval: duration(number(item["interval"])), AlbumName: albumName, AlbumID: albumID, PicURL: pic, Extra: rawMetadata(item, map[string]any{"songId": id, "albumMid": albumID, "strMediaMid": mediaMID})}
	song.Normalize()
	return song
}

func (c *Client) qqPage(ctx context.Context, id string, begin int) (map[string]any, error) {
	numericID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, errors.New("QQ playlist ID is too large")
	}
	payload := map[string]any{"comm": map[string]any{"cv": 4747474, "ct": 24, "format": "json", "inCharset": "utf-8", "outCharset": "utf-8", "platform": "yqq.json", "needNewCode": 1, "uin": 0}, "req_1": map[string]any{"module": "music.srfDissInfo.aiDissInfo", "method": "uniform_get_Dissinfo", "param": map[string]any{"disstid": numericID, "userinfo": 1, "tag": 1, "orderlist": 1, "song_begin": begin, "song_num": 200, "onlysonglist": 0, "enc_host_uin": ""}}}
	body, err := c.json(ctx, http.MethodPost, "https://u.y.qq.com/cgi-bin/musicu.fcg", payload, map[string]string{"Origin": "https://y.qq.com", "Referer": "https://y.qq.com/n/ryqq/playlist/" + id})
	if err != nil {
		return nil, err
	}
	if err := apiCode(body, 0); err != nil {
		return nil, err
	}
	req := object(body["req_1"])
	if err := apiCode(req, 0); err != nil {
		return nil, err
	}
	data := object(req["data"])
	if data == nil {
		return nil, errors.New("QQ playlist endpoint returned no data")
	}
	return data, nil
}

func (c *Client) importQQ(ctx context.Context, id string) (model.ImportedPlaylist, error) {
	data, err := c.qqPage(ctx, id, 0)
	if err != nil {
		if ctx.Err() != nil {
			return model.ImportedPlaylist{}, ctx.Err()
		}
		legacy, legacyErr := c.importQQLegacy(ctx, id)
		if legacyErr == nil {
			return legacy, nil
		}
		return model.ImportedPlaylist{}, fmt.Errorf("QQ playlist request failed: %v; legacy endpoint: %w", err, legacyErr)
	}
	info := object(data["dirinfo"])
	p := model.ImportedPlaylist{Name: text(info["title"]), Description: playlistDescription(info["desc"]), Total: number(data["total_song_num"])}
	if p.Name == "" {
		return model.ImportedPlaylist{}, errors.New("QQ playlist is private, unavailable, or missing its title")
	}
	seen := map[string]bool{}
	seenPages := map[[32]byte]bool{}
	begin, available, duplicates := 0, 0, 0
	if _, ok := data["total_song_num"]; !ok {
		p.Warnings = append(p.Warnings, "平台未提供总曲目数，无法确认歌单是否完整")
	}
	for {
		items := list(data["songlist"])
		if len(items) == 0 {
			break
		}
		pageJSON, _ := json.Marshal(items)
		fingerprint := sha256.Sum256(pageJSON)
		if seenPages[fingerprint] {
			p.Warnings = append(p.Warnings, "平台重复返回同一页，已停止分页")
			break
		}
		seenPages[fingerprint] = true
		for _, value := range items {
			if begin >= maxPlaylistTracks {
				break
			}
			begin++
			song := qqSong(object(value))
			if song.SongID == "" || song.Name == "" {
				continue
			}
			available++
			if seen[song.SongID] {
				duplicates++
				continue
			}
			seen[song.SongID] = true
			p.Songs = append(p.Songs, song)
		}
		if begin > p.Total {
			p.Total = begin
		}
		if begin >= p.Total {
			break
		}
		if begin >= maxPlaylistTracks {
			p.Warnings = append(p.Warnings, fmt.Sprintf("歌单超过 %d 首处理上限，已截断", maxPlaylistTracks))
			break
		}
		data, err = c.qqPage(ctx, id, begin)
		if err != nil {
			if ctx.Err() != nil {
				return model.ImportedPlaylist{}, ctx.Err()
			}
			p.Warnings = append(p.Warnings, fmt.Sprintf("获取第 %d 首起的歌单分页失败：%v", begin+1, err))
			break
		}
	}
	appendImportWarnings(&p, p.Total-available, duplicates)
	return p, nil
}

func playlistDescription(value any) string {
	return strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n").Replace(text(value))
}

func (c *Client) importQQLegacy(ctx context.Context, id string) (model.ImportedPlaylist, error) {
	q := url.Values{"type": {"1"}, "json": {"1"}, "utf8": {"1"}, "onlysong": {"0"}, "new_format": {"1"}, "disstid": {id}, "loginUin": {"0"}, "hostUin": {"0"}, "format": {"json"}, "inCharset": {"utf8"}, "outCharset": {"utf-8"}, "notice": {"0"}, "platform": {"yqq.json"}, "needNewCode": {"0"}}
	body, err := c.json(ctx, http.MethodGet, queryURL("https://c.y.qq.com/qzone/fcg-bin/fcg_ucc_getcdinfo_byids_cp.fcg", q), nil, map[string]string{"Origin": "https://y.qq.com", "Referer": "https://y.qq.com/n/ryqq/playlist/" + id})
	if err != nil {
		return model.ImportedPlaylist{}, err
	}
	if err := apiCode(body, 0); err != nil {
		return model.ImportedPlaylist{}, err
	}
	if subcode, ok := body["subcode"]; ok && str(subcode) != "0" {
		return model.ImportedPlaylist{}, fmt.Errorf("QQ playlist unavailable: subcode %s", str(subcode))
	}
	cdlist := list(body["cdlist"])
	if len(cdlist) == 0 {
		return model.ImportedPlaylist{}, errors.New("QQ playlist is private or unavailable")
	}
	data := object(cdlist[0])
	items := list(data["songlist"])
	p := model.ImportedPlaylist{Name: text(data["dissname"]), Description: playlistDescription(data["desc"]), Total: number(first(data["total_song_num"], data["songnum"]))}
	if p.Name == "" {
		return model.ImportedPlaylist{}, errors.New("QQ playlist title is missing")
	}
	if p.Total == 0 {
		p.Total = len(items)
		p.Warnings = append(p.Warnings, "备用接口未提供总曲目数，无法确认歌单是否完整")
	}
	if len(items) > p.Total {
		p.Total = len(items)
	}
	if len(items) > maxPlaylistTracks {
		items = items[:maxPlaylistTracks]
	}
	seen := map[string]bool{}
	available, duplicates := 0, 0
	for _, value := range items {
		song := qqSong(object(value))
		if song.SongID == "" || song.Name == "" {
			continue
		}
		available++
		if seen[song.SongID] {
			duplicates++
			continue
		}
		seen[song.SongID] = true
		p.Songs = append(p.Songs, song)
	}
	appendImportWarnings(&p, p.Total-available, duplicates)
	return p, nil
}
