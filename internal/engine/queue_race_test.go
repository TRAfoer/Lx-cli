package engine

import (
	"context"
	"encoding/json"
	"lxcli/internal/model"
	"lxcli/internal/player"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Real JS/HTTP resolution with only the external mpv process substituted.
type acceptingPlayer struct{}

func (acceptingPlayer) Ensure(context.Context) error { return nil }
func (acceptingPlayer) Command(context.Context, ...any) (json.RawMessage, error) {
	return json.RawMessage(`null`), nil
}
func (acceptingPlayer) Running(context.Context) bool { return true }
func (acceptingPlayer) Close(context.Context) error  { return nil }
func (acceptingPlayer) Events() <-chan player.Event  { return nil }

func TestAddingQueueDuringAutomaticResolutionKeepsPlayingAndPreservesAddition(t *testing.T) {
	setTestHome(t, t.TempDir())
	e, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"url":"https://example.com/audio.flac"}`))
	}))
	defer server.Close()
	script := `/** @name delayed */
lx.send('inited',{status:true,sources:{wy:{qualitys:['320k']}}});
lx.on('request',()=>new Promise((resolve,reject)=>lx.request('` + server.URL + `',{},(err,resp,body)=>err?reject(err):resolve(body.url))));`
	path := filepath.Join(t.TempDir(), "slow.js")
	if err = os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AddSource(context.Background(), path, ""); err != nil {
		t.Fatal(err)
	}
	m := newManager(e)
	m.mpv = acceptingPlayer{}
	m.q = model.Queue{Songs: []model.Song{{Source: "wy", SongID: "1"}, {Source: "wy", SongID: "2"}}, CurrentIndex: 0}
	result := make(chan error, 1)
	go func() { result <- m.advance(context.Background(), 1, true) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("resolution did not begin")
	}
	if _, err = m.dispatch(context.Background(), RPCRequest{Op: "queue_add", Songs: []model.Song{{Source: "wy", SongID: "3", Name: "added"}}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = <-result; err != nil {
		t.Fatalf("queue addition interrupted automatic playback: %v", err)
	}
	if len(m.q.Songs) != 3 || m.q.CurrentIndex != 1 || m.q.Songs[2].Name != "added" || m.st.Status != "playing" {
		t.Fatalf("queue/state %#v %#v", m.q, m.st)
	}
}
