# Lx-Cli

面向洛雪音乐 JS 音源协议的 Go 命令行工具。支持歌曲及在线歌单搜索、mpv 播放、音源管理、本地歌单、网易/QQ 歌单导入、歌词和下载；输出 JSON 或普通文本。

## 构建与使用

需要 Go 1.23+；播放需要 mpv 和可用音源。

```powershell
.\build.cmd
.\bin\Lx-Cli.exe --help
.\bin\Lx-Cli.exe source add .\source.js
.\bin\Lx-Cli.exe search '晴天' --source wy --json
.\bin\Lx-Cli.exe queue play 3 --json
```

`queue play <位置>` 按从 1 开始的位置播放现有队列中的歌曲，保留队列内容及顺序；重复歌曲按位置区分。

跨平台构建：`go build -o bin/Lx-Cli ./cmd/lx-cli`。

## 在线歌单

网易云和 QQ 的公开歌单可按关键词搜索，无需安装 JS 音源。播放仍需可用音源和 mpv。

```powershell
.\bin\Lx-Cli.exe playlist search '夜跑' --source all --page 1 --limit 20
.\bin\Lx-Cli.exe playlist inspect 'https://music.163.com/playlist?id=123'
.\bin\Lx-Cli.exe playlist import 'https://music.163.com/playlist?id=123' --name '夜跑收藏'
```

`playlist search` 返回 `playlists`、`total`、`page`、`limit`、`has_more`，每个歌单包含名称、作者、平台、曲目数及可供查看/导入的 `link`。`--source` 支持 `all`（默认）、`wy`、`tx`；`--page` 从 1 开始，`--limit` 默认 20、上限 100。`all` 对每个平台分别取一页、每页最多 `limit` 个歌单，按网易云、QQ 顺序合并；任一平台有后续结果时 `has_more` 为真。部分平台失败会保留其他结果并返回 `warnings`，全部失败则报错。

`playlist inspect` 查看远程歌单及歌曲，缓存歌曲 ID 以便后续播放；不会创建本地歌单。`playlist import` 才将歌单保存到本地；同名时使用 `--name` 指定新名称。

配置兼容原 `alx-go` 用户目录及 `ALX_HOME`、`ALX_CONFIG`、`ALX_DATA`、`ALX_CACHE`。可用 `ALX_MPV` 指定 mpv。`bin/`、运行数据和编译产物不纳入 Git。

## 验证

```powershell
go test ./...
go vet ./...
```

移植部分遵守 MIT 许可，见 `LICENSE` 及相关源码目录中的版权声明。
