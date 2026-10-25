package player

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

type wireRequest struct {
	Command []json.RawMessage `json:"command"`
	ID      uint64            `json:"request_id"`
}

func protocolServer(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	endpoint, listener := testListener(t)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		serve(conn)
	}()
	return endpoint
}

func readRequest(conn net.Conn) (wireRequest, error) {
	var req wireRequest
	err := json.NewDecoder(conn).Decode(&req)
	return req, err
}

func TestCommandMatchesResponseAndPublishesEvents(t *testing.T) {
	endpoint := protocolServer(t, func(conn net.Conn) {
		req, err := readRequest(conn)
		if err != nil {
			return
		}
		fmt.Fprintf(conn, "{\"event\":\"end-file\",\"reason\":\"eof\",\"playlist_entry_id\":7}\n")
		fmt.Fprintf(conn, "{\"request_id\":%d,\"error\":\"success\",\"data\":\"wrong\"}\n", req.ID+100)
		fmt.Fprintf(conn, "{\"request_id\":%d,\"error\":\"success\",\"data\":12.5}\n", req.ID)
	})
	client := New(endpoint, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err := client.Command(ctx, "get_property", "time-pos")
	if err != nil || string(data) != "12.5" {
		t.Fatalf("data=%s err=%v", data, err)
	}
	select {
	case event := <-client.Events():
		if event.Event != "end-file" || event.Reason != "eof" || event.PlaylistEntryID != 7 {
			t.Fatalf("event=%+v", event)
		}
	case <-ctx.Done():
		t.Fatal("missing end-file event")
	}
}

func TestCommandsShareConnectionAndMatchReversedReplies(t *testing.T) {
	endpoint := protocolServer(t, func(conn net.Conn) {
		decoder := json.NewDecoder(conn)
		var requests [2]wireRequest
		for i := range requests {
			if decoder.Decode(&requests[i]) != nil {
				return
			}
		}
		for i := len(requests) - 1; i >= 0; i-- {
			fmt.Fprintf(conn, "{\"request_id\":%d,\"error\":\"success\",\"data\":%s}\n", requests[i].ID, requests[i].Command[1])
		}
	})
	client := New(endpoint, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errs := make(chan error, 2)
	for _, property := range []string{"pause", "volume"} {
		go func(property string) {
			data, err := client.Command(ctx, "get_property", property)
			if err == nil && string(data) != fmt.Sprintf("%q", property) {
				err = fmt.Errorf("%s received %s", property, data)
			}
			errs <- err
		}(property)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommandReportsMPVError(t *testing.T) {
	endpoint := protocolServer(t, func(conn net.Conn) {
		req, err := readRequest(conn)
		if err != nil {
			return
		}
		fmt.Fprintf(conn, "{\"request_id\":%d,\"error\":\"property unavailable\"}\n", req.ID)
	})
	_, err := New(endpoint, nil).Command(context.Background(), "get_property", "missing")
	if err == nil || !strings.Contains(err.Error(), "property unavailable") {
		t.Fatalf("err=%v", err)
	}
}

func TestCommandCancellationDoesNotDiscardLaterResponses(t *testing.T) {
	endpoint := protocolServer(t, func(conn net.Conn) {
		decoder := json.NewDecoder(conn)
		var first, second wireRequest
		if decoder.Decode(&first) != nil || decoder.Decode(&second) != nil {
			return
		}
		fmt.Fprintf(conn, "{\"request_id\":%d,\"error\":\"success\",\"data\":false}\n", first.ID)
		fmt.Fprintf(conn, "{\"request_id\":%d,\"error\":\"success\",\"data\":true}\n", second.ID)
	})
	client := New(endpoint, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	_, err := client.Command(ctx, "get_property", "pause")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	data, err := client.Command(ctx2, "get_property", "pause")
	if err != nil || string(data) != "true" {
		t.Fatalf("data=%s err=%v", data, err)
	}
}

func TestCommandReturnsWhenIPCDisconnects(t *testing.T) {
	endpoint := protocolServer(t, func(conn net.Conn) { _, _ = readRequest(conn) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := New(endpoint, nil).Command(ctx, "get_property", "pause")
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestCommandRejectsMalformedResponse(t *testing.T) {
	endpoint := protocolServer(t, func(conn net.Conn) {
		_, _ = readRequest(conn)
		fmt.Fprintln(conn, "{invalid json}")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := New(endpoint, nil).Command(ctx, "get_property", "pause")
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestCommandDoesNotStartMPV(t *testing.T) {
	t.Setenv("ALX_MPV", "alx-player-must-not-launch-mpv")
	endpoint, listener := testListener(t)
	_ = listener.Close()
	client := New(endpoint, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.Command(ctx, "get_property", "pause"); err == nil {
		t.Fatal("missing endpoint must fail")
	}
	if client.Running(ctx) {
		t.Fatal("absent endpoint reported running")
	}
}

func TestCanceledCommandDoesNotWrite(t *testing.T) {
	endpoint := protocolServer(t, func(conn net.Conn) { _, _ = bufio.NewReader(conn).ReadString('\n') })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(endpoint, nil).Command(ctx, "get_property", "pause")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestEnsureUsesExistingEndpointWithoutLaunching(t *testing.T) {
	t.Setenv("ALX_MPV", "alx-player-must-not-launch-mpv")
	endpoint := protocolServer(t, func(conn net.Conn) {
		req, err := readRequest(conn)
		if err != nil {
			return
		}
		fmt.Fprintf(conn, "{\"request_id\":%d,\"error\":\"success\",\"data\":\"mpv-test\"}\n", req.ID)
	})
	client := New(endpoint, nil)
	if err := client.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !client.Running(context.Background()) {
		t.Fatal("existing endpoint should answer query")
	}
}

func TestCloseReportsRejectedQuit(t *testing.T) {
	endpoint := protocolServer(t, func(conn net.Conn) {
		req, err := readRequest(conn)
		if err != nil {
			return
		}
		fmt.Fprintf(conn, "{\"request_id\":%d,\"error\":\"command rejected\"}\n", req.ID)
	})
	err := New(endpoint, nil).Close(context.Background())
	if err == nil || !strings.Contains(err.Error(), "command rejected") {
		t.Fatalf("err=%v", err)
	}
}

func TestEnsureReportsMissingExecutable(t *testing.T) {
	t.Setenv("ALX_MPV", "alx-player-must-not-launch-mpv")
	endpoint, listener := testListener(t)
	_ = listener.Close()
	if err := New(endpoint, nil).Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "find mpv") {
		t.Fatalf("err=%v", err)
	}
}
