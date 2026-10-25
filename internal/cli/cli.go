package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"lxcli/internal/config"
	"lxcli/internal/engine"
	"lxcli/internal/model"
)

const Version = "0.1.1-go"
const help = `用法: Lx-Cli [--config FILE] <命令> [--json] [--no-color]

  config show/get/set/path    配置管理
  source add/list/info/remove/enable/disable/update  JS 音源
  search <关键词> [--source all|wy|kw|kg|tx|mg] [--page N] [--limit N]
  play <ID|URL|文件>... [--quality 320k] [--from-playlist NAME]
  state / now / pause / resume / stop / next / prev / quit
  seek <秒|+秒|mm:ss|百分比> / volume [0..100|+N|-N]
  repeat [off|one|all] / shuffle [on|off]
  queue show/add/insert/remove/clear/move / queue play <位置（从1开始）>
  playlist list/show/create/delete/rename/add/remove/play
  playlist import <歌单链接|分享文字|JSON/M3U文件> [--name 名称]
  playlist export <名称> [--format json|m3u] [--output 文件]
  fav [add/remove/play] / history [--limit N]
  lyric <ID> [--save] / pic <ID>
  download add <ID>... [--quality 320k] / download list

歌曲ID可使用完整 cli_id、唯一前缀或 平台:原始ID。
歌单链接支持网易云与 QQ 音乐的公开歌单。播放需要 mpv 和可用 JS 音源。
默认输出 JSON；config path 与 search --id-only 输出纯文本。
--json、--no-color 为兼容选项；--help 显示帮助，--version 显示版本。
`

type plainOutput string

type options struct {
	values map[string]string
	flags  map[string]bool
	args   []string
}

func parse(args []string) (options, error) {
	o := options{values: map[string]string{}, flags: map[string]bool{}}
	valued := map[string]bool{"config": true, "source": true, "page": true, "limit": true, "quality": true, "name": true, "output": true, "format": true, "from-playlist": true, "track": true}
	bools := map[string]bool{"json": true, "quiet": true, "no-color": true, "verbose": true, "debug": true, "shuffle": true, "yes": true, "save": true, "translated": true, "romanized": true, "id-only": true, "help": true, "version": true}
	aliases := map[string]string{"c": "config", "s": "source", "p": "page", "l": "limit", "o": "output", "h": "help", "V": "version", "v": "verbose"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.args = append(o.args, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' && (a[1] == '-' || a[1] < '0' || a[1] > '9') {
			key := strings.TrimLeft(a, "-")
			key, val, has := strings.Cut(key, "=")
			if k, ok := aliases[key]; ok {
				key = k
			}
			if key == "q" {
				key = "quality"
			}
			if valued[key] {
				if !has {
					i++
					if i >= len(args) {
						return o, fmt.Errorf("--%s 缺少值", key)
					}
					val = args[i]
				}
				o.values[key] = val
			} else if bools[key] {
				if has {
					return o, fmt.Errorf("--%s 不接受值", key)
				}
				o.flags[key] = true
			} else {
				return o, fmt.Errorf("未知选项 --%s", key)
			}
		} else {
			o.args = append(o.args, a)
		}
	}
	return o, nil
}
func Run(ctx context.Context, args []string, out, errout io.Writer) int {
	o, err := parse(args)
	if err != nil {
		writeError(out, err)
		return 1
	}
	if o.flags["version"] {
		fmt.Fprintln(out, "Lx-Cli "+Version)
		return 0
	}
	if o.flags["help"] || len(o.args) == 0 {
		fmt.Fprint(out, help)
		return 0
	}
	e, err := engine.Open(o.values["config"])
	if err != nil {
		writeError(out, err)
		return 1
	}
	defer e.Close()
	if o.args[0] == "_serve" {
		if err = e.Serve(ctx); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		return 0
	}
	result, plain, err := execute(ctx, e, o)
	if err != nil {
		writeError(out, err)
		return 1
	}
	if text, ok := result.(plainOutput); ok {
		if text != "" {
			fmt.Fprintln(out, text)
		}
		return 0
	}
	if plain != "" {
		fmt.Fprintln(out, plain)
		return 0
	}
	if result == nil {
		result = map[string]any{"status": "ok"}
	}
	if err = json.NewEncoder(out).Encode(result); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}
