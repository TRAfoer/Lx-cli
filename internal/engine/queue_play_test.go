package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lxcli/internal/model"
)

// Only the external player is replaced; queue persistence and resolution are real.
type queuePlayPlayer struct {
	acceptingPlayer
	ensureErr error
	loadErr   error
	loaded    [][]any
}

func (p *queuePlayPlayer) Ensure(context.Context) error { return p.ensureErr }
func (p *queuePlayPlayer) Command(_ context.Context, args ...any) (json.RawMessage, error) {
	if len(args) > 0 && args[0] == "loadfile" {
		p.loaded = append(p.loaded, append([]any{}, args...))
		if p.loadErr != nil {
			return nil, p.loadErr
		}
	}
	return json.RawMessage(`null`), nil
}

func TestQueuePlayPreservesQueueAndUsesRequestedOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		index   int
		shuffle bool
		url     string
	}{
		{"stopped first item", "stopped", 0, false, "https://example.com/a.flac"},
		{"playing duplicate occurrence", "playing", 2, false, "https://example.com/a.flac"},
		{"paused shuffle ignores random selection", "paused", 1, true, "https://example.com/b.flac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			e, err := Open("")
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			m := newManager(e)
			p := &queuePlayPlayer{}
			m.mpv = p
			m.q = model.Queue{Songs: []model.Song{
				{CLIID: "a", Source: "url", SongID: "https://example.com/a.flac", Name: "A"},
				{CLIID: "b", Source: "url", SongID: "https://example.com/b.flac", Name: "B"},
				{CLIID: "a", Source: "url", SongID: "https://example.com/a.flac", Name: "A"},
			}, CurrentIndex: 0}
			original := append([]model.Song{}, m.q.Songs...)
			m.st.Status, m.st.Position, m.st.Shuffle = tc.status, 19, tc.shuffle

			if _, err = m.dispatch(context.Background(), RPCRequest{Op: "queue_play", Index: tc.index}); err != nil {
				t.Fatalf("queue play failed: %v", err)
			}
			if !reflect.DeepEqual(m.q.Songs, original) || m.q.CurrentIndex != tc.index {
				t.Fatalf("queue changed or wrong occurrence selected: %#v", m.q)
			}
			if m.st.Status != "playing" || m.st.Position != 0 || m.st.Shuffle != tc.shuffle {
				t.Fatalf("playback state: %#v", m.st)
			}
			if !reflect.DeepEqual(p.loaded, [][]any{{"loadfile", tc.url, "replace"}}) {
				t.Fatalf("wrong audio loaded: %#v", p.loaded)
			}
			var persisted model.Queue
			if err = e.Store.Get("queue", &persisted); err != nil || !sameQueueJSON(persisted, m.q) {
				t.Fatalf("queue not persisted: %#v, %v", persisted, err)
			}
		})
	}
}

func TestQueuePlayFailurePreservesQueueAndPlaybackState(t *testing.T) {
	playerErr := errors.New("player rejected file")
	for _, tc := range []struct {
		name       string
		index      int
		empty      bool
		unresolved bool
		ensureErr  error
		loadErr    error
		wantErr    error
	}{
		{name: "negative index", index: -1},
		{name: "past last index", index: 2},
		{name: "empty queue", index: 0, empty: true},
		{name: "resolution fails", index: 1, unresolved: true, wantErr: fs.ErrNotExist},
		{name: "player cannot start", index: 1, ensureErr: playerErr, wantErr: playerErr},
		{name: "player rejects file", index: 1, loadErr: playerErr, wantErr: playerErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			e, err := Open("")
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			m := newManager(e)
			m.mpv = &queuePlayPlayer{ensureErr: tc.ensureErr, loadErr: tc.loadErr}
			m.q = model.Queue{Songs: []model.Song{
				{CLIID: "a", Source: "url", SongID: "https://example.com/a.flac"},
				{CLIID: "b", Source: "url", SongID: "https://example.com/b.flac"},
			}, CurrentIndex: 0}
			if tc.unresolved {
				m.q.Songs[1] = model.Song{CLIID: "missing", Source: "local", SongID: filepath.Join(t.TempDir(), "missing.wav")}
			}
			if tc.empty {
				m.q = model.Queue{Songs: []model.Song{}, CurrentIndex: -1}
			}
			m.st.Status, m.st.Position = "paused", 19
			original := model.Queue{Songs: append([]model.Song{}, m.q.Songs...), CurrentIndex: m.q.CurrentIndex}
			originalState := m.st
			if err = m.saveLocked(); err != nil {
				t.Fatal(err)
			}

			_, err = m.dispatch(context.Background(), RPCRequest{Op: "queue_play", Index: tc.index})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("wrong playback failure: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "队列位置超出范围") {
				t.Fatalf("wrong index failure: %v", err)
			}
			if !reflect.DeepEqual(m.q, original) || !reflect.DeepEqual(m.st, originalState) {
				t.Fatalf("failure changed queue/state: %#v, %#v", m.q, m.st)
			}
			var persisted model.Queue
			if err = e.Store.Get("queue", &persisted); err != nil || !sameQueueJSON(persisted, original) {
				t.Fatalf("failure changed persisted queue: %#v, %v", persisted, err)
			}
		})
	}
}

func sameQueueJSON(a, b model.Queue) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}
