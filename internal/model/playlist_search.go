package model

// RemotePlaylist describes a public platform playlist without importing it.
type RemotePlaylist struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Link        string `json:"link"`
	Author      string `json:"author"`
	PicURL      string `json:"pic_url"`
	Count       int    `json:"count"`
	PlayCount   int64  `json:"play_count"`
}

// PlaylistSearchResult uses a per-source Limit when searching all platforms.
type PlaylistSearchResult struct {
	Playlists []RemotePlaylist `json:"playlists"`
	Total     int              `json:"total"`
	Page      int              `json:"page"`
	Limit     int              `json:"limit"`
	HasMore   bool             `json:"has_more"`
	Warnings  []string         `json:"warnings,omitempty"`
}
