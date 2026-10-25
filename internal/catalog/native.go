package catalog

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lxcli/internal/model"
)

// Search supports the native adapters supplied by the original project. QQ and
// Migu searches require a configured JS source rather than a fabricated result.
func (c *Client) Search(ctx context.Context, platform, keyword string, page, limit int) ([]model.Song, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if strings.TrimSpace(keyword) == "" {
		return nil, errors.New("search keyword is empty")
	}
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		return nil, errors.New("search page is too large")
	}
	if limit < 1 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	var endpoint string
	q := url.Values{}
	switch platform {
	case "wy":
		endpoint = "https://music.163.com/api/search/get/web"
		q = url.Values{"s": {keyword}, "type": {"1"}, "offset": {strconv.Itoa((page - 1) * limit)}, "limit": {strconv.Itoa(limit)}}
	case "kw":
		endpoint = "https://search.kuwo.cn/r.s"
		q = url.Values{"client": {"kt"}, "all": {keyword}, "pn": {strconv.Itoa(page - 1)}, "rn": {strconv.Itoa(limit)}, "rformat": {"json"}, "encoding": {"utf8"}, "ver": {"kwplayer_ar_9.2.2.1"}, "vipver": {"1"}, "show_copyright_off": {"1"}, "newver": {"1"}, "ft": {"music"}}
	case "kg":
		endpoint = "https://songsearch.kugou.com/song_search_v2"
		q = url.Values{"keyword": {keyword}, "page": {strconv.Itoa(page)}, "pagesize": {strconv.Itoa(limit)}, "platform": {"WebFilter"}, "filter": {"2"}, "iscorrection": {"1"}}
	default:
		return nil, fmt.Errorf("native search is unsupported for platform %q; use a JS source", platform)
	}
	var body map[string]any
	var err error
	if platform == "kw" {
		var data []byte
		data, _, err = c.request(ctx, http.MethodGet, queryURL(endpoint, q), nil, nil)
		if err == nil {
			body, err = decodeJSON(normalizeKuwoJSON(data))
		}
	} else {
		body, err = c.json(ctx, http.MethodGet, queryURL(endpoint, q), nil, nil)
	}
	if err != nil {
		return nil, err
	}
	var items []any
	switch platform {
	case "wy":
		if err := apiCode(body, 200); err != nil {
			return nil, err
		}
		result := object(body["result"])
		if result == nil {
			return nil, errors.New("NetEase search returned no result object")
		}
		items = list(result["songs"])
	case "kw":
		if _, ok := body["TOTAL"]; !ok {
			return nil, errors.New("Kuwo search did not return TOTAL")
		}
		items = list(body["abslist"])
	case "kg":
		if number(body["status"]) != 1 || number(body["error_code"]) != 0 {
			return nil, fmt.Errorf("Kugou search failed: status %s, code %s", str(body["status"]), str(body["error_code"]))
		}
		if object(body["data"]) == nil {
			return nil, errors.New("Kugou search returned no data")
		}
		items = list(object(body["data"])["lists"])
	}
	songs := make([]model.Song, 0, len(items))
	for _, item := range items {
		var song model.Song
		switch platform {
		case "wy":
			song = netEaseSong(object(item))
		case "kw":
			song = kuwoSong(object(item))
		case "kg":
			song = kugouSong(object(item))
		}
		if song.SongID != "" && song.Name != "" {
			song.Normalize()
			songs = append(songs, song)
		}
	}
	return songs, nil
}

// Kuwo's public search endpoint returns JavaScript object strings delimited by
// single quotes. Convert delimiters while preserving escaped apostrophes and
// embedded quotes; a blanket quote replacement corrupts real song names.
func normalizeKuwoJSON(data []byte) []byte {
	var out bytes.Buffer
	for i := 0; i < len(data); i++ {
		quote := data[i]
		if quote != '\'' && quote != '"' {
			out.WriteByte(quote)
			continue
		}
		out.WriteByte('"')
		for i++; i < len(data); i++ {
			b := data[i]
			if b == quote {
				out.WriteByte('"')
				break
			}
			if b == '\\' && i+1 < len(data) {
				i++
				next := data[i]
				if quote == '\'' && next == '\'' {
					out.WriteByte('\'')
				} else {
					out.WriteByte('\\')
					out.WriteByte(next)
				}
				continue
			}
			if quote == '\'' && b == '"' {
				out.WriteString(`\"`)
			} else {
				out.WriteByte(b)
			}
		}
	}
	return out.Bytes()
}

