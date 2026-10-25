# Online Playlist Search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Find online playlists by keyword, preview their songs, and import them from the CLI and TUI.

**Architecture:** Lx-Cli catalog requests public NetEase/QQ endpoints and normalizes metadata. CLI commands provide a stable JSON boundary consumed by Alx-tui, which adds playlist search/preview alongside existing song search.

**Tech Stack:** Go 1.23+, net/http, existing SQLite store, Bubble Tea.

**Spec:** `docs/superpowers/specs/2026-10-25-playlist-search-design.md`

## Global Constraints

- Existing Go dependencies only; public wy/tx playlists only.
- Preserve the pre-existing TUI `internal/ui/songlist.go` edit and exclude it from task commits.
- JSON fields and pagination semantics follow the spec exactly.
- No JS source is required for playlist metadata; playback keeps existing source behavior.

## Review Focus

- Missing or malformed platform result objects produce errors instead of silent empty results.
- One failed aggregate source retains successful results and exposes warnings.
- Aggregate pagination continues while any successful source has another page.
- Stale search/preview replies cannot replace a newer mode, selection, or query.
- Preview caches usable song identities but never creates a local playlist.

### Task 1: Native catalog search

**Files:** `internal/model/playlist_search.go`, `internal/catalog/playlist_search.go`, `internal/catalog/playlist_search_test.go`.

**Interfaces:** Produces `(*catalog.Client).SearchPlaylists(ctx context.Context, platform, keyword string, page, limit int) (model.PlaylistSearchResult, error)` and the model structs defined by the spec.

- [x] Write fixture HTTP tests for wy/tx request pagination and mapping, all-source partial failure/order/has_more, empty arrays, invalid parameters, malformed results and canceled requests.
- [x] Run `go test ./internal/catalog -run PlaylistSearch` and confirm missing-feature failure.
- [x] Implement the native adapters and normalization, using existing catalog HTTP helpers.
- [x] Run catalog tests and the Lx-Cli suite; report RED/GREEN evidence.

### Task 2: CLI search and inspect

**Files:** `internal/cli/playlist.go`, `internal/cli/playlist_search_test.go`, `internal/cli/cli.go`, `README.md`.

**Interfaces:** Consumes Task 1; adds JSON `playlist search` and `playlist inspect` commands. Inspect calls `Catalog.ImportPlaylist` and `Store.CacheSongs` but not `Store.ImportPlaylist`.

- [x] Write failing tests proving option/keyword forwarding, validation, cache-backed inspection and no local playlist creation.
- [x] Implement routing/help/docs, run `go test ./internal/cli` and full suite.

### Task 3: TUI search, preview and import

**Files:** Adjacent `alx-tui/internal/alx/{types,client}.go`, UI search/update/actions/commands/hits/app/backend files, focused new UI and client tests, README. Do not edit `songlist.go`.

**Interfaces:** JSON mirrors Task 1; `Client.SearchPlaylists(ctx, keyword, source string, page, limit int) (PlaylistSearchResult,error)` invokes `playlist search`; `Client.InspectPlaylist(ctx, link string) (ImportedPlaylist,error)` invokes `playlist inspect`.

- [x] Write failing tests for mode selection, query/pagination, preview/back/import, warnings/errors, stale search/preview messages, and wrapper JSON decoding/CLI arguments.
- [x] Add visible song/playlist controls, playlist metadata rows and song preview reusing existing renderers/actions; restrict playlist sources to all/wy/tx and reuse import name dialog.
- [x] Run focused tests, full TUI suite and render width checks.

### Task 4: Verify, review, commit and push

- [x] Run `go test ./...`, `go vet ./...`, and `go build` in both repositories; run targeted race checks where supported.
- [x] Probe real public playlist searches and report any platform/network limitation honestly.
- [x] Independent review of both diffs and cross-repository JSON contract; fix material findings and rerun affected checks.
- [x] Stage only task files, commit each repository and push to origin; verify remote commit IDs and final worktree state.
