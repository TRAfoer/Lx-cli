package cli

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"lxcli/internal/model"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRealCLIBackgroundDownloadAndQuit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds executable for background-process integration")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "Lx-Cli")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "../../cmd/lx-cli")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	home := filepath.Join(dir, "home")
	setTestHome(t, home)
	env := os.Environ()
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, b)
		}
		return bytes.TrimSpace(b)
	}
	t.Cleanup(func() { cmd := exec.Command(bin, "quit"); cmd.Env = env; _ = cmd.Run() })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "fLaC-real-cli-download-content")
	}))
	defer server.Close()
	script := filepath.Join(dir, "source.js")
	if err := os.WriteFile(script, []byte(`/** @name Download fixture */
lx.send('inited',{status:true,sources:{wy:{qualitys:['320k'],actions:['musicUrl']}}});
lx.on('request',()=> '`+server.URL+`');`), 0o600); err != nil {
		t.Fatal(err)
	}
	run("config", "set", "download.output_dir", filepath.Join(dir, "downloads"))
	run("config", "set", "download.save_lyrics_file", "false")
	run("source", "add", script)
	file := filepath.Join(dir, "import.json")
	if err := os.WriteFile(file, []byte(`{"name":"Download","songs":[{"source":"wy","song_id":"123","name":"Real"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run("playlist", "import", file)
	run("download", "add", "f7ecf16f7f10680c")
	deadline := time.Now().Add(10 * time.Second)
	var tasks []model.DownloadTask
	for time.Now().Before(deadline) {
		if err := json.Unmarshal(run("download", "list"), &tasks); err != nil {
			t.Fatal(err)
		}
		if len(tasks) == 1 && (tasks[0].Status == "completed" || tasks[0].Status == "failed") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(tasks) != 1 || tasks[0].Status != "completed" {
		t.Fatalf("background download: %#v", tasks)
	}
	b, err := os.ReadFile(tasks[0].FilePath)
	if err != nil || string(b) != "fLaC-real-cli-download-content" {
		t.Fatalf("file: %q %v", b, err)
	}
	// A new play immediately after quit must survive the old shutdown request.
	wav := make([]byte, 44+44100*2*10)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 44100)
	binary.LittleEndian.PutUint32(wav[28:], 88200)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(wav)-44))
	wavPath := filepath.Join(dir, "silence.wav")
	if err = os.WriteFile(wavPath, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	run("config", "set", "player.mpv_args", `["--ao=null"]`)
	if _, err = exec.LookPath("mpv"); err == nil || os.Getenv("ALX_MPV") != "" {
		run("play", wavPath)
		run("quit")
		run("play", wavPath)
		time.Sleep(800 * time.Millisecond)
		var st model.State
		if err = json.Unmarshal(run("state"), &st); err != nil {
			t.Fatal(err)
		}
		if !st.DaemonActive || st.Status != "playing" {
			t.Fatalf("new playback killed by previous quit: %#v", st)
		}
	}
	run("quit")
}
