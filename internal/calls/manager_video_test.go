package calls

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purpshell/meowcaller"
)

type fakeVideoSession struct {
	*fakeSession
	sink    meowcaller.VideoSink
	enabled atomic.Bool
}

func (f *fakeVideoSession) IsVideo() bool                                     { return true }
func (f *fakeVideoSession) ReceiveVideo(s meowcaller.VideoSink)               { f.sink = s }
func (f *fakeVideoSession) SendVideoWithDuration([]byte, time.Duration) error { return nil }
func (f *fakeVideoSession) SetVideoEnabled(v bool) error                      { f.enabled.Store(v); return nil }

type fakeVideo struct {
	camera, closed atomic.Bool
	err            error
}

func (v *fakeVideo) WriteVideo([]byte) error { return nil }
func (v *fakeVideo) Camera(on bool) error    { v.camera.Store(on); return nil }
func (v *fakeVideo) CameraOn() bool          { return v.camera.Load() }
func (v *fakeVideo) Err() error              { return v.err }
func (v *fakeVideo) Close() error            { v.camera.Store(false); v.closed.Store(true); return nil }

func TestOutgoingVideoStartsCameraAfterPeerAccept(t *testing.T) {
	m, _, a := newTestManager(t)
	f := &fakeVideoSession{fakeSession: &fakeSession{phase: meowcaller.CallPhaseRinging}}
	v := &fakeVideo{}
	m.dialVideo = func(context.Context, string) (session, error) { return f, nil }
	m.checkVideo = func() error { return nil }
	m.newVideo = func(context.Context, func([]byte) error) (Video, error) { return v, nil }
	if err := m.StartVideo(f.Peer()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if v.CameraOn() || a.closed.Load() || !m.Snapshot().Video {
		t.Fatal("video opened before acceptance")
	}
	f.peerAccept()
	eventually(t, func() bool { return m.Snapshot().Camera && f.sink == v })
	if err := m.ToggleCamera(); err != nil {
		t.Fatal(err)
	}
	if v.CameraOn() || f.enabled.Load() {
		t.Fatal("camera still active")
	}
	if err := m.ToggleCamera(); err != nil {
		t.Fatal(err)
	}
	if !v.CameraOn() || !f.enabled.Load() {
		t.Fatal("camera not restored")
	}
	f.finish("ended")
	if !v.closed.Load() || !a.closed.Load() {
		t.Fatal("video/audio resources leaked")
	}
}

func TestIncomingVideoCanBeAnswered(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeVideoSession{fakeSession: &fakeSession{phase: meowcaller.CallPhaseRinging}}
	v := &fakeVideo{}
	m.checkVideo = func() error { return nil }
	m.newVideo = func(context.Context, func([]byte) error) (Video, error) { return v, nil }
	m.receive(f)
	if !m.Snapshot().CanAnswer || !m.Snapshot().Video || v.CameraOn() {
		t.Fatal("incoming video state wrong")
	}
	if err := m.Answer(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return m.Snapshot().Camera })
	if f.answered.Load() != 1 {
		t.Fatal("video call not answered")
	}
}

func TestVideoFailureKeepsAudioCall(t *testing.T) {
	m, _, a := newTestManager(t)
	f := &fakeVideoSession{fakeSession: &fakeSession{phase: meowcaller.CallPhaseRinging}}
	m.dialVideo = func(context.Context, string) (session, error) { return f, nil }
	m.checkVideo = func() error { return nil }
	m.newVideo = func(context.Context, func([]byte) error) (Video, error) { return nil, errors.New("camera offline") }
	if err := m.StartVideo(f.Peer()); err != nil {
		t.Fatal(err)
	}
	f.peerAccept()
	eventually(t, func() bool { return m.Snapshot().VideoError != "" })
	if !m.Snapshot().Busy || a.closed.Load() {
		t.Fatal("video failure ended audio")
	}
}