func writeError(out io.Writer, err error) {
	_ = json.NewEncoder(out).Encode(map[string]string{"error": err.Error()})
}
func need(args []string, n int) error {
	if len(args) < n {
		return errors.New("参数不足，请运行 --help 查看用法")
	}
	return nil
}
func number(value string, def, min, max int) (int, error) {
	if value == "" {
		return def, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("数值 %q 应在 %d–%d 范围", value, min, max)
	}
	return n, nil
}
func resolve(e *engine.Engine, ids []string, allowDirect bool) ([]model.Song, error) {
	out := []model.Song{}
	for _, id := range ids {
		song, err := e.Store.Song(id)
		if err != nil && allowDirect {
			if strings.HasPrefix(id, "https://") || strings.HasPrefix(id, "http://") {
				song = model.Song{Source: "url", SongID: id, Name: id}
				song.Normalize()
				err = nil
			} else if st, statErr := os.Stat(config.Expand(id)); statErr == nil && !st.IsDir() {
				path, pathErr := filepath.Abs(config.Expand(id))
				if pathErr != nil {
					return nil, pathErr
				}
				song = model.Song{Source: "local", SongID: path, Name: filepath.Base(path)}
				song.Normalize()
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
		out = append(out, song)
	}
	if err := e.Store.CacheSongs(out); err != nil {
		return nil, err
	}
	return out, nil
}
func rpc(ctx context.Context, e *engine.Engine, r engine.RPCRequest, start bool) (any, error) {
	b, err := e.Call(ctx, r, start)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
func execute(ctx context.Context, e *engine.Engine, o options) (any, string, error) {
	args := o.args
	cmd := args[0]
	args = args[1:]
	aliases := map[string]string{"sch": "search", "q": "queue", "pl": "playlist", "favorites": "fav", "hist": "history", "lrc": "lyric", "lyrics": "lyric", "lyr": "lyric", "cover": "pic", "dl": "download"}
	if v := aliases[cmd]; v != "" {
		cmd = v
	}
	switch cmd {
	case "config":
		sub := "show"
		if len(args) > 0 {
			sub = args[0]
			args = args[1:]
		}
		switch sub {
		case "show":
			return e.Config, "", nil
		case "path":
			return nil, e.Config.Path, nil
		case "get":
			if err := need(args, 1); err != nil {
				return nil, "", err
			}
			v, err := e.Config.Get(args[0])
			return v, "", err
		case "set":
			if err := need(args, 2); err != nil {
				return nil, "", err
			}
			return nil, "", e.Config.Set(args[0], args[1])
		default:
			return nil, "", fmt.Errorf("未知 config 操作 %q", sub)
		}
	case "search":
		if err := need(args, 1); err != nil {
			return nil, "", err
		}
		page, err := number(o.values["page"], 1, 1, 100000)
		if err != nil {
			return nil, "", err
		}
		limit, err := number(o.values["limit"], 30, 1, 100)
		if err != nil {
			return nil, "", err
		}
		songs, err := e.Search(ctx, strings.Join(args, " "), o.values["source"], page, limit)
		if err != nil {
			return nil, "", err
		}
		if o.flags["id-only"] {
			ids := []string{}
			for _, s := range songs {
				ids = append(ids, s.CLIID)
			}
			return plainOutput(strings.Join(ids, "\n")), "", nil
		}
		return songs, "", nil
	case "source":
		v, err := sourceCommand(ctx, e, args, o)
		return v, "", err
	case "playlist":
		v, err := playlistCommand(ctx, e, args, o)
		return v, "", err
	case "play":
		var songs []model.Song
		var err error
		if name := o.values["from-playlist"]; name != "" {
			songs, err = e.Store.PlaylistSongs(name)
		} else {
			if err = need(args, 1); err != nil {
				return nil, "", err
			}
			songs, err = resolve(e, args, true)
		}
		if err != nil {
			return nil, "", err
		}
		if o.flags["shuffle"] {
			_, err = rpc(ctx, e, engine.RPCRequest{Op: "shuffle", Value: "on"}, true)
			if err != nil {
				return nil, "", err
			}
		}
		quality := o.values["quality"]
		if quality == "" {
			quality = e.Config.Source.DefaultQuality
		}
		v, err := rpc(ctx, e, engine.RPCRequest{Op: "play", Songs: songs, Value: quality}, true)
		return v, "", err
	case "queue":
		v, err := queueCommand(ctx, e, args)
		return v, "", err
	case "state", "now", "pause", "resume", "stop", "quit", "next", "prev", "volume", "seek", "repeat", "shuffle":
		op := cmd
		if op == "now" {
			op = "state"
		}
		value := ""
		if len(args) > 0 {
			value = args[0]
		}
		if cmd == "seek" && value == "" {
			return nil, "", errors.New("seek 缺少时间")
		}
		start := cmd == "repeat" || cmd == "shuffle" || cmd == "volume"
		v, err := rpc(ctx, e, engine.RPCRequest{Op: op, Value: value}, start)
		return v, "", err
	case "lyric":
		if err := need(args, 1); err != nil {
			return nil, "", err
		}
		song, err := e.Store.Song(args[0])
		if err != nil {
			return nil, "", err
		}
		lyric, err := e.Lyric(ctx, song)
		if err != nil {
			return nil, "", err
		}
		if o.flags["save"] {
			path := filepath.Join(config.Expand(e.Config.Download.OutputDir), safeName(song.Name)+".lrc")
			if err = config.AtomicWrite(path, []byte(lyric), 0o644); err != nil {
				return nil, "", err
			}
		}
		return map[string]any{"cli_id": song.CLIID, "song_id": song.SongID, "name": song.Name, "singer": song.Singer, "lyric": lyric, "track": "main"}, "", nil
	case "pic":
		if err := need(args, 1); err != nil {
			return nil, "", err
		}
		song, err := e.Store.Song(args[0])
		if err != nil {
			return nil, "", err
		}
		pic := song.PicURL
		if pic == "" {
			pic, err = e.Catalog.Pic(ctx, song)
		}
		if err != nil {
			return nil, "", err
		}
		if o.flags["save"] {
			return nil, "", errors.New("封面保存尚未实现；pic 返回封面链接")
		}
		return map[string]any{"url": pic, "cli_id": song.CLIID}, "", nil
	case "download":
		v, err := downloadCommand(ctx, e, args, o)
		return v, "", err
	case "history":
		limit, err := number(o.values["limit"], 30, 1, 10000)
		if err != nil {
			return nil, "", err
		}
		v, err := e.Store.History(limit)
		return v, "", err
	case "fav":
		sub := "list"
		if len(args) > 0 {
			sub = args[0]
			args = args[1:]
		}
		switch sub {
		case "list":
			v, err := e.Store.PlaylistSongs("Favorites")
			return v, "", err
		case "play":
			v, err := playlistCommand(ctx, e, []string{"play", "Favorites"}, o)
			return v, "", err
		case "add":
			if len(args) == 0 {
				raw, err := e.Call(ctx, engine.RPCRequest{Op: "state"}, false)
				if err != nil {
					return nil, "", err
				}
				var st model.State
				_ = json.Unmarshal(raw, &st)
				if st.CurrentSong == nil {
					return nil, "", errors.New("当前没有播放歌曲")
				}
				args = []string{st.CurrentSong.CLIID}
			}
			v, err := playlistCommand(ctx, e, append([]string{"add", "Favorites"}, args...), o)
			return v, "", err
		case "remove":
			v, err := playlistCommand(ctx, e, append([]string{"remove", "Favorites"}, args...), o)
			return v, "", err
		}
		return nil, "", errors.New("未知 fav 操作")
	default:
		return nil, "", fmt.Errorf("命令 %q 尚未实现；请运行 --help 查看本版支持的命令", cmd)
	}
}

func sourceCommand(ctx context.Context, e *engine.Engine, args []string, o options) (any, error) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub := args[0]
	args = args[1:]
	if sub == "list" {
		return e.Store.Sources()
	}
	if err := need(args, 1); err != nil {
		return nil, err
	}
	if sub == "add" {
		return e.AddSource(ctx, args[0], o.values["name"])
	}
	all, err := e.Store.Sources()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.ID != args[0] {
			continue
		}
		switch sub {
		case "info":
			return s, nil
		case "remove":
			if err = e.Store.DeleteSource(s.ID); err != nil {
				return nil, err
			}
			abs, _ := filepath.Abs(s.ScriptPath)
			dir, _ := filepath.Abs(e.Config.SourcesDir)
			if filepath.Dir(abs) == dir {
				_ = os.Remove(abs)
			}
			return nil, nil
		case "enable", "disable":
			s.Enabled = sub == "enable"
			return nil, e.Store.PutSource(s)
		case "update":
			if s.SourceURL == "" {
				return nil, errors.New("该音源没有更新 URL")
			}
			return e.AddSource(ctx, s.SourceURL, s.Name)
		default:
			return nil, fmt.Errorf("未知 source 操作 %q", sub)
		}
	}
	return nil, errors.New("音源不存在")
}
func queueCommand(ctx context.Context, e *engine.Engine, args []string) (any, error) {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "show", "list":
		return rpc(ctx, e, engine.RPCRequest{Op: "queue"}, false)
	case "add", "insert":
		if err := need(args, 1); err != nil {
			return nil, err
		}
		songs, err := resolve(e, args, true)
		if err != nil {
			return nil, err
		}
		return rpc(ctx, e, engine.RPCRequest{Op: "queue_" + sub, Songs: songs}, true)
	case "clear":
		return rpc(ctx, e, engine.RPCRequest{Op: "queue_clear"}, true)
	case "remove", "move", "play":
		if err := need(args, 1); err != nil {
			return nil, err
		}
		i, err := number(args[0], 0, 1, 10000000)
		if err != nil {
			return nil, err
		}
		r := engine.RPCRequest{Op: "queue_" + sub, Index: i - 1}
		if sub == "move" {
			if err = need(args, 2); err != nil {
				return nil, err
			}
			n, err := number(args[1], 0, 1, 10000000)
			if err != nil {
				return nil, err
			}
			r.To = n - 1
		}
		return rpc(ctx, e, r, true)
	default:
		return nil, fmt.Errorf("未知 queue 操作 %q", sub)
	}
}
func downloadCommand(ctx context.Context, e *engine.Engine, args []string, o options) (any, error) {
	if len(args) == 0 {
		return e.Store.Downloads()
	}
	sub := args[0]
	args = args[1:]
	if sub == "list" || sub == "status" || sub == "history" {
		return e.Store.Downloads()
	}
	if sub != "add" {
		args = append([]string{sub}, args...)
	}
	if err := need(args, 1); err != nil {
		return nil, err
	}
	songs, err := resolve(e, args, false)
	if err != nil {
		return nil, err
	}
	quality := o.values["quality"]
	if quality == "" {
		quality = e.Config.Source.DefaultQuality
	}
	names := []string{}
	for _, s := range songs {
		_, err = e.Store.NewDownload(model.DownloadTask{SongID: s.SongID, Source: s.Source, Name: s.Name, Singer: s.Singer, Quality: quality})
		if err != nil {
			return nil, err
		}
		names = append(names, s.Name)
	}
	if _, err = e.Call(ctx, engine.RPCRequest{Op: "download_wake"}, true); err != nil {
		return nil, err
	}
	return map[string]any{"songs": names, "status": "queued"}, nil
}