func netEaseSong(item map[string]any) model.Song {
	id := str(item["id"])
	if !validID(id) {
		return model.Song{}
	}
	album := object(item["al"])
	if album == nil {
		album = object(item["album"])
	}
	artist := item["ar"]
	if artist == nil {
		artist = item["artists"]
	}
	song := model.Song{SongID: id, Songmid: id, Name: text(item["name"]), Singer: names(artist), Source: "wy", AlbumName: text(album["name"]), AlbumID: str(album["id"]), Interval: duration(number(first(item["dt"], item["duration"])) / 1000), PicURL: str(first(album["picUrl"], album["pic"], object(album["artist"])["img1v1Url"])), Extra: rawMetadata(item, nil)}
	if pc := object(item["pc"]); pc != nil {
		song.Name = text(first(pc["sn"], item["name"]))
		song.Singer = text(pc["ar"])
		song.AlbumName = text(pc["alb"])
	}
	song.Normalize()
	return song
}

func kuwoSong(item map[string]any) model.Song {
	id := strings.TrimPrefix(str(item["MUSICRID"]), "MUSIC_")
	if !validID(id) {
		return model.Song{}
	}
	pic := str(first(item["hts_MVPIC"], item["WEBPIC"], item["webpic"], item["pic"]))
	if pic == "" && str(item["web_albumpic_short"]) != "" {
		pic = "https://img4.kuwo.cn/wmvpic/" + str(item["web_albumpic_short"])
	}
	song := model.Song{SongID: id, Songmid: id, Name: text(item["SONGNAME"]), Singer: text(item["ARTIST"]), Source: "kw", AlbumName: text(item["ALBUM"]), AlbumID: str(item["ALBUMID"]), Interval: duration(number(item["DURATION"])), PicURL: pic, Extra: rawMetadata(item, nil)}
	song.Normalize()
	return song
}

var highlightTags = regexp.MustCompile(`(?i)</?em>`)

func kugouText(value any) string {
	return html.UnescapeString(highlightTags.ReplaceAllString(str(value), ""))
}
func kugouSong(item map[string]any) model.Song {
	hash := str(item["FileHash"])
	id := str(first(item["FileHash"], item["Audioid"], item["ID"]))
	if id == "" || id == "0" {
		return model.Song{}
	}
	alb := str(item["AlbumID"])
	if alb == "0" {
		alb = ""
	}
	song := model.Song{SongID: id, Songmid: id, Hash: hash, Name: kugouText(item["SongName"]), Singer: kugouText(item["SingerName"]), Source: "kg", AlbumName: kugouText(item["AlbumName"]), AlbumID: alb, Interval: duration(number(item["Duration"])), PicURL: strings.ReplaceAll(str(first(item["Image"], object(item["trans_param"])["union_cover"])), "{size}", "400"), Extra: rawMetadata(item, nil)}
	song.Normalize()
	return song
}

func songID(song model.Song) string { return str(first(song.SongID, song.Songmid)) }
func validLyric(lyric string) bool {
	switch strings.ToLower(strings.TrimSpace(lyric)) {
	case "", "null", "undefined", "[object object]":
		return false
	}
	return true
}
func (c *Client) Lyric(ctx context.Context, song model.Song) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if song.Source != "wy" && song.Source != "kw" {
		return "", fmt.Errorf("native lyric is unsupported for platform %q", song.Source)
	}
	id := songID(song)
	if !validID(id) {
		return "", errors.New("native lyric requires a numeric platform song ID")
	}
	var lyric string
	switch song.Source {
	case "wy":
		body, err := c.json(ctx, http.MethodGet, queryURL("https://music.163.com/api/song/media", url.Values{"id": {id}}), nil, nil)
		if err != nil {
			return "", err
		}
		if code, ok := body["code"]; ok && str(code) != "200" {
			return "", fmt.Errorf("NetEase lyric failed: %s", str(code))
		}
		lyric = str(first(body["lyric"], object(body["lrc"])["lyric"]))
	case "kw":
		body, err := c.json(ctx, http.MethodGet, queryURL("https://mobile.kuwo.cn/mpage/html5/songinfoandlrc", url.Values{"mid": {id}, "flag": {"0"}}), nil, nil)
		if err != nil {
			return c.kuwoBinaryLyric(ctx, id)
		}
		items := list(body["lrclist"])
		if items == nil {
			items = list(object(body["data"])["lrclist"])
		}
		var b strings.Builder
		for _, value := range items {
			item := object(value)
			line := text(item["lineinfo"])
			if !validLyric(line) {
				continue
			}
			rawTime := str(item["time"])
			stamp := rawTime
			if !strings.Contains(stamp, ":") {
				seconds, err := strconv.ParseFloat(stamp, 64)
				if err != nil || seconds < 0 {
					continue
				}
				hundredths := int(seconds*100 + 0.5)
				stamp = fmt.Sprintf("%02d:%02d.%02d", hundredths/6000, (hundredths/100)%60, hundredths%100)
			}
			fmt.Fprintf(&b, "[%s]%s\n", stamp, line)
		}
		lyric = b.String()
		if !validLyric(lyric) {
			return c.kuwoBinaryLyric(ctx, id)
		}
	default:
		return "", fmt.Errorf("native lyric is unsupported for platform %q", song.Source)
	}
	if !validLyric(lyric) {
		return "", errors.New("platform returned no usable lyrics")
	}
	return lyric, nil
}

