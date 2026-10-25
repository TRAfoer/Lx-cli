package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"lxcli/internal/engine"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setTestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("ALX_HOME", home)
	for _, key := range []string{"ALX_CONFIG", "ALX_DATA", "ALX_CACHE"} {
		t.Setenv(key, "")
	}
}

func TestCLIIdentityUsesPlainTextWithoutOpeningConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(home, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	setTestHome(t, home)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"version", []string{"--version"}},
		{"help", []string{"--help"}},
		{"no arguments", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errout bytes.Buffer
			if code := Run(context.Background(), tc.args, &out, &errout); code != 0 {
				t.Fatalf("exit %d: %s", code, out.String())
			}
			text := out.String()
			if tc.name == "version" {
				if text != "Lx-Cli 0.1.1-go\n" {
					t.Fatalf("version: %q", text)
				}
			} else if !strings.HasPrefix(text, "用法: Lx-Cli ") || !strings.Contains(text, "playlist import") || strings.Contains(text, "alx-go") {
				t.Fatalf("command-line help: %q", text)
			}
			if strings.ContainsAny(text, "\x1b\r┌┐└┘│─") || errout.Len() != 0 {
				t.Fatalf("unexpected terminal rendering or stderr: stdout=%q stderr=%q", text, errout.String())
			}
		})
	}
}

func TestCLIErrorsRemainOneJSONObjectOnStdout(t *testing.T) {
	setTestHome(t, t.TempDir())
	for _, args := range [][]string{
		{"--unknown"},
		{"--json", "--no-color", "not-a-command"},
		{"playlist", "show", "missing"},
	} {
		var out, errout bytes.Buffer
		if code := Run(context.Background(), args, &out, &errout); code != 1 {
			t.Fatalf("%v: exit %d", args, code)
		}
		var result map[string]string
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result) != 1 || result["error"] == "" {
			t.Fatalf("%v: stdout is not a single error object: %q (%v)", args, out.String(), err)
		}
		if errout.Len() != 0 {
			t.Fatalf("%v: stderr=%q", args, errout.String())
		}
	}
}

func TestCLIEmptyIDOnlySearchProducesNoOutput(t *testing.T) {
	setTestHome(t, t.TempDir())
	file := filepath.Join(t.TempDir(), "empty-source.js")
	script := `/* @name Empty search fixture */
console.log('must not reach stdout');
lx.send('inited', {status:true,sources:{tx:{actions:['musicSearch'],qualitys:['320k']}}});
lx.on('request', () => []);`
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := Run(context.Background(), []string{"source", "add", file}, &out, &errout); code != 0 {
		t.Fatalf("register source: %d %s", code, out.String())
	}
	for _, cmd := range []string{"search", "sch"} {
		out.Reset()
		errout.Reset()
		if code := Run(context.Background(), []string{cmd, "empty", "--source", "tx", "--id-only", "--no-color"}, &out, &errout); code != 0 {
			t.Fatalf("search: %d %s", code, out.String())
		}
		if out.Len() != 0 || errout.Len() != 0 {
			t.Fatalf("empty ID output: stdout=%q stderr=%q", out.String(), errout.String())
		}
	}
}

func TestCLIPersistsImportAndNeverOverwritesExistingPlaylist(t *testing.T) {
	setTestHome(t, t.TempDir())
	file := filepath.Join(t.TempDir(), "playlist.json")
	if err := os.WriteFile(file, []byte(`{"name":"链接歌单","songs":[{"source":"wy","song_id":"123","name":"Test"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string) {
		var out, errout bytes.Buffer
		code := Run(context.Background(), args, &out, &errout)
		return code, out.String()
	}
	if code, out := run("playlist", "import", file, "--json"); code != 0 || !strings.Contains(out, `"count":1`) {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ := run("playlist", "import", file, "--json"); code == 0 {
		t.Fatal("same-name import succeeded")
	}
	code, out := run("--json", "playlist", "show", "链接歌单")
	if code != 0 {
		t.Fatal(out)
	}
	var songs []map[string]any
	if err := json.Unmarshal([]byte(out), &songs); err != nil {
		t.Fatal(err)
	}
	if len(songs) != 1 || songs[0]["cli_id"] != "f7ecf16f7f10680c" {
		t.Fatalf("%s", out)
	}
	if code, out := run("lyric", "unknown", "--json"); code == 0 || !strings.Contains(out, `"error"`) {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := run("state", "--json"); code != 0 || !strings.Contains(out, `"daemon_active":false`) {
		t.Fatalf("%d %s", code, out)
	}
}

func TestDirectSongCanBeAddedToPlaylist(t *testing.T) {
	setTestHome(t, t.TempDir())
	e, err := engine.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	path := filepath.Join(t.TempDir(), "local.wav")
	if err = os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	songs, err := resolve(e, []string{path}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Store.CreatePlaylist("local", ""); err != nil {
		t.Fatal(err)
	}
	_, err = playlistCommand(context.Background(), e, []string{"add", "local", songs[0].CLIID}, options{})
	if err != nil {
		t.Fatalf("directly played file cannot be saved: %v", err)
	}
}

func TestCLIRejectsUnknownOptionsAndInvalidNumbers(t *testing.T) {
	setTestHome(t, t.TempDir())
	for _, args := range [][]string{{"search", "test", "--page", "oops"}, {"not-a-command"}, {"playlist", "create", "x", "--typo"}} {
		var out bytes.Buffer
		if Run(context.Background(), args, &out, &out) == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
}
