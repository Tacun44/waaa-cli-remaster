package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/purpshell/meowcaller"
)

type Video interface {
	meowcaller.VideoSink
	Camera(bool) error
	CameraOn() bool
	Err() error
}

// frameQueue bounds latency without feeding broken inter-frame dependencies to
// the decoder after an overrun. Resume at the next IDR, with cached SPS/PPS.
type frameQueue struct {
	mu       sync.Mutex
	frames   chan []byte
	waitKey  bool
	sps, pps []byte
}

func newFrameQueue() *frameQueue { return &frameQueue{frames: make(chan []byte, 3), waitKey: true} }
func (q *frameQueue) offer(frame []byte) {
	if len(frame) == 0 || len(frame) > maxVideoFrame {
		return
	}
	idr, sps, pps := videoSummary(frame)
	q.mu.Lock()
	defer q.mu.Unlock()
	if sps != nil {
		q.sps = sps
	}
	if pps != nil {
		q.pps = pps
	}
	if len(q.frames) == cap(q.frames) {
		for len(q.frames) > 0 {
			select {
			case <-q.frames:
			default:
			}
		}
		q.waitKey = true
	}
	if q.waitKey && !idr {
		return
	}
	packet := make([]byte, 0, len(frame)+len(q.sps)+len(q.pps))
	if q.waitKey {
		if sps == nil {
			packet = append(packet, q.sps...)
		}
		if pps == nil {
			packet = append(packet, q.pps...)
		}
	}
	packet = append(packet, frame...)
	q.frames <- packet
	q.waitKey = false
}

type tailLog struct {
	mu   sync.Mutex
	data []byte
}

func (l *tailLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.data = append(l.data, p...)
	if len(l.data) > 4096 {
		l.data = append([]byte(nil), l.data[len(l.data)-4096:]...)
	}
	return len(p), nil
}
func (l *tailLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(string(l.data))
}

type desktopVideo struct {
	ctx          context.Context
	cancel       context.CancelFunc
	once         sync.Once
	incoming     *frameQueue
	outgoing     *frameQueue
	mu           sync.Mutex
	err          error
	cameraMu     sync.Mutex
	cameraCancel context.CancelFunc
	cameraDone   chan struct{}
	cameraOn     atomic.Bool
	orientation  atomic.Int32
	viewerDone   chan struct{}
	writerDone   chan struct{}
	socketDir    string
}

func cameraDevice() string {
	if path := strings.TrimSpace(os.Getenv("WA_CALL_CAMERA")); path != "" {
		return path
	}
	return "/dev/video0"
}

func CheckVideo() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("el video requiere Linux con una cámara V4L2")
	}
	for _, bin := range []string{"ffmpeg", "mpv"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("falta %s (en Arch: sudo pacman -S --needed ffmpeg mpv)", bin)
		}
	}
	if _, err := os.Stat(cameraDevice()); err != nil {
		return fmt.Errorf("cámara %s: %w", cameraDevice(), err)
	}
	return nil
}

func cameraArgs() []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-f", "v4l2", "-framerate", "15", "-video_size", "640x480", "-i", cameraDevice()}
	return append(args, encoderArgs()...)
}

func encoderArgs() []string {
	return []string{"-an", "-vf", "fps=15,format=yuv420p", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "baseline", "-level:v", "3.0",
		"-b:v", "500k", "-maxrate", "600k", "-bufsize", "600k", "-g", "15", "-keyint_min", "15", "-sc_threshold", "0", "-bf", "0",
		"-x264-params", "aud=1:repeat-headers=1", "-f", "h264", "pipe:1"}
}

func viewerArgs(socket string) []string {
	return []string{"--no-config", "--really-quiet", "--profile=low-latency", "--no-audio", "--cache=no", "--untimed", "--demuxer-lavf-format=h264",
		"--force-window=immediate", "--title=wa-cli · Videollamada", "--autofit=640x480", "--input-ipc-server=" + socket, "-"}
}

// openVideo opens only the receiver. Camera(true) is separate and is called only
// for a video call explicitly accepted/started by the user. No media is saved.
func openVideo(parent context.Context, send func([]byte) error) (Video, error) {
	return openVideoWithViewer(parent, send, viewerArgs)
}

