//go:build !windows

package player

import (
	"net"
	"path/filepath"
	"testing"
)

func testListener(t *testing.T) (string, net.Listener) {
	t.Helper()
	endpoint := filepath.Join(t.TempDir(), "mpv.sock")
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint, listener
}
