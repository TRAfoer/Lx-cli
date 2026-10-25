package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"lxcli/internal/config"
	"lxcli/internal/model"
)

func safeFilename(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, v)
	v = strings.Trim(v, " .")
	r := []rune(v)
	if len(r) > 150 {
		v = string(r[:150])
	}
	if v == "" {
		v = "audio"
	}
	base := strings.ToUpper(strings.Split(v, ".")[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		v = "_" + v
	}
	return v
}
func audioExtension(head []byte) (string, error) {
	if len(head) >= 4 && string(head[:4]) == "fLaC" {
		return ".flac", nil
	}
	if len(head) >= 3 && string(head[:3]) == "ID3" || len(head) >= 2 && head[0] == 0xff && head[1]&0xe0 == 0xe0 {
		return ".mp3", nil
	}
	if len(head) >= 4 && string(head[:4]) == "OggS" {
		return ".ogg", nil
	}
	if len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE" {
		return ".wav", nil
	}
	if len(head) >= 8 && string(head[4:8]) == "ftyp" {
		return ".m4a", nil
	}
	return "", errors.New("下载响应不是可识别的音频，可能是音源错误页面")
}

func (e *Engine) DownloadOne(ctx context.Context, task model.DownloadTask) (err error) {
	task.Status = "downloading"
	task.ErrorMessage = ""
	task.BytesDownloaded = 0
	task.BytesTotal = 0
	task.Progress = 0
	task.FilePath = ""
	_ = e.Store.UpdateDownload(task)
	defer func() {
		if err != nil {
			task.Status = "failed"
			task.ErrorMessage = err.Error()
			_ = e.Store.UpdateDownload(task)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.Config.Download.Timeout)*time.Second)
	defer cancel()
	song, err := e.Store.Song(task.Source + ":" + task.SongID)
	if err != nil {
		return err
	}
	audioURL, err := e.Resolve(ctx, song, task.Quality)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, audioURL, nil)
	if err != nil {
		return err
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("下载失败 HTTP %d", resp.StatusCode)
	}
	r := bufio.NewReader(resp.Body)
	head, _ := r.Peek(16)
	ext, err := audioExtension(head)
	if err != nil {
		return err
	}
	dir := config.Expand(e.Config.Download.OutputDir)
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpl := e.Config.Download.FilenameTemplate
	tmpl = strings.TrimSuffix(tmpl, ".{ext}")
	tmpl = strings.NewReplacer("{singer}", song.Singer, "{title}", song.Name, "{album}", song.AlbumName, "{id}", song.SongID, "{source}", song.Source, "{ext}", strings.TrimPrefix(ext, ".")).Replace(tmpl)
	path := filepath.Join(dir, safeFilename(tmpl)+" ["+safeFilename(song.Source+"-"+song.SongID)+"]"+ext)
	if st, statErr := os.Stat(path); statErr == nil && e.Config.Download.SkipExisting {
		task.Status = "completed"
		task.Progress = 100
		task.BytesDownloaded = st.Size()
		task.BytesTotal = st.Size()
		task.FilePath = path
		return e.Store.UpdateDownload(task)
	}
	f, err := os.CreateTemp(dir, ".alx-download-*.part")
	if err != nil {
		return err
	}
	part := f.Name()
	defer os.Remove(part)
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	task.BytesTotal = resp.ContentLength
	if task.BytesTotal < 0 {
		task.BytesTotal = 0
	}
	last := time.Now()
	buf := make([]byte, 64<<10)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			written, writeErr := f.Write(buf[:n])
			task.BytesDownloaded += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			if time.Since(last) > 300*time.Millisecond {
				if task.BytesTotal > 0 {
					task.Progress = float64(task.BytesDownloaded) * 100 / float64(task.BytesTotal)
				}
				_ = e.Store.UpdateDownload(task)
				last = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if resp.ContentLength >= 0 && task.BytesDownloaded != resp.ContentLength {
		return io.ErrUnexpectedEOF
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	closed = true
	if err = os.Rename(part, path); err != nil {
		return err
	}
	task.FilePath = path
	task.Status = "completed"
	task.Progress = 100
	task.BytesTotal = task.BytesDownloaded
	if e.Config.Download.SaveLyricsFile {
		if text, lrcErr := e.Lyric(ctx, song); lrcErr == nil && text != "" {
			_ = config.AtomicWrite(strings.TrimSuffix(path, ext)+".lrc", []byte(text), 0o644)
		}
	}
	return e.Store.UpdateDownload(task)
}