func openVideoWithViewer(parent context.Context, send func([]byte) error, args func(string) []string) (Video, error) {
	ctx, cancel := context.WithCancel(parent)
	dir, err := os.MkdirTemp("", "wa-call-video-")
	if err != nil {
		cancel()
		return nil, err
	}
	v := &desktopVideo{ctx: ctx, cancel: cancel, incoming: newFrameQueue(), outgoing: newFrameQueue(), socketDir: dir, viewerDone: make(chan struct{}), writerDone: make(chan struct{})}
	socket := filepath.Join(dir, "mpv.sock")
	cmd := exec.CommandContext(ctx, "mpv", args(socket)...)
	var stderr tailLog
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("abrir video: %w", err)
	}
	go func() {
		defer close(v.viewerDone)
		err := cmd.Wait()
		if ctx.Err() == nil {
			if err == nil {
				v.fail(fmt.Errorf("ventana de video cerrada; el audio continúa"))
			} else {
				v.fail(fmt.Errorf("visor de video: %v %s", err, stderr.String()))
			}
		}
	}()
	go func() {
		defer close(v.writerDone)
		defer stdin.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-v.incoming.frames:
				if _, err := stdin.Write(frame); err != nil {
					if ctx.Err() == nil {
						v.fail(fmt.Errorf("reproducir video: %w", err))
					}
					return
				}
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-v.outgoing.frames:
				if !v.cameraOn.Load() {
					continue
				}
				if err := send(frame); err != nil && ctx.Err() == nil {
					v.fail(fmt.Errorf("enviar video: %w", err))
					return
				}
			}
		}
	}()
	go v.rotateViewer(socket)
	return v, nil
}

func (v *desktopVideo) fail(err error) {
	v.mu.Lock()
	if v.err == nil {
		v.err = err
	}
	v.mu.Unlock()
	v.cancel()
}
func (v *desktopVideo) Err() error     { v.mu.Lock(); defer v.mu.Unlock(); return v.err }
func (v *desktopVideo) CameraOn() bool { return v.cameraOn.Load() }
func (v *desktopVideo) WriteVideo(frame []byte) error {
	if v.ctx.Err() != nil {
		return io.ErrClosedPipe
	}
	v.incoming.offer(frame)
	return nil
}
func (v *desktopVideo) SetOrientation(orientation int) {
	v.orientation.Store(int32((orientation%4 + 4) % 4))
}
func (v *desktopVideo) rotateViewer(socket string) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	previous := int32(-1)
	for {
		select {
		case <-v.ctx.Done():
			return
		case <-tick.C:
		}
		orientation := v.orientation.Load()
		if orientation == previous {
			continue
		}
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err != nil {
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		err = json.NewEncoder(conn).Encode(map[string]any{"command": []any{"set_property", "video-rotate", int(orientation) * 90}})
		conn.Close()
		if err == nil {
			previous = orientation
		}
	}
}

func (v *desktopVideo) Camera(enabled bool) error {
	v.cameraMu.Lock()
	defer v.cameraMu.Unlock()
	if !enabled {
		v.cameraOn.Store(false)
		if v.cameraCancel != nil {
			v.cameraCancel()
			<-v.cameraDone
			v.cameraCancel = nil
		}
		// Don't send stale captured frames when the camera is enabled again.
		for len(v.outgoing.frames) > 0 {
			select {
			case <-v.outgoing.frames:
			default:
			}
		}
		return nil
	}
	if v.ctx.Err() != nil {
		return io.ErrClosedPipe
	}
	if v.cameraOn.Load() {
		return nil
	}
	ctx, cancel := context.WithCancel(v.ctx)
	cmd := exec.CommandContext(ctx, "ffmpeg", cameraArgs()...)
	var stderr tailLog
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		stdout.Close()
		return fmt.Errorf("abrir cámara: %w", err)
	}
	done := make(chan struct{})
	v.cameraCancel, v.cameraDone = cancel, done
	v.cameraOn.Store(true)
	go func() {
		defer close(done)
		err := readAccessUnits(stdout, func(frame []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			v.outgoing.offer(frame)
			return nil
		})
		stdout.Close()
		// A parser error must not leave ffmpeg blocked on a full stdout pipe.
		unexpected := ctx.Err() == nil
		cancel()
		waitErr := cmd.Wait()
		v.cameraOn.Store(false)
		if unexpected {
			if err == nil {
				err = waitErr
			}
			if err == nil {
				err = io.EOF
			}
			v.fail(fmt.Errorf("cámara: %v %s", err, stderr.String()))
		}
	}()
	return nil
}

func (v *desktopVideo) Close() error {
	v.once.Do(func() {
		v.cancel()
		_ = v.Camera(false)
		<-v.viewerDone
		<-v.writerDone
		_ = os.RemoveAll(v.socketDir)
	})
	return nil
}
