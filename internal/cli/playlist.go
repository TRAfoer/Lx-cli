package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"lxcli/internal/config"
	"lxcli/internal/engine"
	"lxcli/internal/model"
)

func playlistCommand(ctx context.Context, e *engine.Engine, args []string, o options) (any, error) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub := args[0]
	args = args[1:]
	if sub == "list" {
		return e.Store.ListPlaylists()
	}
	if err := need(args, 1); err != nil {
		return nil, err
	}
	name := args[0]
	switch sub {
	case "create":
		desc := ""
		if len(args) > 1 {
			desc = args[1]
		}
		return nil, e.Store.CreatePlaylist(name, desc)
	case "show":
		return e.Store.PlaylistSongs(name)
	case "delete":
		return nil, e.Store.DeletePlaylist(name)
	case "rename":
		if err := need(args, 2); err != nil {
			return nil, err
		}
		return nil, e.Store.RenamePlaylist(name, args[1])
	case "add":
		if err := need(args, 2); err != nil {
			return nil, err
		}
		songs, err := resolve(e, args[1:], false)
		if err != nil {
			return nil, err
		}
		return nil, e.Store.AddToPlaylist(name, songs)
	case "remove":
		if err := need(args, 2); err != nil {
			return nil, err
		}
		return nil, e.Store.RemoveFromPlaylist(name, args[1])
	case "play":
		songs, err := e.Store.PlaylistSongs(name)
		if err != nil {
			return nil, err
		}
		if len(songs) == 0 {
			return nil, errors.New("歌单为空")
		}
		if o.flags["shuffle"] {
			if _, err = rpc(ctx, e, engine.RPCRequest{Op: "shuffle", Value: "on"}, true); err != nil {
				return nil, err
			}
		}
		return rpc(ctx, e, engine.RPCRequest{Op: "play", Songs: songs, Value: e.Config.Source.DefaultQuality}, true)
	case "import", "parse":
		var p model.ImportedPlaylist
		var err error
		if st, statErr := os.Stat(config.Expand(name)); statErr == nil && !st.IsDir() {
			p, err = readPlaylist(config.Expand(name))
		} else {
			p, err = e.Catalog.ImportPlaylist(ctx, strings.Join(args, " "))
		}
		if err != nil {
			return nil, err
		}
		if n := o.values["name"]; n != "" {
			p.Name = n
		}
		if p.Name == "" {
			p.Name = "Imported"
		}
		if err = e.Store.ImportPlaylist(p.Name, p.Description, p.Songs); err != nil {
			return nil, err
		}
		list, err := e.Store.PlaylistSongs(p.Name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": p.Name, "count": len(list), "source": p.Source, "total": p.Total, "warnings": p.Warnings}, nil
	case "export":
		songs, err := e.Store.PlaylistSongs(name)
		if err != nil {
			return nil, err
		}
		p := model.ImportedPlaylist{Name: name, Songs: songs, Total: len(songs)}
		format := o.values["format"]
		if format == "" {
			format = "json"
		}
		var b []byte
		switch format {
		case "json":
			b, err = json.MarshalIndent(p, "", "  ")
		case "m3u":
			var text strings.Builder
			text.WriteString("#EXTM3U\n")
			for _, s := range songs {
				if s.Source != "local" && s.Source != "url" {
					text.WriteString("#ALX:" + s.Source + ":" + s.SongID + "\n")
				}
				text.WriteString("#EXTINF:-1," + s.Singer + " - " + s.Name + "\n")
				if s.Source == "local" || s.Source == "url" {
					text.WriteString(s.SongID + "\n")
				} else {
					text.WriteString("alx://" + s.Source + "/" + s.SongID + "\n")
				}
			}
			b = []byte(text.String())
		default:
			return nil, errors.New("导出格式应为 json/m3u")
		}
		if err != nil {
			return nil, err
		}
		if path := o.values["output"]; path != "" {
			path = config.Expand(path)
			if st, err := os.Stat(path); err == nil && st.IsDir() {
				path = filepath.Join(path, safeName(name)+"."+format)
			}
			if err = config.AtomicWrite(path, b, 0o644); err != nil {
				return nil, err
			}
			return map[string]any{"path": path, "count": len(songs)}, nil
		}
		if format == "json" {
			return json.RawMessage(b), nil
		}
		return map[string]any{"format": "m3u", "content": string(b)}, nil
	default:
		return nil, fmt.Errorf("未知 playlist 操作 %q", sub)
	}
}
func safeName(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, v)
	v = strings.Trim(v, " .")
	if v == "" {
		v = "playlist"
	}
	return v
}
func readPlaylist(path string) (model.ImportedPlaylist, error) {
	var p model.ImportedPlaylist
	info, err := os.Stat(path)
	if err != nil {
		return p, err
	}
	if info.Size() > 16<<20 {
		return p, errors.New("歌单文件超过 16 MiB")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	p.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	p.Source = "file"
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		var obj model.ImportedPlaylist
		if err = json.Unmarshal(b, &obj); err == nil && obj.Songs != nil {
			if obj.Name != "" {
				p.Name = obj.Name
			}
			p.Description = obj.Description
			p.Songs = obj.Songs
		} else if err = json.Unmarshal(b, &p.Songs); err != nil {
			return p, fmt.Errorf("不支持的 JSON 歌单: %w", err)
		}
	} else {
		scanner := bufio.NewScanner(strings.NewReader(string(b)))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		title := ""
		artist := ""
		interval := ""
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "#EXTINF:") {
				meta, name, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
				title = name
				artist = ""
				if a, t, ok := strings.Cut(name, " - "); ok {
					artist = a
					title = t
				}
				if sec, parseErr := strconv.Atoi(meta); parseErr == nil && sec > 0 {
					interval = fmt.Sprintf("%02d:%02d", sec/60, sec%60)
				}
				continue
			}
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			s := model.Song{Name: title, Singer: artist, Interval: interval}
			if strings.HasPrefix(line, "alx://") {
				parts := strings.SplitN(strings.TrimPrefix(line, "alx://"), "/", 2)
				if len(parts) != 2 {
					return p, errors.New("无效歌单歌曲地址")
				}
				s.Source = parts[0]
				s.SongID = parts[1]
			} else if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
				s.Source = "url"
				s.SongID = line
			} else {
				s.Source = "local"
				if !filepath.IsAbs(line) {
					line = filepath.Join(filepath.Dir(path), line)
				}
				s.SongID = line
			}
			if s.Name == "" {
				s.Name = filepath.Base(s.SongID)
			}
			s.Normalize()
			p.Songs = append(p.Songs, s)
			title = ""
			artist = ""
			interval = ""
		}
		if err = scanner.Err(); err != nil {
			return p, err
		}
	}
	for i := range p.Songs {
		p.Songs[i].Normalize()
	}
	p.Total = len(p.Songs)
	if p.Total == 0 {
		return p, errors.New("歌单中没有歌曲")
	}
	return p, nil
}
