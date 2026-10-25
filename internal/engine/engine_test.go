package engine

import (
	"context"
	"io"
	"lxcli/internal/model"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func setTestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("ALX_HOME", home)
	for _, key := range []string{"ALX_CONFIG", "ALX_DATA", "ALX_CACHE"} {
		t.Setenv(key, "")
	}
}

func TestSourceCanResolveImportedMetadataAndDownloadAtomically(t *testing.T) {
	setTestHome(t, t.TempDir())
	e, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Config.Download.OutputDir = filepath.Join(t.TempDir(), "downloads")
	e.Config.Download.SaveLyricsFile = false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		_, _ = io.WriteString(w, "fLaC0123456789")
	}))
	defer server.Close()
	script := `/** @name fixture */
lx.send('inited',{status:true,sources:{wy:{qualitys:['320k'],actions:['musicUrl']}}});
lx.on('request',({action,info})=>{if(info.musicInfo.name!=='Imported')throw Error('metadata missing');return '` + server.URL + `';});`
	path := filepath.Join(t.TempDir(), "fixture.js")
	if err = os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AddSource(context.Background(), path, ""); err != nil {
		t.Fatal(err)
	}
	song := model.Song{Source: "wy", SongID: "1", Name: "Imported", Singer: "../CON"}
	song.Normalize()
	if err = e.Store.CacheSongs([]model.Song{song}); err != nil {
		t.Fatal(err)
	}
	task, err := e.Store.NewDownload(model.DownloadTask{SongID: "1", Source: "wy", Name: song.Name, Singer: song.Singer, Quality: "320k"})
	if err != nil {
		t.Fatal(err)
	}
	// A recovered task restarts with a fresh temporary file, not a byte range.
	task.BytesDownloaded = 7
	task.Progress = 50
	if err = e.Store.UpdateDownload(task); err != nil {
		t.Fatal(err)
	}
	if err = e.DownloadOne(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	list, _ := e.Store.Downloads()
	if len(list) != 1 || list[0].Status != "completed" {
		t.Fatalf("%#v", list)
	}
	if filepath.Dir(list[0].FilePath) != e.Config.Download.OutputDir {
		t.Fatal("filename escaped output dir")
	}
	if _, err = os.Stat(list[0].FilePath + ".part"); !os.IsNotExist(err) {
		t.Fatal("part file remains")
	}
}

func TestFailedDownloadNeverCompletes(t *testing.T) {
	setTestHome(t, t.TempDir())
	e, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	song := model.Song{Source: "wy", SongID: "no-source", Name: "fail"}
	e.Store.CacheSongs([]model.Song{song})
	task, err := e.Store.NewDownload(model.DownloadTask{Source: "wy", SongID: song.SongID, Name: song.Name, Quality: "320k"})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.DownloadOne(context.Background(), task); err == nil {
		t.Fatal("missing source succeeded")
	}
	list, _ := e.Store.Downloads()
	if list[0].Status != "failed" || list[0].ErrorMessage == "" {
		t.Fatalf("%#v", list)
	}
}
