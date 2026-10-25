package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"lxcli/internal/model"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Store struct{ DB *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS songs (id TEXT PRIMARY KEY, source TEXT NOT NULL, song_id TEXT NOT NULL, payload TEXT NOT NULL, UNIQUE(source,song_id));
CREATE TABLE IF NOT EXISTS playlists (name TEXT PRIMARY KEY, description TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS playlist_songs (playlist TEXT NOT NULL REFERENCES playlists(name) ON DELETE CASCADE ON UPDATE CASCADE, song TEXT NOT NULL REFERENCES songs(id), position INTEGER NOT NULL, PRIMARY KEY(playlist,song));
CREATE TABLE IF NOT EXISTS sources (id TEXT PRIMARY KEY,payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY,payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS downloads (id INTEGER PRIMARY KEY AUTOINCREMENT,payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS history (id INTEGER PRIMARY KEY AUTOINCREMENT,song TEXT NOT NULL,played_at TEXT NOT NULL);
INSERT OR IGNORE INTO playlists(name,description) VALUES ('Favorites','收藏');`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func validSong(song model.Song) error {
	if song.Source == "" || song.SongID == "" {
		return errors.New("歌曲缺少平台或原始 ID")
	}
	return nil
}
func cache(tx *sql.Tx, song model.Song) error {
	song.Normalize()
	if err := validSong(song); err != nil {
		return err
	}
	b, err := json.Marshal(song)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO songs(id,source,song_id,payload) VALUES (?,?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload`, song.CLIID, song.Source, song.SongID, string(b))
	return err
}
func (s *Store) CacheSongs(songs []model.Song) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, song := range songs {
		if err = cache(tx, song); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Song(id string) (model.Song, error) {
	var song model.Song
	var rows *sql.Rows
	var err error
	if a, b, ok := strings.Cut(id, ":"); ok {
		rows, err = s.DB.Query(`SELECT payload FROM songs WHERE source=? AND song_id=?`, a, b)
	} else {
		rows, err = s.DB.Query(`SELECT payload FROM songs WHERE id=? OR substr(id,1,length(?))=? LIMIT 2`, id, id, id)
	}
	if err != nil {
		return song, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var b string
		if err = rows.Scan(&b); err != nil {
			return song, err
		}
		if err = json.Unmarshal([]byte(b), &song); err != nil {
			return song, err
		}
	}
	if err = rows.Err(); err != nil {
		return song, err
	}
	if count == 0 {
		return song, fmt.Errorf("CLI ID %q not found in cache，请先搜索或导入歌单", id)
	}
	if count > 1 {
		return model.Song{}, fmt.Errorf("ID %q 不唯一，请使用完整 cli_id", id)
	}
	return song, nil
}
func validName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("歌单名称不能为空")
	}
	if len(name) > 500 {
		return errors.New("歌单名称过长")
	}
	return nil
}
func (s *Store) CreatePlaylist(name, description string) error {
	if err := validName(name); err != nil {
		return err
	}
	_, err := s.DB.Exec(`INSERT INTO playlists(name,description) VALUES (?,?)`, name, description)
	if err != nil {
		return fmt.Errorf("无法创建歌单（名称可能已存在）: %w", err)
	}
	return nil
}
func (s *Store) ListPlaylists() ([]model.Playlist, error) {
	out := []model.Playlist{}
	rows, err := s.DB.Query(`SELECT p.name,p.description,count(ps.song) FROM playlists p LEFT JOIN playlist_songs ps ON p.name=ps.playlist GROUP BY p.name ORDER BY p.rowid`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var p model.Playlist
		if err = rows.Scan(&p.Name, &p.Description, &p.Count); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) PlaylistSongs(name string) ([]model.Song, error) {
	var exists int
	if err := s.DB.QueryRow(`SELECT 1 FROM playlists WHERE name=?`, name).Scan(&exists); err != nil {
		return nil, fmt.Errorf("歌单 %q 不存在", name)
	}
	out := []model.Song{}
	rows, err := s.DB.Query(`SELECT s.payload FROM playlist_songs ps JOIN songs s ON s.id=ps.song WHERE ps.playlist=? ORDER BY ps.position`, name)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		var song model.Song
		if err = rows.Scan(&b); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(b), &song); err != nil {
			return out, err
		}
		out = append(out, song)
	}
	return out, rows.Err()
}
func (s *Store) ImportPlaylist(name, description string, songs []model.Song) error {
	if err := validName(name); err != nil {
		return err
	}
	if len(songs) == 0 {
		return errors.New("歌单没有可导入歌曲")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO playlists(name,description) VALUES (?,?)`, name, description); err != nil {
		return fmt.Errorf("歌单 %q 已存在，请用 --name 指定新名称", name)
	}
	seen := map[string]bool{}
	pos := 0
	for _, song := range songs {
		song.Normalize()
		if seen[song.CLIID] {
			continue
		}
		seen[song.CLIID] = true
		if err = cache(tx, song); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO playlist_songs(playlist,song,position) VALUES (?,?,?)`, name, song.CLIID, pos); err != nil {
			return err
		}
		pos++
	}
	return tx.Commit()
}
func (s *Store) AddToPlaylist(name string, songs []model.Song) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow(`SELECT 1 FROM playlists WHERE name=?`, name).Scan(&exists); err != nil {
		return fmt.Errorf("歌单 %q 不存在", name)
	}
	var pos int
	err = tx.QueryRow(`SELECT COALESCE(MAX(position),-1)+1 FROM playlist_songs WHERE playlist=?`, name).Scan(&pos)
	if err != nil {
		return err
	}
	for _, song := range songs {
		song.Normalize()
		if err = cache(tx, song); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO playlist_songs(playlist,song,position) VALUES (?,?,?)`, name, song.CLIID, pos); err != nil {
			return err
		}
		pos++
	}
	return tx.Commit()
}
func (s *Store) RemoveFromPlaylist(name, id string) error {
	song, err := s.Song(id)
	if err != nil {
		return err
	}
	r, err := s.DB.Exec(`DELETE FROM playlist_songs WHERE playlist=? AND song=?`, name, song.CLIID)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errors.New("歌单中没有该歌曲")
	}
	return nil
}
func (s *Store) DeletePlaylist(name string) error {
	if name == "Favorites" {
		return errors.New("不能删除 Favorites")
	}
	r, err := s.DB.Exec(`DELETE FROM playlists WHERE name=?`, name)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errors.New("歌单不存在")
	}
	return nil
}
func (s *Store) RenamePlaylist(old, next string) error {
	if old == "Favorites" {
		return errors.New("不能重命名 Favorites")
	}
	if err := validName(next); err != nil {
		return err
	}
	r, err := s.DB.Exec(`UPDATE playlists SET name=? WHERE name=?`, next, old)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errors.New("歌单不存在")
	}
	return nil
}
func (s *Store) Sources() ([]model.Source, error) {
	out := []model.Source{}
	rows, err := s.DB.Query(`SELECT payload FROM sources ORDER BY rowid`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		var v model.Source
		if err = rows.Scan(&b); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(b), &v); err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) PutSource(v model.Source) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO sources(id,payload) VALUES (?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload`, v.ID, string(b))
	return err
}
func (s *Store) DeleteSource(id string) error {
	r, err := s.DB.Exec(`DELETE FROM sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errors.New("音源不存在")
	}
	return nil
}
func (s *Store) Set(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO kv(key,payload) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET payload=excluded.payload`, key, string(b))
	return err
}
func (s *Store) Get(key string, v any) error {
	var b string
	if err := s.DB.QueryRow(`SELECT payload FROM kv WHERE key=?`, key).Scan(&b); err != nil {
		return err
	}
	return json.Unmarshal([]byte(b), v)
}
func (s *Store) NewDownload(v model.DownloadTask) (model.DownloadTask, error) {
	v.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	v.UpdatedAt = v.CreatedAt
	v.Status = "pending"
	b, _ := json.Marshal(v)
	r, err := s.DB.Exec(`INSERT INTO downloads(payload) VALUES (?)`, string(b))
	if err != nil {
		return v, err
	}
	v.ID, err = r.LastInsertId()
	if err != nil {
		return v, err
	}
	return v, s.UpdateDownload(v)
}
func (s *Store) UpdateDownload(v model.DownloadTask) error {
	v.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`UPDATE downloads SET payload=? WHERE id=?`, string(b), v.ID)
	return err
}
func (s *Store) Downloads() ([]model.DownloadTask, error) {
	out := []model.DownloadTask{}
	rows, err := s.DB.Query(`SELECT payload FROM downloads ORDER BY id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		var v model.DownloadTask
		if err = rows.Scan(&b); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(b), &v); err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) AddHistory(song model.Song) error {
	b, err := json.Marshal(song)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO history(song,played_at) VALUES (?,?)`, string(b), time.Now().UTC().Format(time.RFC3339))
	return err
}
func (s *Store) History(limit int) ([]model.Song, error) {
	out := []model.Song{}
	rows, err := s.DB.Query(`SELECT song FROM history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		var song model.Song
		if err = rows.Scan(&b); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(b), &song); err != nil {
			return out, err
		}
		out = append(out, song)
	}
	return out, rows.Err()
}
