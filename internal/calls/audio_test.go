package calls

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in: opens the real default mic briefly, discards its samples,
// and sends silence to the speaker. No network and no recording files.
func TestDesktopAudioSmoke(t *testing.T) {
	if os.Getenv("WA_CALL_AUDIO_SMOKE") != "1" {
		t.Skip("set WA_CALL_AUDIO_SMOKE=1 to test desktop audio")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	audio, err := openAudio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := audio.(*desktopAudio)
	defer a.Close()
	select {
	case <-a.mic:
	case <-ctx.Done():
		t.Fatal("no microphone frames received", a.Err())
	}
	if err := a.WriteFrame(make([]float32, 960)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := a.Err(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedAudioDropsOldFrames(t *testing.T) {
	q := make(chan []float32, 2)
	for i := 0; i < 100; i++ {
		pushLatest(q, []float32{float32(i)})
	}
	if len(q) != 2 || (<-q)[0] != 98 || (<-q)[0] != 99 {
		t.Fatal("audio accumulated a backlog")
	}
}
func TestPCMClipsAndHandlesNaN(t *testing.T) {
	frame := []float32{-2, -1, 0, 0.5, 1, 2, float32(math.NaN())}
	want := []int16{-32768, -32768, 0, 16384, 32767, 32767, 0}
	buf := make([]byte, len(frame)*2)
	encodePCM(buf, frame)
	for i, value := range want {
		if int16(binary.LittleEndian.Uint16(buf[i*2:])) != value {
			t.Fatalf("sample %d", i)
		}
	}
}
func TestMutedSourceDrainsWithoutTransmitting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &desktopAudio{ctx: ctx, cancel: cancel, mic: make(chan []float32, 2), speaker: make(chan []float32, 2)}
	a.mic <- []float32{1, 1}
	a.Mute(true)
	frame, err := a.ReadFrame()
	if err != nil || frame[0] != 0 || len(a.mic) != 0 {
		t.Fatal("muted source leaked or retained audio")
	}
	a.Close()
	if _, err = a.ReadFrame(); err == nil {
		t.Fatal("closed source still readable")
	}
	if a.WriteFrame(frame) == nil {
		t.Fatal("closed sink still writable")
	}
}
func TestAudioDeviceSelectionPassedAsSingleArgument(t *testing.T) {
	t.Setenv("WA_CALL_SOURCE", "mic with spaces")
	args := pulseArgs(true)
	if args[len(args)-1] != "--device=mic with spaces" {
		t.Fatal(args)
	}
	if !strings.Contains(strings.Join(args, " "), "--rate=16000") {
		t.Fatal(args)
	}
}
