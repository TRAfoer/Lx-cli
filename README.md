# Lx-Cli

面向洛雪音乐 JS 音源协议的 Go 命令行工具。支持搜索、mpv 播放、音源管理、本地歌单、网易/QQ 歌单导入、歌词和下载；输出 JSON 或普通文本。

## 构建与使用

需要 Go 1.23+；播放需要 mpv 和可用音源。

```powershell
.\build.cmd
.\bin\Lx-Cli.exe --help
.\bin\Lx-Cli.exe source add .\source.js
.\bin\Lx-Cli.exe search '晴天' --source wy --json
```

跨平台构建：`go build -o bin/Lx-Cli ./cmd/lx-cli`。

配置兼容原 `alx-go` 用户目录及 `ALX_HOME`、`ALX_CONFIG`、`ALX_DATA`、`ALX_CACHE`。可用 `ALX_MPV` 指定 mpv。`bin/`、运行数据和编译产物不纳入 Git。

## 验证

```powershell
go test ./...
go vet ./...
```

移植部分遵守 MIT 许可，见 `LICENSE` 及相关源码目录中的版权声明。
