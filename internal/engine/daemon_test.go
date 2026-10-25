package engine

import (
	"context"
	"lxcli/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIdleStateAndQueueDoNotStartMPV(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("ALX_MPV", "missing-mpv-for-test")
	e, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	m := newManager(e)
	st := m.state(context.Background())
	if st.DaemonActive || st.Status != "stopped" {
		t.Fatalf("%#v", st)
	}
	_, err = m.dispatch(context.Background(), RPCRequest{Op: "queue_add", Songs: []model.Song{{Source: "wy", SongID: "1", Name: "queued"}}})
	if err != nil {
		t.Fatal(err)
	}
	if st = m.state(context.Background()); st.DaemonActive || st.QueueLength != 1 {
		t.Fatalf("%#v", st)
	}
	if _, err = m.dispatch(context.Background(), RPCRequest{Op: "queue_remove", Index: 3}); err == nil {
		t.Fatal("out-of-range accepted")
	}
	var q model.Queue
	if err = e.Store.Get("queue", &q); err != nil || len(q.Songs) != 1 {
		t.Fatalf("queue not persisted %#v %v", q, err)
	}
}

func TestQuitWaitsForDaemonBeingStarted(t *testing.T) {
	setTestHome(t, t.TempDir())
	e, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err = os.WriteFile(filepath.Join(e.Config.CacheDir, "daemon-start.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { time.Sleep(100 * time.Millisecond); done <- e.Serve(serveCtx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()
	ctx, timeout := context.WithTimeout(context.Background(), 3*time.Second)
	defer timeout()
	if _, err = e.Call(ctx, RPCRequest{Op: "quit"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
		done <- nil
	case <-time.After(time.Second):
		t.Fatal("quit returned before startup completed and left the old daemon running")
	}
}

func TestFailedResolutionPreservesCurrentSongAndQueue(t *testing.T) {
	setTestHome(t, t.TempDir())
	e, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	m := newManager(e)
	m.q = model.Queue{Songs: []model.Song{{Source: "wy", SongID: "1", Name: "original"}, {Source: "wy", SongID: "2", Name: "unplayable"}}, CurrentIndex: 0}
	if err = m.advance(context.Background(), 1, false); err == nil {
		t.Fatal("missing source unexpectedly resolved")
	}
	if m.q.CurrentIndex != 0 {
		t.Fatalf("failed skip changed index to %d", m.q.CurrentIndex)
	}
	if _, err = m.dispatch(context.Background(), RPCRequest{Op: "play", Songs: []model.Song{{Source: "wy", SongID: "3", Name: "new unplayable"}}}); err == nil {
		t.Fatal("missing source unexpectedly played")
	}
	if len(m.q.Songs) != 2 || m.q.Songs[0].Name != "original" {
		t.Fatalf("failed play replaced queue: %#v", m.q)
	}
}

func TestRemovingStoppedCurrentItemDoesNotRequireOrStartPlayer(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("ALX_MPV", "missing-test-player")
	e, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	m := newManager(e)
	m.q = model.Queue{Songs: []model.Song{{Source: "wy", SongID: "1"}, {Source: "wy", SongID: "2"}}, CurrentIndex: 0}
	if _, err = m.dispatch(context.Background(), RPCRequest{Op: "queue_remove", Index: 0}); err != nil {
		t.Fatalf("stopped queue edit failed: %v", err)
	}
	if len(m.q.Songs) != 1 || m.mpv.Running(context.Background()) {
		t.Fatal("queue removal started a player")
	}
}
