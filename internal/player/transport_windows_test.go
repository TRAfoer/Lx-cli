//go:build windows

package player

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"github.com/Microsoft/go-winio"
)

var testPipeCounter atomic.Uint64

func testListener(t *testing.T) (string, net.Listener) {
	t.Helper()
	endpoint := fmt.Sprintf(`\\.\pipe\alx-go-test-%d-%d`, os.Getpid(), testPipeCounter.Add(1))
	listener, err := winio.ListenPipe(endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint, listener
}
