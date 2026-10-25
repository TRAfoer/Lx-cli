package model

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
)

type Song struct {
	CLIID     string          `json:"cli_id"`
	SongID    string          `json:"song_id"`
	Name      string          `json:"name"`
	Singer    string          `json:"singer"`
	Source    string          `json:"source"`
	Interval  string          `json:"interval"`
	AlbumName string          `json:"album_name"`
	AlbumID   string          `json:"album_id"`
	PicURL    string          `json:"pic_url"`
	Songmid   string          `json:"songmid"`
	Hash      string          `json:"hash"`
	Extra     json.RawMessage `json:"extra"`
}

func SongID(source, id string) string {
	h := md5.Sum([]byte(source + "-" + id))
	return hex.EncodeToString(h[:])[:16]
}

func (s *Song) Normalize() {
	if s.SongID == "" {
		s.SongID = s.Songmid
	}
	if s.Songmid == "" {
		s.Songmid = s.SongID
	}
	if s.CLIID == "" {
		s.CLIID = SongID(s.Source, s.SongID)
	}
}

type Source struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Author      string `json:"author"`
	Homepage    string `json:"homepage"`
	Repository  string `json:"repository"`
	ScriptPath  string `json:"script_path"`
	SourceURL   string `json:"source_url"`
	ContentHash string `json:"content_hash"`
	Platforms   string `json:"platforms"`
	Qualities   string `json:"qualities"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type Playlist struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Count       int    `json:"count"`
}

type ImportedPlaylist struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	Link        string   `json:"link"`
	Songs       []Song   `json:"songs"`
	Total       int      `json:"total"`
	Warnings    []string `json:"warnings,omitempty"`
}

type Queue struct {
	Songs        []Song `json:"songs"`
	CurrentIndex int    `json:"current_index"`
}

type State struct {
	CurrentSong  *Song   `json:"current_song"`
	DaemonActive bool    `json:"daemon_active"`
	Duration     float64 `json:"duration"`
	Position     float64 `json:"position"`
	QueueLength  int     `json:"queue_length"`
	QueueIndex   int     `json:"queue_index"`
	Repeat       string  `json:"repeat"`
	Shuffle      bool    `json:"shuffle"`
	Status       string  `json:"status"`
	Volume       int     `json:"volume"`
}

type DownloadTask struct {
	ID              int64   `json:"id"`
	SongID          string  `json:"song_id"`
	Source          string  `json:"source"`
	Name            string  `json:"name"`
	Singer          string  `json:"singer"`
	Quality         string  `json:"quality"`
	Status          string  `json:"status"`
	Progress        float64 `json:"progress"`
	BytesDownloaded int64   `json:"bytes_downloaded"`
	BytesTotal      int64   `json:"bytes_total"`
	ErrorMessage    string  `json:"error_message"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	FilePath        string  `json:"file_path"`
}
