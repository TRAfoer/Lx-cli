package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lxcli/internal/model"
)

// SearchPlaylists searches public playlists using native platform catalog APIs.
// All fetches one page per source in wy/tx order; Limit applies to each source.
func (c *Client) SearchPlaylists(ctx context.Context, platform, keyword string, page, limit int) (model.PlaylistSearchResult, error) {
	if platform == "" {
		platform = "all"
	}
	if platform != "all" && platform != "wy" && platform != "tx" {
		return model.PlaylistSearchResult{}, fmt.Errorf("playlist search is unsupported for platform %q; use all, wy or tx", platform)
	}
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return model.PlaylistSearchResult{}, errors.New("playlist search keyword is empty")
	}
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		return model.PlaylistSearchResult{}, errors.New("playlist search page is too large (maximum 1000000)")
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	result := model.PlaylistSearchResult{Playlists: []model.RemotePlaylist{}, Page: page, Limit: limit}
	sources := []string{platform}
	if platform == "all" {
		sources = []string{"wy", "tx"}
	}
	succeeded := 0
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return model.PlaylistSearchResult{}, err
		}
		part, err := c.searchPlaylistPage(ctx, source, keyword, page, limit)
		if ctx.Err() != nil {
			return model.PlaylistSearchResult{}, ctx.Err()
		}
		if err != nil {
			if platform != "all" {
				return model.PlaylistSearchResult{}, fmt.Errorf("%s playlist search: %w", source, err)
			}
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s playlist search: %v", source, err))
			continue
		}
		succeeded++
		result.Playlists = append(result.Playlists, part.Playlists...)
		result.Total += part.Total
		result.HasMore = result.HasMore || part.HasMore
	}
	if succeeded == 0 {
		return model.PlaylistSearchResult{}, fmt.Errorf("all playlist searches failed: %s", strings.Join(result.Warnings, "; "))
	}
	return result, nil
}

func (c *Client) searchPlaylistPage(ctx context.Context, source, keyword string, page, limit int) (model.PlaylistSearchResult, error) {
	var endpoint string
	var query url.Values
	var headers map[string]string
	if source == "wy" {
		endpoint = "https://music.163.com/api/search/get/web"
		query = url.Values{"s": {keyword}, "type": {"1000"}, "offset": {strconv.Itoa((page - 1) * limit)}, "limit": {strconv.Itoa(limit)}}
		headers = map[string]string{"Referer": "https://music.163.com/", "Origin": "https://music.163.com"}
	} else {
		endpoint = "https://c.y.qq.com/soso/fcgi-bin/client_music_search_songlist"
		query = url.Values{"query": {keyword}, "page_no": {strconv.Itoa(page - 1)}, "num_per_page": {strconv.Itoa(limit)}, "format": {"json"}, "remoteplace": {"txt.yqq.playlist"}, "inCharset": {"utf8"}, "outCharset": {"utf-8"}}
		headers = map[string]string{"Referer": "http://y.qq.com/portal/search.html", "User-Agent": "Mozilla/5.0 (compatible; MSIE 9.0; Windows NT 6.1; WOW64; Trident/5.0)"}
	}
	body, err := c.json(ctx, http.MethodGet, queryURL(endpoint, query), nil, headers)
	if err != nil {
		return model.PlaylistSearchResult{}, err
	}
	success, container, itemsKey, totalKey := 200, "result", "playlists", "playlistCount"
	if source == "tx" {
		success, container, itemsKey, totalKey = 0, "data", "list", "sum"
	}
	if err := apiCode(body, success); err != nil {
		return model.PlaylistSearchResult{}, err
	}
	if source == "tx" {
		if code, ok := body["subcode"]; ok && str(code) != "0" {
			return model.PlaylistSearchResult{}, fmt.Errorf("QQ playlist search rejected request (subcode %s)", str(code))
		}
	}
	data := object(body[container])
	if data == nil {
		return model.PlaylistSearchResult{}, fmt.Errorf("playlist search returned no %s object", container)
	}
	total, err := playlistSearchCount(data[totalKey])
	if err != nil {
		return model.PlaylistSearchResult{}, fmt.Errorf("playlist search returned invalid %s", totalKey)
	}
	items, ok := data[itemsKey].([]any)
	if !ok {
		// NetEase omits the playlists property for a successful zero-hit query.
		// A present null/scalar property remains a malformed response.
		_, present := data[itemsKey]
		if source != "wy" || total != 0 || present {
			return model.PlaylistSearchResult{}, fmt.Errorf("playlist search returned no %s array", itemsKey)
		}
		items = []any{}
	}
	result := model.PlaylistSearchResult{Playlists: make([]model.RemotePlaylist, 0, len(items)), Total: total, Page: page, Limit: limit, HasMore: int64(page)*int64(limit) < int64(total)}
	for _, value := range items {
		item := object(value)
		p := model.RemotePlaylist{Source: source}
		if source == "wy" {
			p.ID, p.Name = str(item["id"]), text(item["name"])
			p.Description = playlistDescription(item["description"])
			p.Author, p.PicURL = text(object(item["creator"])["nickname"]), text(item["coverImgUrl"])
			p.Count = number(item["trackCount"])
			p.PlayCount, _ = strconv.ParseInt(str(item["playCount"]), 10, 64)
			p.Link = "https://music.163.com/playlist?id=" + p.ID
		} else {
			p.ID, p.Name = str(item["dissid"]), text(item["dissname"])
			p.Description = playlistDescription(text(item["introduction"]))
			p.Author, p.PicURL = text(object(item["creator"])["name"]), text(item["imgurl"])
			p.Count = number(item["song_count"])
			p.PlayCount, _ = strconv.ParseInt(str(item["listennum"]), 10, 64)
			p.Link = "https://y.qq.com/n/ryqq/playlist/" + p.ID
		}
		if !validID(p.ID) || strings.TrimSpace(p.Name) == "" {
			return model.PlaylistSearchResult{}, errors.New("playlist search returned an item without a valid ID or title")
		}
		result.Playlists = append(result.Playlists, p)
	}
	return result, nil
}

func playlistSearchCount(value any) (int, error) {
	n, err := strconv.ParseInt(str(value), 10, 32)
	if err != nil || n < 0 {
		return 0, errors.New("invalid playlist search count")
	}
	return int(n), nil
}
