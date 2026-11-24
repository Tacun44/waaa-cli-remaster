package calls

import (
	"errors"
	"time"

	"github.com/purpshell/meowcaller"
)

// Optional so audio-only sessions and their tests keep their original contract.
type videoSession interface {
	IsVideo() bool
	ReceiveVideo(meowcaller.VideoSink)
	SendVideoWithDuration([]byte, time.Duration) error
	SetVideoEnabled(bool) error
}

func (m *Manager) attachVideo(e *entry, c session) {
	e.op.Lock()
	defer e.op.Unlock()
	m.mu.Lock()
	valid := m.current == e && e.videoWanted
	m.mu.Unlock()
	if !valid {
		return
	}
	vc, ok := c.(videoSession)
	if !ok {
		m.videoFailed(e, nil, errors.New("esta llamada no admite video"))
		return
	}
	v, err := m.newVideo(e.ctx, func(frame []byte) error { return vc.SendVideoWithDuration(frame, time.Second/15) })
	if err != nil {
		m.videoFailed(e, nil, err)
		return
	}
	m.mu.Lock()
	if m.current != e {
		m.mu.Unlock()
		v.Close()
		return
	}
	e.video = v
	m.mu.Unlock()
	vc.ReceiveVideo(v)
	if err = v.Camera(true); err != nil {
		m.videoFailed(e, v, err)
		return
	}
	m.mu.Lock()
	if m.current == e && e.video == v && e.videoWanted {
		m.state.Camera = true
		m.state.VideoError = ""
	}
	m.mu.Unlock()
	if e.ctx.Err() != nil {
		v.Close()
	}
}

func (m *Manager) videoFailed(e *entry, expected Video, err error) {
	m.mu.Lock()
	if m.current != e || (expected != nil && e.video != expected) {
		m.mu.Unlock()
		return
	}
	v := e.video
	e.video = nil
	e.videoWanted = false
	m.state.Camera = false
	m.state.VideoError = err.Error()
	c := e.call
	m.mu.Unlock()
	if v != nil {
		v.Close()
	}
	if vc, ok := c.(videoSession); ok {
		go vc.SetVideoEnabled(false)
	}
}

// ToggleCamera never turns an audio call into video implicitly. Starting video
// is an explicit choice in the idle call panel; mid-call upgrades are deferred.
func (m *Manager) ToggleCamera() error {
	m.mu.Lock()
	e := m.current
	if e == nil || !m.state.Video || !e.accepted {
		m.mu.Unlock()
		return errors.New("inicia una videollamada desde el panel para usar la cámara")
	}
	m.mu.Unlock()
	e.op.Lock()
	defer e.op.Unlock()
	m.mu.Lock()
	if m.current != e {
		m.mu.Unlock()
		return errors.New("la llamada ya terminó")
	}
	vc, ok := e.call.(videoSession)
	if !ok {
		m.mu.Unlock()
		return errors.New("video no disponible")
	}
	v := e.video
	enable := !e.videoWanted
	if v == nil && enable {
		e.videoWanted = true
		e.videoAttempted = false
		m.state.VideoError = ""
		m.mu.Unlock()
		if err := vc.SetVideoEnabled(true); err != nil {
			m.videoFailed(e, nil, err)
			return err
		}
		return nil
	}
	e.videoWanted = enable
	m.mu.Unlock()
	if v == nil {
		if !enable {
			return vc.SetVideoEnabled(false)
		}
		return nil
	} // An initial camera start can be cancelled before it opens.
	if !enable {
		_ = v.Camera(false) // Stop capture even if the network signal fails.
		err := vc.SetVideoEnabled(false)
		m.mu.Lock()
		if m.current == e {
			m.state.Camera = false
		}
		m.mu.Unlock()
		return err
	}
	if err := vc.SetVideoEnabled(true); err != nil {
		m.videoFailed(e, v, err)
		return err
	}
	if err := v.Camera(true); err != nil {
		m.videoFailed(e, v, err)
		return err
	}
	m.mu.Lock()
	if m.current == e && e.video == v && e.videoWanted {
		m.state.Camera = true
	}
	m.mu.Unlock()
	return nil
}
