package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"lxcli/internal/model"
	"lxcli/internal/player"
)

type RPCRequest struct {
	Op    string       `json:"op"`
	Value string       `json:"value,omitempty"`
	Songs []model.Song `json:"songs,omitempty"`
	Index int          `json:"index,omitempty"`
	To    int          `json:"to,omitempty"`
}
type manager struct {
	e          *Engine
	mpv        playerAPI
	mu         sync.Mutex
	playMu     sync.Mutex
	q          model.Queue
	st         model.State
	generation int
	intent     int
}

type playerAPI interface {
	Ensure(context.Context) error
	Command(context.Context, ...any) (json.RawMessage, error)
	Running(context.Context) bool
	Close(context.Context) error
	Events() <-chan player.Event
}

func newManager(e *Engine) *manager {
	ep := e.Config.Player.MPVSocket
	if ep == "" {
		h := model.SongID("ipc", e.Config.CacheDir)
		if runtime.GOOS == "windows" {
			ep = `\\.\pipe\alx-go-` + h
		} else {
			ep = filepath.Join(e.Config.CacheDir, "mpv-"+h+".sock")
		}
	}
	m := &manager{e: e, mpv: player.New(ep, e.Config.Player.MPVArgs), q: model.Queue{Songs: []model.Song{}, CurrentIndex: -1}, st: model.State{Status: "stopped", Volume: e.Config.Player.DefaultVolume, Repeat: e.Config.Player.Repeat, Shuffle: e.Config.Player.Shuffle}}
	_ = e.Store.Get("queue", &m.q)
	if m.q.Songs == nil {
		m.q.Songs = []model.Song{}
	}
	if m.q.CurrentIndex >= len(m.q.Songs) {
		m.q.CurrentIndex = -1
	}
	return m
}
func (m *manager) saveLocked() error { return m.e.Store.Set("queue", m.q) }
func (m *manager) state(ctx context.Context) model.State {
	m.mu.Lock()
	st := m.st
	st.QueueLength = len(m.q.Songs)
	st.QueueIndex = m.q.CurrentIndex
	if m.q.CurrentIndex >= 0 && m.q.CurrentIndex < len(m.q.Songs) {
		song := m.q.Songs[m.q.CurrentIndex]
		st.CurrentSong = &song
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	st.DaemonActive = m.mpv.Running(ctx)
	if !st.DaemonActive {
		st.Status = "stopped"
		return st
	}
	get := func(name string, v any) bool {
		b, err := m.mpv.Command(ctx, "get_property", name)
		return err == nil && json.Unmarshal(b, v) == nil
	}
	var idle, paused bool
	get("idle-active", &idle)
	if idle {
		st.Status = "stopped"
	} else if get("pause", &paused) && paused {
		st.Status = "paused"
	} else {
		st.Status = "playing"
	}
	get("time-pos", &st.Position)
	get("duration", &st.Duration)
	var volume float64
	if get("volume", &volume) {
		st.Volume = int(volume)
	}
	return st
}
func (m *manager) start(ctx context.Context, quality string) error {
	m.mu.Lock()
	q := m.q
	q.Songs = append([]model.Song{}, q.Songs...)
	gen := m.generation
	intent := m.intent
	m.mu.Unlock()
	return m.loadQueue(ctx, q, quality, gen, intent)
}

// Resolve first; publish queue metadata only after mpv accepts the new file.
func (m *manager) loadQueue(ctx context.Context, q model.Queue, quality string, gen, intent int) error {
	m.playMu.Lock()
	defer m.playMu.Unlock()
	if q.CurrentIndex < 0 || q.CurrentIndex >= len(q.Songs) {
		return errors.New("播放队列为空")
	}
	song := q.Songs[q.CurrentIndex]
	url, err := m.e.Resolve(ctx, song, quality)
	if err != nil {
		return err
	}
	if err = m.mpv.Ensure(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if intent != m.intent {
		return errors.New("播放请求已被后续操作替代")
	}
	if gen != m.generation {
		// Queue additions/reordering preserve the pending target and their edits.
		idx := -1
		same := func(a, b model.Song) bool { return a.Source == b.Source && a.SongID == b.SongID }
		if q.CurrentIndex < len(m.q.Songs) && same(m.q.Songs[q.CurrentIndex], song) {
			idx = q.CurrentIndex
		} else {
			for i, v := range m.q.Songs {
				if same(v, song) {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			return errors.New("待播放歌曲已从队列移除")
		}
		q = model.Queue{Songs: append([]model.Song{}, m.q.Songs...), CurrentIndex: idx}
	}
	if _, err = m.mpv.Command(ctx, "loadfile", url, "replace"); err != nil {
		return err
	}
	_, _ = m.mpv.Command(ctx, "set_property", "volume", m.st.Volume)
	_, _ = m.mpv.Command(ctx, "set_property", "pause", false)
	m.q = q
	m.generation++
	m.st.Status = "playing"
	m.st.Position = 0
	m.st.Duration = 0
	_ = m.e.Store.AddHistory(song)
	return m.saveLocked()
}
func (m *manager) advance(ctx context.Context, direction int, automatic bool) error {
	m.mu.Lock()
	n := len(m.q.Songs)
	if n == 0 {
		m.mu.Unlock()
		return errors.New("播放队列为空")
	}
	idx := m.q.CurrentIndex
	if automatic && m.st.Repeat == "one" {
	} else if m.st.Shuffle && n > 1 {
		offset := 1 + rand.Intn(n-1)
		idx = (idx + offset) % n
	} else {
		idx += direction
	}
	if idx < 0 || idx >= n {
		if m.st.Repeat == "all" {
			idx = (idx + n) % n
		} else {
			m.st.Status = "stopped"
			m.mu.Unlock()
			if automatic {
				return nil
			}
			return errors.New("已到达播放队列边界")
		}
	}
	q := m.q
	q.Songs = append([]model.Song{}, q.Songs...)
	q.CurrentIndex = idx
	gen := m.generation
	if !automatic {
		m.intent++
	}
	intent := m.intent
	m.mu.Unlock()
	return m.loadQueue(ctx, q, "", gen, intent)
}
func (m *manager) events(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-m.mpv.Events():
			if !ok {
				return
			}
			if ev.Event == "end-file" && ev.Reason == "eof" {
				go func() {
					c, cancel := context.WithTimeout(ctx, 60*time.Second)
					defer cancel()
					if err := m.advance(c, 1, true); err != nil {
						m.mu.Lock()
						m.st.Status = "stopped"
						m.mu.Unlock()
					}
				}()
			}
		}
	}
}
func (m *manager) dispatch(ctx context.Context, r RPCRequest) (any, error) {
	ok := func() any { return map[string]any{"status": "ok"} }
	switch r.Op {
	case "state":
		return m.state(ctx), nil
	case "queue":
		m.mu.Lock()
		defer m.mu.Unlock()
		q := m.q
		q.Songs = append([]model.Song{}, q.Songs...)
		return q, nil
	case "play":
		if len(r.Songs) == 0 {
			return nil, errors.New("没有可播放歌曲")
		}
		m.mu.Lock()
		m.intent++
		gen := m.generation
		intent := m.intent
		m.mu.Unlock()
		if err := m.loadQueue(ctx, model.Queue{Songs: r.Songs, CurrentIndex: 0}, r.Value, gen, intent); err != nil {
			return nil, err
		}
		s := r.Songs[0]
		return map[string]any{"id": s.CLIID, "name": s.Name, "singer": s.Singer, "source": s.Source, "status": "playing", "queue_count": len(r.Songs)}, nil
	case "next":
		err := m.advance(ctx, 1, false)
		return ok(), err
	case "prev":
		err := m.advance(ctx, -1, false)
		return ok(), err
	case "queue_add", "queue_insert":
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Op == "queue_insert" {
			pos := m.q.CurrentIndex + 1
			if pos < 0 {
				pos = 0
			}
			tail := append([]model.Song{}, m.q.Songs[pos:]...)
			m.q.Songs = append(m.q.Songs[:pos], r.Songs...)
			m.q.Songs = append(m.q.Songs, tail...)
		} else {
			m.q.Songs = append(m.q.Songs, r.Songs...)
		}
		m.generation++
		err := m.saveLocked()
		return map[string]any{"status": "added", "count": len(r.Songs)}, err
	case "queue_remove":
		before := m.state(ctx)
		wasPlaying := before.Status == "playing" || before.Status == "paused"
		m.mu.Lock()
		if r.Index < 0 || r.Index >= len(m.q.Songs) {
			m.mu.Unlock()
			return nil, errors.New("队列位置超出范围")
		}
		current := r.Index == m.q.CurrentIndex
		if current {
			m.intent++
		}
		if current {
			m.st.Status = "stopped"
			m.st.Position = 0
		}
		m.q.Songs = append(m.q.Songs[:r.Index], m.q.Songs[r.Index+1:]...)
		if r.Index < m.q.CurrentIndex {
			m.q.CurrentIndex--
		}
		if m.q.CurrentIndex >= len(m.q.Songs) {
			m.q.CurrentIndex = len(m.q.Songs) - 1
		}
		m.generation++
		remaining := len(m.q.Songs)
		err := m.saveLocked()
		m.mu.Unlock()
		if err == nil && current && wasPlaying {
			if m.mpv.Running(ctx) {
				_, _ = m.mpv.Command(ctx, "stop")
			}
			if remaining > 0 {
				err = m.start(ctx, "")
			}
		}
		return ok(), err
	case "queue_move":
		m.mu.Lock()
		defer m.mu.Unlock()
		n := len(m.q.Songs)
		if r.Index < 0 || r.Index >= n || r.To < 0 || r.To >= n {
			return nil, errors.New("队列位置超出范围")
		}
		current := ""
		if m.q.CurrentIndex >= 0 {
			current = m.q.Songs[m.q.CurrentIndex].CLIID
		}
		song := m.q.Songs[r.Index]
		m.q.Songs = append(m.q.Songs[:r.Index], m.q.Songs[r.Index+1:]...)
		m.q.Songs = append(m.q.Songs, model.Song{})
		copy(m.q.Songs[r.To+1:], m.q.Songs[r.To:])
		m.q.Songs[r.To] = song
		for i, s := range m.q.Songs {
			if s.CLIID == current {
				m.q.CurrentIndex = i
				break
			}
		}
		m.generation++
		return ok(), m.saveLocked()
	case "queue_clear":
		m.mu.Lock()
		m.intent++
		m.q = model.Queue{Songs: []model.Song{}, CurrentIndex: -1}
		m.generation++
		err := m.saveLocked()
		m.st.Status = "stopped"
		m.mu.Unlock()
		if m.mpv.Running(ctx) {
			_, err = m.mpv.Command(ctx, "stop")
		}
		return ok(), err
	case "stop":
		m.mu.Lock()
		m.intent++
		m.generation++
		m.st.Status = "stopped"
		m.st.Position = 0
		m.mu.Unlock()
		if m.mpv.Running(ctx) {
			_, err := m.mpv.Command(ctx, "stop")
			return ok(), err
		}
		return ok(), nil
	case "quit":
		m.mu.Lock()
		m.intent++
		m.generation++
		m.st.Status = "stopped"
		m.mu.Unlock()
		return ok(), m.mpv.Close(ctx)
	case "pause", "resume":
		if !m.mpv.Running(ctx) {
			return nil, errors.New("播放器未启动")
		}
		_, err := m.mpv.Command(ctx, "set_property", "pause", r.Op == "pause")
		return ok(), err
	case "volume":
		m.mu.Lock()
		if r.Value == "" {
			v := m.st.Volume
			m.mu.Unlock()
			return map[string]any{"volume": v}, nil
		}
		n, err := strconv.Atoi(r.Value)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if strings.HasPrefix(r.Value, "+") || strings.HasPrefix(r.Value, "-") {
			n = m.st.Volume + n
		}
		if n < 0 {
			n = 0
		}
		if n > 100 {
			n = 100
		}
		m.st.Volume = n
		m.mu.Unlock()
		if m.mpv.Running(ctx) {
			_, err = m.mpv.Command(ctx, "set_property", "volume", n)
		}
		return map[string]any{"volume": n}, err
	case "repeat":
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Value != "" {
			if r.Value != "off" && r.Value != "one" && r.Value != "all" {
				return nil, errors.New("循环模式应为 off/one/all")
			}
			m.st.Repeat = r.Value
		}
		return map[string]any{"repeat": m.st.Repeat}, nil
	case "shuffle":
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Value {
		case "":
			m.st.Shuffle = !m.st.Shuffle
		case "on", "true":
			m.st.Shuffle = true
		case "off", "false":
			m.st.Shuffle = false
		default:
			return nil, errors.New("随机模式应为 on/off")
		}
		return map[string]any{"shuffle": m.st.Shuffle}, nil
	case "seek":
		if !m.mpv.Running(ctx) {
			return nil, errors.New("播放器未启动")
		}
		value, mode, err := seekValue(r.Value)
		if err != nil {
			return nil, err
		}
		_, err = m.mpv.Command(ctx, "seek", value, mode)
		return ok(), err
	default:
		return nil, fmt.Errorf("未知后台操作 %q", r.Op)
	}
}
func seekValue(s string) (float64, string, error) {
	mode := "absolute"
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		mode = "relative"
	}
	if strings.HasSuffix(s, "%") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if n < 0 || n > 100 {
			return 0, "", errors.New("百分比超出 0–100")
		}
		return n, "absolute-percent", err
	}
	if strings.Contains(s, ":") {
		n := 0.0
		for _, part := range strings.Split(s, ":") {
			v, err := strconv.ParseFloat(part, 64)
			if err != nil || v < 0 {
				return 0, "", errors.New("时间格式无效")
			}
			n = n*60 + v
		}
		return n, mode, nil
	}
	n, err := strconv.ParseFloat(s, 64)
	return n, mode, err
}
