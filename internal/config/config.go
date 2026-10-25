package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type SourceSettings struct {
	DefaultSource    string   `toml:"default_source" json:"default_source"`
	DefaultQuality   string   `toml:"default_quality" json:"default_quality"`
	QualityFallback  []string `toml:"quality_fallback" json:"quality_fallback"`
	JSPriority       bool     `toml:"js_priority" json:"js_priority"`
	Priority         []string `toml:"priority" json:"priority"`
	PlatformPriority []string `toml:"platform_priority" json:"platform_priority"`
}
type PlayerSettings struct {
	DefaultVolume int      `toml:"default_volume" json:"default_volume"`
	Repeat        string   `toml:"repeat" json:"repeat"`
	Shuffle       bool     `toml:"shuffle" json:"shuffle"`
	MPVArgs       []string `toml:"mpv_args" json:"mpv_args"`
	MPVSocket     string   `toml:"mpv_socket" json:"mpv_socket"`
	EnableMPRIS   bool     `toml:"enable_mpris" json:"enable_mpris"`
}
type DownloadSettings struct {
	OutputDir        string `toml:"output_dir" json:"output_dir"`
	FilenameTemplate string `toml:"filename_template" json:"filename_template"`
	MaxConcurrent    int    `toml:"max_concurrent" json:"max_concurrent"`
	SkipExisting     bool   `toml:"skip_existing" json:"skip_existing"`
	EmbedLyrics      bool   `toml:"embed_lyrics" json:"embed_lyrics"`
	SaveLyricsFile   bool   `toml:"save_lyrics_file" json:"save_lyrics_file"`
	Timeout          int    `toml:"timeout" json:"timeout"`
}
type Config struct {
	Source     SourceSettings   `toml:"source" json:"source"`
	Player     PlayerSettings   `toml:"player" json:"player"`
	Download   DownloadSettings `toml:"download" json:"download"`
	Path       string           `toml:"-" json:"-"`
	DataDir    string           `toml:"-" json:"-"`
	CacheDir   string           `toml:"-" json:"-"`
	SourcesDir string           `toml:"-" json:"-"`
	DBPath     string           `toml:"-" json:"-"`
}

func Load(path string) (*Config, error) {
	home, _ := os.UserHomeDir()
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	base = filepath.Join(base, "alx-go")
	if v := os.Getenv("ALX_HOME"); v != "" {
		base = Expand(v)
	}
	if path == "" {
		path = os.Getenv("ALX_CONFIG")
	}
	if path == "" {
		path = filepath.Join(base, "config.toml")
	}
	path = Expand(path)
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data := filepath.Join(base, "data")
	if v := os.Getenv("ALX_DATA"); v != "" {
		data = Expand(v)
	}
	cache := filepath.Join(base, "cache")
	if v := os.Getenv("ALX_CACHE"); v != "" {
		cache = Expand(v)
	}
	c := &Config{
		Source:   SourceSettings{DefaultSource: "all", DefaultQuality: "320k", QualityFallback: []string{"320k", "128k", "flac"}, JSPriority: true, Priority: []string{}, PlatformPriority: []string{"wy", "kw", "kg", "tx", "mg"}},
		Player:   PlayerSettings{DefaultVolume: 80, Repeat: "off", MPVArgs: []string{}},
		Download: DownloadSettings{OutputDir: filepath.Join(home, "Music", "alx-go"), FilenameTemplate: "{singer} - {title}", MaxConcurrent: 3, SkipExisting: true, SaveLyricsFile: true, Timeout: 90},
	}
	b, err := os.ReadFile(path)
	isNew := os.IsNotExist(err)
	if err != nil && !isNew {
		return nil, err
	}
	if !isNew {
		if err = toml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("配置 TOML 无效: %w", err)
		}
	}
	c.Path = path
	c.DataDir = data
	c.CacheDir = cache
	c.SourcesDir = filepath.Join(data, "sources")
	c.DBPath = filepath.Join(data, "alx-go.db")
	for _, p := range []string{data, cache, c.SourcesDir} {
		if err = os.MkdirAll(p, 0o700); err != nil {
			return nil, err
		}
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	if isNew {
		err = c.Save()
	}
	return c, err
}

func Expand(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimLeft(strings.TrimPrefix(path, "~"), "/\\"))
		}
	}
	return os.ExpandEnv(path)
}

func (c *Config) Validate() error {
	if c.Player.DefaultVolume < 0 || c.Player.DefaultVolume > 100 {
		return fmt.Errorf("音量应为 0–100")
	}
	if !oneOf(c.Player.Repeat, "off", "one", "all") {
		return fmt.Errorf("循环模式应为 off/one/all")
	}
	if !oneOf(c.Source.DefaultQuality, "128k", "192k", "320k", "flac", "flac24bit", "ape", "wav") {
		return fmt.Errorf("不支持的音质 %q", c.Source.DefaultQuality)
	}
	if c.Download.MaxConcurrent < 1 || c.Download.MaxConcurrent > 16 {
		return fmt.Errorf("下载并发数应为 1–16")
	}
	if c.Download.Timeout < 1 {
		return fmt.Errorf("下载超时应大于 0")
	}
	return nil
}
func oneOf(v string, values ...string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}
func (c *Config) Save() error {
	b, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return AtomicWrite(c.Path, b, 0o600)
}
func AtomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".alx-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}
func (c *Config) Map() map[string]any {
	b, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
func (c *Config) Get(key string) (any, error) {
	var v any = c.Map()
	for _, k := range strings.Split(key, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("未知配置键 %q", key)
		}
		v, ok = m[k]
		if !ok {
			return nil, fmt.Errorf("未知配置键 %q", key)
		}
	}
	return v, nil
}
func (c *Config) Set(key, value string) error {
	old, err := c.Get(key)
	if err != nil {
		return err
	}
	m := c.Map()
	parts := strings.Split(key, ".")
	target := m
	for _, p := range parts[:len(parts)-1] {
		target = target[p].(map[string]any)
	}
	var v any
	switch old.(type) {
	case bool:
		v, err = strconv.ParseBool(value)
	case float64:
		v, err = strconv.Atoi(value)
	case []any:
		err = json.Unmarshal([]byte(value), &v)
	default:
		v = value
	}
	if err != nil {
		return fmt.Errorf("配置值无效: %w", err)
	}
	target[parts[len(parts)-1]] = v
	b, _ := json.Marshal(m)
	next := *c
	if err = json.Unmarshal(b, &next); err != nil {
		return err
	}
	if err = next.Validate(); err != nil {
		return err
	}
	if err = next.Save(); err != nil {
		return err
	}
	*c = next
	return nil
}
