// Package calls owns the lifecycle and desktop audio of one WhatsApp audio call.
package calls

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/purpshell/meowcaller"
)

// Audio is owned by a call and must unblock every reader/writer when closed.
type Audio interface {
	Source() meowcaller.AudioSource
	Sink() meowcaller.AudioSink
	Close() error
	Mute(bool)
	Err() error
}

type desktopAudio struct {
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	muted   atomic.Bool
	mic     chan []float32
	speaker chan []float32
	mu      sync.Mutex
	err     error
	workers sync.WaitGroup
}

func CheckAudio() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("las llamadas de audio requieren Linux con PipeWire/PulseAudio")
	}
	for _, bin := range []string{"parec", "pacat"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("falta %s (en Arch: sudo pacman -S --needed libpulse)", bin)
		}
	}
	return nil
}

// openAudio streams PCM in memory; it never records a call to disk. Small bounded
// queues discard old frames when the desktop stalls instead of accumulating delay.
func openAudio(parent context.Context) (Audio, error) {
	if err := CheckAudio(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	a := &desktopAudio{ctx: ctx, cancel: cancel, mic: make(chan []float32, 2), speaker: make(chan []float32, 2)}
	mic := exec.CommandContext(ctx, "parec", pulseArgs(true)...)
	out, err := mic.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	speaker := exec.CommandContext(ctx, "pacat", pulseArgs(false)...)
	in, err := speaker.StdinPipe()
	if err != nil {
		cancel()
		out.Close()
		return nil, err
	}
	if err = speaker.Start(); err != nil {
		cancel()
		out.Close()
		in.Close()
		return nil, fmt.Errorf("abrir salida de audio: %w", err)
	}
	if err = mic.Start(); err != nil {
		cancel()
		out.Close()
		in.Close()
		_ = speaker.Wait()
		return nil, fmt.Errorf("abrir micrófono: %w", err)
	}
	a.workers.Add(2)
	go func() {
		defer a.workers.Done()
		defer out.Close()
		buf := make([]byte, meowcaller.FrameSamples*2)
		for {
			if _, err := io.ReadFull(out, buf); err != nil {
				a.fail("micrófono", err)
				break
			}
			frame := make([]float32, meowcaller.FrameSamples)
			for i := range frame {
				frame[i] = float32(int16(binary.LittleEndian.Uint16(buf[i*2:]))) / 32768
			}
			pushLatest(a.mic, frame)
		}
		_ = mic.Wait()
	}()
	go func() {
		defer a.workers.Done()
		defer in.Close()
		defer speaker.Wait()
		buf := make([]byte, meowcaller.FrameSamples*2)
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-a.speaker:
				if len(buf) != len(frame)*2 {
					buf = make([]byte, len(frame)*2)
				}
				encodePCM(buf, frame)
				if _, err := in.Write(buf); err != nil {
					a.fail("salida de audio", err)
					return
				}
			}
		}
	}()
	return a, nil
}

func pulseArgs(record bool) []string {
	mode, stream, device := "--playback", "Llamada WhatsApp", os.Getenv("WA_CALL_SINK")
	if record {
		mode, stream, device = "--record", "Micrófono WhatsApp", os.Getenv("WA_CALL_SOURCE")
	}
	args := []string{mode, "--raw", "--format=s16le", "--rate=16000", "--channels=1", "--latency-msec=40", "--process-time-msec=20", "--client-name=wa-cli", "--stream-name=" + stream, "--property=media.role=phone"}
	if device = strings.TrimSpace(device); device != "" {
		args = append(args, "--device="+device)
	}
	return args
}

func pushLatest(q chan []float32, frame []float32) {
	select {
	case q <- frame:
		return
	default:
	}
	select {
	case <-q:
	default:
	}
	select {
	case q <- frame:
	default:
	}
}

func encodePCM(dst []byte, frame []float32) {
	for i, sample := range frame {
		if math.IsNaN(float64(sample)) {
			sample = 0
		}
		v := max(float32(-32768), min(float32(32767), sample*32768))
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(v)))
	}
}

func (a *desktopAudio) fail(device string, err error) {
	if a.ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	if a.err == nil {
		a.err = fmt.Errorf("%s: %w; comprueba los dispositivos en pavucontrol", device, err)
	}
	a.mu.Unlock()
	a.cancel()
}
func (a *desktopAudio) Err() error                     { a.mu.Lock(); defer a.mu.Unlock(); return a.err }
func (a *desktopAudio) Mute(v bool)                    { a.muted.Store(v) }
func (a *desktopAudio) Source() meowcaller.AudioSource { return a }
func (a *desktopAudio) Sink() meowcaller.AudioSink     { return a }
func (a *desktopAudio) Close() error {
	a.once.Do(func() { a.cancel(); a.workers.Wait() })
	return nil
}
func (a *desktopAudio) ReadFrame() ([]float32, error) {
	if a.ctx.Err() != nil {
		return nil, io.EOF
	}
	frame := make([]float32, meowcaller.FrameSamples)
	select {
	case frame = <-a.mic:
	default:
	}
	if a.muted.Load() {
		clear(frame)
	}
	return frame, nil
}
func (a *desktopAudio) WriteFrame(frame []float32) error {
	if a.ctx.Err() != nil {
		return io.ErrClosedPipe
	}
	pushLatest(a.speaker, append([]float32(nil), frame...))
	return nil
}
