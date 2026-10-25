# Online playlist search

Add keyword search for public NetEase and QQ Music playlists to Lx-Cli and the adjacent Alx-tui repository. Use native platform catalog requests, independently implemented against platform response formats; JS playback sources keep their existing role.

The CLI exposes `playlist search <keywords> --source all|wy|tx --page N --limit N` and `playlist inspect <link>`. Search returns playlist metadata and pagination. Inspect returns remote metadata and songs, caching song identities for playback without creating a local playlist. Existing `playlist import` saves a searched playlist with an optional local name.

The TUI search page offers song/playlist modes. Playlist mode supports all/NetEase/QQ, selection, pagination, loading/errors, read-only song preview, and import using the existing name dialog. Search and preview responses must be rejected when stale. Song search and existing keyboard/mouse controls remain usable; new actions must be discoverable by visible labels/help.

Model contract: `RemotePlaylist` has JSON fields `id`, `name`, `description`, `source`, `link`, `author`, `pic_url`, `count`, `play_count`. `PlaylistSearchResult` has `playlists` (always an array), `total`, `page`, `limit`, `has_more`, optional `warnings`. Aggregate all requests one page per source with deterministic wy/tx ordering, per-source limit, summed total and any-source has_more. A failed source produces a warning if another succeeds; all failures return an error. Explicit unsupported platforms fail before HTTP. Bounds: page defaults to 1, maximum 1,000,000; limit defaults to 20, maximum 100. `ImportedPlaylist` remains the inspect result.

Use existing Go dependencies and HTTP timeouts/response caps. Cover pagination, empty results, invalid input, malformed responses, partial failures, and stale UI messages with offline tests. Run both repositories' tests, vet and builds, then independently review. Commit only this task's changes; preserve the pre-existing TUI `internal/ui/songlist.go` edit. Push each repository to its configured remote as requested.