var lyricWordTimes = regexp.MustCompile(`<[-\d]+,[-\d]+(?:,[-\d]+)?>`)
var lyricTimestamp = regexp.MustCompile(`\[\d{1,3}:\d{2}(?:\.\d{1,3})?\]`)

func (c *Client) kuwoBinaryLyric(ctx context.Context, id string) (string, error) {
	data, _, err := c.request(ctx, http.MethodGet, queryURL("https://mlyric.kuwo.cn/mobi.s", url.Values{"f": {"web"}, "type": {"lyric"}, "lrcx": {"1"}, "rid": {id}, "encode": {"utf8"}}), nil, nil)
	if err != nil {
		return "", err
	}
	separator := bytes.Index(data, []byte("\r\n\r\n"))
	if !bytes.HasPrefix(bytes.ToLower(data), []byte("tp=content")) || separator < 0 {
		return "", errors.New("Kuwo returned no usable lyric payload")
	}
	reader, err := zlib.NewReader(bytes.NewReader(data[separator+4:]))
	if err != nil {
		return "", fmt.Errorf("invalid Kuwo compressed lyric: %w", err)
	}
	defer reader.Close()
	const maxLyricBytes = 1 << 20
	inflated, err := io.ReadAll(io.LimitReader(reader, maxLyricBytes+1))
	if err != nil {
		return "", err
	}
	if len(inflated) > maxLyricBytes {
		return "", errors.New("Kuwo lyric exceeds decompression limit")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(inflated)))
	if err != nil {
		return "", fmt.Errorf("invalid Kuwo lyric encoding: %w", err)
	}
	key := []byte("yeelion")
	for i := range decoded {
		decoded[i] ^= key[i%len(key)]
	}
	lyric := html.UnescapeString(lyricWordTimes.ReplaceAllString(string(decoded), ""))
	if !utf8.ValidString(lyric) || !validLyric(lyric) || !lyricTimestamp.MatchString(lyric) {
		return "", errors.New("Kuwo returned no usable timestamped lyrics")
	}
	return lyric, nil
}

func imageURL(raw string) bool { u, err := url.Parse(raw); return err == nil && safeHTTPURL(u) }
func (c *Client) Pic(ctx context.Context, song model.Song) (string, error) {
	if imageURL(song.PicURL) {
		return song.PicURL, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	id := songID(song)
	var pic string
	switch song.Source {
	case "wy":
		if !validID(id) {
			return "", errors.New("invalid NetEase song ID")
		}
		body, err := c.json(ctx, http.MethodGet, queryURL("https://music.163.com/api/song/detail/", url.Values{"id": {id}, "ids": {"[" + id + "]"}}), nil, nil)
		if err != nil {
			return "", err
		}
		if err := apiCode(body, 200); err != nil {
			return "", err
		}
		if songs := list(body["songs"]); len(songs) > 0 {
			pic = netEaseSong(object(songs[0])).PicURL
		}
	case "kw":
		if !validID(id) {
			return "", errors.New("invalid Kuwo song ID")
		}
		data, _, err := c.request(ctx, http.MethodGet, queryURL("https://artistpic.kuwo.cn/pic.web", url.Values{"type": {"ect_music"}, "rid": {id}, "size": {"500"}}), nil, nil)
		if err != nil {
			return "", err
		}
		pic = strings.TrimSpace(string(data))
	case "tx":
		if song.AlbumID != "" {
			pic = "https://y.gtimg.cn/music/photo_new/T002R500x500M000" + url.PathEscape(song.AlbumID) + ".jpg"
		}
	default:
		return "", fmt.Errorf("native picture lookup is unsupported for platform %q", song.Source)
	}
	if !imageURL(pic) {
		return "", errors.New("platform returned no usable picture")
	}
	return pic, nil
}
