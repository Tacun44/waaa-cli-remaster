package calls

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in: loops real camera H.264 back into a headless mpv decoder. No call,
// no window, no screenshot and no saved video.
func TestDesktopVideoSmoke(t *testing.T) {
	if os.Getenv("WA_CALL_VIDEO_SMOKE") != "1" {
		t.Skip("set WA_CALL_VIDEO_SMOKE=1 for local camera/decoder smoke test")
	}
	if err := CheckVideo(); err != nil {
		t.Fatal(err)
	}
	for _, bin := range []string{"ffmpeg", "mpv"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var v Video
	var sent atomic.Int32
	var err error
	v, err = openVideoWithViewer(ctx, func(frame []byte) error {
		sent.Add(1)
		return v.WriteVideo(frame)
	}, func(socket string) []string {
		args := viewerArgs(socket)
		return append(args[:len(args)-1], append([]string{"--vo=null", "--force-window=no"}, args[len(args)-1])...)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.Camera(true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if sent.Load() >= 5 {
			break
		}
		if err := v.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sent.Load() < 5 {
		t.Fatal(fmt.Errorf("only %d camera frames; check %s and its permissions", sent.Load(), cameraDevice()))
	}
	// Reading decoder properties proves mpv recognized the H.264 stream,
	// beyond merely accepting bytes on stdin.
	decoded := false
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", filepath.Join(v.(*desktopVideo).socketDir, "mpv.sock"), 100*time.Millisecond)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
			_ = json.NewEncoder(conn).Encode(map[string]any{"command": []string{"get_property", "video-params"}, "request_id": 99})
			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				var response struct {
					RequestID int    `json:"request_id"`
					Error     string `json:"error"`
					Data      struct {
						W int `json:"w"`
						H int `json:"h"`
					} `json:"data"`
				}
				if json.Unmarshal(scanner.Bytes(), &response) == nil && response.RequestID == 99 && response.Error == "success" && response.Data.W == 640 && response.Data.H == 480 {
					decoded = true
					break
				}
			}
			conn.Close()
		}
		if decoded {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !decoded {
		t.Fatal("mpv did not recognize a 640x480 H.264 stream")
	}
	conn, err := net.DialTimeout("unix", filepath.Join(v.(*desktopVideo).socketDir, "mpv.sock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewEncoder(conn).Encode(map[string]any{"command": []string{"quit"}})
	conn.Close()
	for time.Now().Before(deadline) {
		if err := v.Err(); err != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := v.Err(); err == nil || !strings.Contains(err.Error(), "ventana de video cerrada") {
		t.Fatalf("closing mpv did not report a recoverable video error: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if v.CameraOn() {
		t.Fatal("camera still running after Close")
	}
}
