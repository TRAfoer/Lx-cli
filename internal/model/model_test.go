package model

import "testing"

func TestSongIDIsStableAndPlatformSpecific(t *testing.T) {
	if got := SongID("wy", "123"); got != "f7ecf16f7f10680c" {
		t.Fatalf("SongID = %q", got)
	}
	if SongID("wy", "123") == SongID("kw", "123") {
		t.Fatal("platform identity lost")
	}
}
