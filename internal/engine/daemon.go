package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"lxcli/internal/config"
	"lxcli/internal/model"
)

type daemonInfo struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}
type RPCResponse struct {
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error,omitempty"`
}

func (e *Engine) daemonPath() string { return filepath.Join(e.Config.CacheDir, "daemon.json") }
func (e *Engine) readDaemon() (daemonInfo, error) {
	var d daemonInfo
	b, err := os.ReadFile(e.daemonPath())
	if err != nil {
		return d, err
	}
	err = json.Unmarshal(b, &d)
	return d, err
}
func (e *Engine) rpc(ctx context.Context, d daemonInfo, r RPCRequest) (json.RawMessage, error) {
	b, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL+"/rpc", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	resp, err := (&http.Client{Timeout: 75 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out RPCResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	return out.Result, nil
}
func (e *Engine) Call(ctx context.Context, r RPCRequest, start bool) (json.RawMessage, error) {
	d, err := e.readDaemon()
	if err == nil {
		if raw, rpcErr := e.rpc(ctx, d, r); rpcErr == nil {
			return raw, nil
		} else if r.Op != "state" && r.Op != "queue" { // Probe before retrying a mutation: a valid daemon error must not spawn another service.
			probe, cancel := context.WithTimeout(ctx, time.Second)
			_, aliveErr := e.rpc(probe, d, RPCRequest{Op: "state"})
			cancel()
			if aliveErr == nil {
				return nil, rpcErr
			}
		}
	}
	if !start {
		if r.Op == "quit" {
			if _, lockErr := os.Stat(filepath.Join(e.Config.CacheDir, "daemon-start.lock")); lockErr == nil {
				return e.quitStartingDaemon(ctx)
			} else if !os.IsNotExist(lockErr) {
				return nil, lockErr
			}
		}
		switch r.Op {
		case "state":
			st := model.State{Status: "stopped", Repeat: e.Config.Player.Repeat, Volume: e.Config.Player.DefaultVolume}
			var q model.Queue
			if e.Store.Get("queue", &q) == nil {
				st.QueueLength = len(q.Songs)
				st.QueueIndex = q.CurrentIndex
			}
			return json.Marshal(st)
		case "queue":
			q := model.Queue{Songs: []model.Song{}, CurrentIndex: -1}
			_ = e.Store.Get("queue", &q)
			return json.Marshal(q)
		case "stop", "quit":
			return json.Marshal(map[string]any{"status": "ok"})
		default:
			return nil, errors.New("播放器后台未启动")
		}
	}
	if err = e.ensureDaemon(ctx); err != nil {
		return nil, err
	}
	d, err = e.readDaemon()
	if err != nil {
		return nil, err
	}
	return e.rpc(ctx, d, r)
}

// A CLI can be cancelled after detaching the daemon but before its endpoint
// exists. Join that startup before acknowledging Quit to the next client.
func (e *Engine) quitStartingDaemon(parent context.Context) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		if d, err := e.readDaemon(); err == nil {
			if result, rpcErr := e.rpc(ctx, d, RPCRequest{Op: "quit"}); rpcErr == nil {
				return result, nil
			}
		}
		if _, err := os.Stat(filepath.Join(e.Config.CacheDir, "daemon-start.lock")); os.IsNotExist(err) {
			if d, readErr := e.readDaemon(); readErr == nil {
				if result, rpcErr := e.rpc(ctx, d, RPCRequest{Op: "quit"}); rpcErr == nil {
					return result, nil
				}
			}
			return json.Marshal(map[string]any{"status": "ok"})
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("等待旧后台启动并退出: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
func (e *Engine) ensureDaemon(ctx context.Context) error {
	lock := filepath.Join(e.Config.CacheDir, "daemon-start.lock")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d, err := e.readDaemon()
		if err == nil {
			probe, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			_, err = e.rpc(probe, d, RPCRequest{Op: "queue"})
			cancel()
			if err == nil {
				return nil
			}
		}
		f, lockErr := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if lockErr == nil {
			f.Close()
			err = e.spawnDaemon()
			if err != nil {
				os.Remove(lock)
				return err
			}
			for i := 0; i < 100; i++ {
				if ctx.Err() != nil {
					// The detached child owns readiness from here; retain the
					// marker so a following Quit can join and stop it.
					return ctx.Err()
				}
				if d, readErr := e.readDaemon(); readErr == nil {
					probe, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
					_, err = e.rpc(probe, d, RPCRequest{Op: "queue"})
					cancel()
					if err == nil {
						os.Remove(lock)
						return nil
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			return fmt.Errorf("后台启动超时，请检查 %s", filepath.Join(e.Config.CacheDir, "daemon.log"))
		}
		if st, statErr := os.Stat(lock); statErr == nil && time.Since(st.ModTime()) > 20*time.Second {
			_ = os.Remove(lock)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("等待后台启动锁超时")
}
func (e *Engine) spawnDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(e.Config.CacheDir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "--config", e.Config.Path, "_serve")
	cmd.Stdin = nil
	cmd.Stdout = log
	cmd.Stderr = log
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func (e *Engine) Serve(parent context.Context) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	d := daemonInfo{URL: "http://" + listener.Addr().String(), Token: hex.EncodeToString(token), PID: os.Getpid()}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	m := newManager(e)
	defer m.mpv.Close(context.Background())
	go m.events(ctx)
	var jobsMu sync.Mutex
	active := map[int64]bool{}
	lastWork := time.Now()
	quitRequested := false
	activeRPC := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+d.Token {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(RPCResponse{Error: "forbidden"})
			return
		}
		var req RPCRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
			_ = json.NewEncoder(w).Encode(RPCResponse{Error: err.Error()})
			return
		}
		jobsMu.Lock()
		activeRPC++
		lastWork = time.Now()
		if req.Op == "play" || req.Op == "download_wake" {
			quitRequested = false
		}
		jobsMu.Unlock()
		defer func() { jobsMu.Lock(); activeRPC--; jobsMu.Unlock() }()
		var result any
		var dispatchErr error
		if req.Op == "download_wake" {
			result = map[string]any{"status": "queued"}
		} else {
			result, dispatchErr = m.dispatch(r.Context(), req)
		}
		jobsMu.Lock()
		lastWork = time.Now()
		if req.Op == "quit" {
			quitRequested = true
		}
		jobsMu.Unlock()
		out := RPCResponse{}
		if dispatchErr != nil {
			out.Error = dispatchErr.Error()
		} else {
			out.Result, _ = json.Marshal(result)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	b, _ := json.Marshal(d)
	if err = config.AtomicWrite(e.daemonPath(), b, 0o600); err != nil {
		server.Close()
		return err
	}
	_ = os.Remove(filepath.Join(e.Config.CacheDir, "daemon-start.lock"))
	defer func() {
		if current, readErr := e.readDaemon(); readErr == nil && current.Token == d.Token {
			os.Remove(e.daemonPath())
		}
	}()
	// A previous crash left these tasks incomplete. Requeue before scheduling.
	if tasks, listErr := e.Store.Downloads(); listErr == nil {
		for _, task := range tasks {
			if task.Status == "downloading" {
				task.Status = "pending"
				_ = e.Store.UpdateDownload(task)
			}
		}
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown, c := context.WithTimeout(context.Background(), 3*time.Second)
			defer c()
			return server.Shutdown(shutdown)
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			tasks, _ := e.Store.Downloads()
			pending := false
			jobsMu.Lock()
			for _, task := range tasks {
				if task.Status != "pending" {
					continue
				}
				pending = true
				if len(active) >= e.Config.Download.MaxConcurrent || active[task.ID] {
					continue
				}
				active[task.ID] = true
				task := task
				go func() {
					worker, err := Open(e.Config.Path)
					if err == nil {
						defer worker.Close()
						_ = worker.DownloadOne(ctx, task)
					} else {
						task.Status = "failed"
						task.ErrorMessage = err.Error()
						_ = e.Store.UpdateDownload(task)
					}
					jobsMu.Lock()
					delete(active, task.ID)
					lastWork = time.Now()
					jobsMu.Unlock()
				}()
			}
			busy := len(active) > 0 || pending || activeRPC > 0
			idleFor := time.Since(lastWork)
			quit := quitRequested
			jobsMu.Unlock()
			if !busy && (quit || idleFor > 60*time.Second && !m.mpv.Running(ctx)) {
				cancel()
			}
		}
	}
}
