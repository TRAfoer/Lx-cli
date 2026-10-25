package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"lxcli/internal/engine"
)

func TestCLIQueuePlayUsesOneBasedPosition(t *testing.T) {
	setTestHome(t, t.TempDir())
	e, err := engine.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	requests := make(chan engine.RPCRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request engine.RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode RPC: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		_, _ = w.Write([]byte(`{"result":{"status":"ok"}}`))
	}))
	defer server.Close()
	daemon, err := json.Marshal(map[string]any{"url": server.URL, "token": "test", "pid": os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Config.CacheDir, "daemon.json"), daemon, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		position string
		index    int
	}{{"1", 0}, {"3", 2}} {
		var out, errout bytes.Buffer
		if code := Run(context.Background(), []string{"queue", "play", tc.position, "--json", "--no-color"}, &out, &errout); code != 0 {
			t.Fatalf("queue play %s: exit %d: %s %s", tc.position, code, &out, &errout)
		}
		var result map[string]string
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["status"] != "ok" || errout.Len() != 0 {
			t.Fatalf("play result: %s, stderr: %s, error: %v", &out, &errout, err)
		}
		select {
		case request := <-requests:
			if request.Op != "queue_play" || request.Index != tc.index || len(request.Songs) != 0 {
				t.Fatalf("wrong queue play request: %#v", request)
			}
		default:
			t.Fatal("queue play did not reach backend")
		}
	}
	for _, args := range [][]string{{"queue", "play"}, {"queue", "play", "0"}, {"queue", "play", "-1"}, {"queue", "play", "invalid"}} {
		var out, errout bytes.Buffer
		if code := Run(context.Background(), args, &out, &errout); code != 1 {
			t.Fatalf("accepted invalid queue position %v: %d %s", args, code, &out)
		}
		var result map[string]string
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["error"] == "" || errout.Len() != 0 {
			t.Fatalf("invalid position result: %s, stderr: %s, error: %v", &out, &errout, err)
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("invalid position reached backend: %#v", request)
	default:
	}
}
