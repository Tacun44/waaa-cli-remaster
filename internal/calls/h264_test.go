package calls

import (
	"bytes"
	"io"
	"os/exec"
	"testing"
)

func TestAnnexBAccessUnitsAcrossShortReads(t *testing.T) {
	// Two pictures with mixed 3/4-byte start codes. A reader that returns
	// one byte at a time checks boundaries spanning arbitrary pipe reads.
	stream := []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 1, 7, 0x64, 0, 0, 0, 1, 8, 0xee, 0, 0, 1, 5, 0xa1,
		0, 0, 0, 1, 9, 0xf0, 0, 0, 1, 1, 0xb2}
	var units [][]byte
	err := readAccessUnits(&slowReader{data: stream}, func(frame []byte) error { units = append(units, bytes.Clone(frame)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Fatalf("got %d pictures", len(units))
	}
	idr, sps, pps := videoSummary(units[0])
	if !idr || len(sps) == 0 || len(pps) == 0 {
		t.Fatal("first picture lost codec headers")
	}
	if other, _, _ := videoSummary(units[1]); other {
		t.Fatal("second picture incorrectly marked keyframe")
	}
}

type slowReader struct{ data []byte }

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestFrameQueueResumesAtKeyframeAfterOverflow(t *testing.T) {
	q := newFrameQueue()
	sps := []byte{0, 0, 0, 1, 7, 0x64}
	pps := []byte{0, 0, 0, 1, 8, 0xee}
	idr := []byte{0, 0, 0, 1, 5, 0x01}
	delta := []byte{0, 0, 0, 1, 1, 0x02}
	q.offer(append(append(bytes.Clone(sps), pps...), idr...))
	for i := 0; i < 10; i++ {
		q.offer(delta)
	}
	if len(q.frames) != 0 {
		t.Fatal("dependent frames retained after overflow")
	}
	q.offer(delta)
	if len(q.frames) != 0 {
		t.Fatal("accepted non-keyframe after overflow")
	}
	q.offer(idr)
	if len(q.frames) != 1 {
		t.Fatal("did not resume at keyframe")
	}
	frame := <-q.frames
	if !bytes.HasPrefix(frame, sps) || !bytes.Contains(frame, pps) {
		t.Fatal("recovery lost SPS/PPS")
	}
}

func TestFFmpegEncoderProducesCompleteAccessUnits(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	args := append([]string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=640x480:rate=15", "-frames:v", "20"}, encoderArgs()...)
	out, err := exec.Command("ffmpeg", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	var frames int
	err = readAccessUnits(bytes.NewReader(out), func(au []byte) error {
		frames++
		if frames == 1 {
			idr, sps, pps := videoSummary(au)
			if !idr || len(sps) == 0 || len(pps) == 0 {
				t.Fatal("encoder first frame lacks IDR/SPS/PPS")
			}
		}
		return nil
	})
	if err != nil || frames != 20 {
		t.Fatalf("frames=%d err=%v", frames, err)
	}
}
