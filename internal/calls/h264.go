package calls

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

const maxVideoFrame = 2 << 20

// splitNAL accepts both Annex-B start code lengths, including codes split over
// pipe reads. Camera ffmpeg always emits AUDs, so each access unit is one frame.
func splitNAL(data []byte, eof bool) (int, []byte, error) {
	start, prefix := startCode(data, 0)
	if start < 0 {
		if eof && len(bytes.Trim(data, "\x00")) != 0 {
			return 0, nil, fmt.Errorf("video sin cabecera Annex-B")
		}
		return 0, nil, nil
	}
	if start > 0 {
		return start, nil, nil
	}
	next, _ := startCode(data, prefix)
	if next >= 0 {
		return next, data[:next], nil
	}
	if eof {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func startCode(data []byte, from int) (int, int) {
	for i := from; i+2 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 {
			continue
		}
		if data[i+2] == 1 {
			return i, 3
		}
		if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			return i, 4
		}
	}
	return -1, 0
}

func nalType(nal []byte) byte {
	i, n := startCode(nal, 0)
	if i < 0 || i+n >= len(nal) {
		return 0
	}
	return nal[i+n] & 31
}

func readAccessUnits(r io.Reader, send func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxVideoFrame)
	scanner.Split(splitNAL)
	var frame []byte
	hasPicture := false
	for scanner.Scan() {
		nal := scanner.Bytes()
		kind := nalType(nal)
		if kind == 9 && hasPicture {
			if err := send(frame); err != nil {
				return err
			}
			frame = nil
			hasPicture = false
		}
		if len(frame)+len(nal) > maxVideoFrame {
			return fmt.Errorf("fotograma H.264 demasiado grande")
		}
		frame = append(frame, nal...)
		hasPicture = hasPicture || kind == 1 || kind == 5
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if hasPicture {
		return send(frame)
	}
	return nil
}

// summary is used when playback falls behind: restart decoding only at an IDR
// and retain SPS/PPS, instead of feeding dependent P frames after dropping data.
func videoSummary(frame []byte) (idr bool, sps, pps []byte) {
	for start, prefix := startCode(frame, 0); start >= 0; {
		end, _ := startCode(frame, start+prefix)
		if end < 0 {
			end = len(frame)
		}
		nal := frame[start:end]
		switch nalType(nal) {
		case 5:
			idr = true
		case 7:
			sps = bytes.Clone(nal)
		case 8:
			pps = bytes.Clone(nal)
		}
		if end == len(frame) {
			break
		}
		start, prefix = startCode(frame, end)
	}
	return
}
