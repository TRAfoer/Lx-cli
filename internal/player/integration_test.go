package player

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt in so normal tests do not require an installed audio player.
func TestMPVLocalAudioLifecycle(t *testing.T) {
	if os.Getenv("ALX_MPV_TEST") != "1" {
		t.Skip("set ALX_MPV_TEST=1 for isolated mpv verification")
	}
	endpoint, listener := testListener(t)
	_ = listener.Close()
	path := filepath.Join(t.TempDir(), "local-test.wav")
	if err := os.WriteFile(path, testWAV(3), 0600); err != nil {
		t.Fatal(err)
	}
	client := New(endpoint, []string{"--volume=0", "--ao=null", "--input-ipc-server=wrong-endpoint", "--idle=no", "--force-window=yes"})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := client.Close(ctx); err != nil {
			t.Errorf("cleanup mpv: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := client.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("isolated mpv endpoint: %s", endpoint)
	if !client.Running(ctx) {
		t.Fatal("started mpv is not running")
	}
	if _, err := client.Command(ctx, "loadfile", path, "replace"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ctx, client, "file-loaded", "")
	if _, err := client.Command(ctx, "set_property", "pause", true); err != nil {
		t.Fatal(err)
	}
	data, err := client.Command(ctx, "get_property", "pause")
	if err != nil || string(data) != "true" {
		t.Fatalf("pause=%s err=%v", data, err)
	}
	if _, err := client.Command(ctx, "seek", 1.0, "absolute+exact"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		data, err = client.Command(ctx, "get_property", "time-pos")
		var position float64
		if err == nil && json.Unmarshal(data, &position) == nil && position >= 0.9 && position <= 1.2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seek time-pos=%s err=%v", data, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := client.Command(ctx, "set_property", "pause", false); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Command(ctx, "seek", 2.8, "absolute+exact"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ctx, client, "end-file", "eof")
	if _, err := client.Command(ctx, "get_property", "idle-active"); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if client.Running(ctx) {
		t.Fatal("mpv still running after quit")
	}
	t.Log("load, pause, seek, resume, EOF, quit and IPC cleanup verified")
}

func waitEvent(t *testing.T, ctx context.Context, client *Client, name, reason string) {
	t.Helper()
	for {
		select {
		case event := <-client.Events():
			if event.Event == name && (reason == "" || event.Reason == reason) {
				return
			}
		case <-ctx.Done():
			t.Fatalf("waiting for %s/%s: %v", name, reason, ctx.Err())
		}
	}
}

func testWAV(seconds int) []byte {
	const sampleRate = 8000
	const bytesPerSample = 2
	count := sampleRate * seconds
	wav := make([]byte, 44+count*bytesPerSample)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], sampleRate)
	binary.LittleEndian.PutUint32(wav[28:], sampleRate*bytesPerSample)
	binary.LittleEndian.PutUint16(wav[32:], bytesPerSample)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(count*bytesPerSample))
	for i := 0; i < count; i++ {
		binary.LittleEndian.PutUint16(wav[44+i*2:], uint16(int16(2000*math.Sin(2*math.Pi*440*float64(i)/sampleRate))))
	}
	return wav
}
