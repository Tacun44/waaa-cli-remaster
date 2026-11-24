package calls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"github.com/purpshell/meowcaller"
	"github.com/rs/zerolog"
)

type Snapshot struct {
	Sequence   uint64
	Peer       string
	State      string
	Reason     string
	Incoming   bool
	Busy       bool
	CanAnswer  bool
	Muted      bool
	Started    time.Time
	Video      bool
	Camera     bool
	VideoError string
}

type session interface {
	Peer() string
	State() meowcaller.CallPhase
	Supported() bool
	Answer() error
	Reject() error
	Hangup() error
	OnEnd(func(string))
	OnPeerAccept(func())
	Attach(Audio)
}

type liveSession struct{ *meowcaller.Call }

func (c liveSession) Peer() string    { return c.Call.Peer().ToNonAD().String() }
func (c liveSession) Supported() bool { _, group := c.GroupState(); return !group }
func (c liveSession) Attach(a Audio)  { c.Receive(a.Sink()); c.Play(a.Source()) }

type entry struct {
	op             sync.Mutex // serialize Answer with local Hangup/Reject
	ctx            context.Context
	cancel         context.CancelFunc
	call           session
	audio          Audio
	accepted       bool
	opening        bool
	incoming       bool
	created        time.Time
	video          Video
	videoWanted    bool
	videoAttempted bool
}

// Manager serializes local state without holding locks over network or audio I/O.
// Snapshot polling keeps network callbacks independent of Bubble Tea's lifetime.
type Manager struct {
	mu            sync.Mutex
	current       *entry
	state         Snapshot
	closed        bool
	dial          func(context.Context, string) (session, error)
	dialVideo     func(context.Context, string) (session, error)
	newVideo      func(context.Context, func([]byte) error) (Video, error)
	checkVideo    func() error
	connected     func() bool
	open          func(context.Context) (Audio, error)
	check         func() error
	removeHandler func()
}

// New must run before wa.Connect: meowcaller installs the call signaling adapter.
func New(wa *whatsmeow.Client) *Manager {
	log := zerolog.New(os.Stdout).Level(zerolog.WarnLevel).With().Timestamp().Logger()
	engine := meowcaller.NewClient(wa, meowcaller.WithLogger(log))
	m := &Manager{connected: wa.IsConnected, open: openAudio, check: CheckAudio, newVideo: openVideo, checkVideo: CheckVideo}
	m.dial = func(ctx context.Context, target string) (session, error) {
		c, err := engine.Call(ctx, target)
		if err != nil {
			return nil, err
		}
		return liveSession{c}, nil
	}
	m.dialVideo = func(ctx context.Context, target string) (session, error) {
		c, err := engine.CallWithOptions(ctx, target, meowcaller.CallOptions{Video: true})
		if err != nil {
			return nil, err
		}
		return liveSession{c}, nil
	}
	engine.OnIncomingCall(func(c *meowcaller.Call) { m.receive(liveSession{c}) })
	handler := wa.AddEventHandler(func(event any) {
		switch event.(type) {
		case *events.Disconnected, *events.LoggedOut:
			go m.stop("Se perdió la conexión con WhatsApp", false)
		}
	})
	m.removeHandler = func() { wa.RemoveEventHandler(handler) }
	return m
}

func (m *Manager) Snapshot() Snapshot { m.mu.Lock(); defer m.mu.Unlock(); return m.state }

func ValidateTarget(target string) error {
	jid, err := types.ParseJID(target)
	if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
		return errors.New("selecciona un chat individual para llamar")
	}
	return nil
}

func (m *Manager) Start(target string) error {
	return m.start(target, false)
}

func (m *Manager) StartVideo(target string) error { return m.start(target, true) }

func (m *Manager) start(target string, video bool) error {
	if err := ValidateTarget(target); err != nil {
		return err
	}
	if !m.connected() {
		return errors.New("WhatsApp está desconectado")
	}
	if err := m.check(); err != nil {
		return err
	}
	if video {
		if err := m.checkVideo(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if m.closed || m.current != nil {
		m.mu.Unlock()
		return errors.New("ya hay una llamada en curso")
	}
	e := m.reserveLocked(target, false)
	m.state.Video, e.videoWanted = video, video
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(e.ctx, 25*time.Second)
	dial := m.dial
	if video {
		dial = m.dialVideo
	}
	c, err := dial(ctx, target)
	cancel()
	if err != nil {
		m.finish(e, "No se pudo llamar: "+err.Error())
		return err
	}
	m.mu.Lock()
	if m.current != e {
		m.mu.Unlock()
		_ = c.Hangup()
		return nil
	}
	e.call = c
	// The offer is on the wire, but WhatsApp has not necessarily confirmed that
	// the peer's device is ringing. Keep those two states distinct.
	m.state.State = "Solicitud enviada · esperando respuesta"
	m.mu.Unlock()
	m.observe(e, c)
	return nil
}

func (m *Manager) reserveLocked(peer string, incoming bool) *entry {
	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{ctx: ctx, cancel: cancel, incoming: incoming, created: time.Now()}
	m.current = e
	m.state = Snapshot{Sequence: m.state.Sequence + 1, Peer: peer, Busy: true, Incoming: incoming, CanAnswer: incoming, State: "Llamando…"}
	if incoming {
		m.state.State = "Llamada entrante"
	}
	return e
}

func (m *Manager) receive(c session) {
	m.mu.Lock()
	if m.closed || m.current != nil {
		m.mu.Unlock()
		go c.Reject()
		return
	}
	e := m.reserveLocked(c.Peer(), true)
	e.call = c
	if vc, ok := c.(videoSession); ok {
		m.state.Video = vc.IsVideo()
		e.videoWanted = m.state.Video
	}
	if !c.Supported() {
		m.state.CanAnswer = false
		m.state.State = "Llamada grupal: contesta desde el teléfono"
	}
	m.mu.Unlock()
	m.observe(e, c)
}

func (m *Manager) observe(e *entry, c session) {
	c.OnEnd(func(reason string) { m.finish(e, reason) })
	c.OnPeerAccept(func() {
		m.mu.Lock()
		if m.current == e {
			e.accepted = true
			m.state.State = "Conectando audio…"
		}
		m.mu.Unlock()
	})
	// Covers a termination that raced listener registration.
	if c.State() == meowcaller.CallPhaseEnded {
		m.finish(e, "Finalizada")
		return
	}
	go m.watch(e, c)
}

func (m *Manager) watch(e *entry, c session) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-tick.C:
		}
		phase := c.State()
		m.mu.Lock()
		if m.current != e {
			m.mu.Unlock()
			return
		}
		if phase == meowcaller.CallPhaseEnded {
			m.mu.Unlock()
			m.finish(e, "Finalizada")
			return
		}
		// Never open an incoming mic before an explicit Answer, or an outgoing
		// mic before the peer accepts (relay readiness may precede acceptance).
		startAudio := e.accepted && !e.opening && e.audio == nil
		if startAudio {
			e.opening = true
		}
		a := e.audio
		startVideo := e.accepted && phase == meowcaller.CallPhaseActive && e.videoWanted && !e.videoAttempted
		if startVideo {
			e.videoAttempted = true
		}
		video := e.video
		if e.accepted && phase == meowcaller.CallPhaseActive && a != nil {
			m.state.State = "En llamada"
			if m.state.Started.IsZero() {
				m.state.Started = time.Now()
			}
		} else if !e.incoming && phase == meowcaller.CallPhaseRinging && !e.accepted {
			m.state.State = "Timbrando…"
		}
		timedOut := m.state.Started.IsZero() && time.Since(e.created) > 75*time.Second
		m.mu.Unlock()
		if timedOut {
			m.finish(e, "Tiempo de espera agotado")
			go c.Hangup()
			return
		}
		if a != nil && a.Err() != nil {
			m.finish(e, a.Err().Error())
			go c.Hangup()
			return
		}
		if startAudio {
			go m.attach(e, c)
		}
		if startVideo {
			go m.attachVideo(e, c)
		}
		if video != nil && video.Err() != nil {
			m.videoFailed(e, video, video.Err())
		}
	}
}

func (m *Manager) attach(e *entry, c session) {
	a, err := m.open(e.ctx)
	if err != nil {
		m.finish(e, err.Error())
		_ = c.Hangup()
		return
	}
	m.mu.Lock()
	if m.current != e {
		m.mu.Unlock()
		a.Close()
		return
	}
	e.audio = a
	a.Mute(m.state.Muted)
	m.mu.Unlock()
	c.Attach(a)
	// If termination raced Attach, the manager still owns and closes a.
	if e.ctx.Err() != nil {
		a.Close()
	}
}

func (m *Manager) Answer() error {
	if err := m.check(); err != nil {
		return err
	}
	m.mu.Lock()
	e := m.current
	m.mu.Unlock()
	if e == nil {
		return errors.New("no hay una llamada de audio para contestar")
	}
	e.op.Lock()
	defer e.op.Unlock()
	m.mu.Lock()
	if m.current != e || e.call == nil || !e.incoming || e.accepted || !m.state.CanAnswer {
		m.mu.Unlock()
		return errors.New("no hay una llamada de audio para contestar")
	}
	video := m.state.Video
	m.mu.Unlock()
	if video {
		if err := m.checkVideo(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if m.current != e {
		m.mu.Unlock()
		return errors.New("la llamada ya terminó")
	}
	e.accepted = true
	m.state.CanAnswer = false
	m.state.State = "Conectando audio…"
	c := e.call
	m.mu.Unlock()
	if err := c.Answer(); err != nil {
		m.finish(e, err.Error())
		_ = c.Hangup()
		return err
	}
	return nil
}

func (m *Manager) ToggleMute() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.current; e != nil && e.accepted {
		m.state.Muted = !m.state.Muted
		if e.audio != nil {
			e.audio.Mute(m.state.Muted)
		}
	}
}

func (m *Manager) finish(e *entry, reason string) {
	m.mu.Lock()
	if m.current != e {
		m.mu.Unlock()
		return
	}
	m.current = nil
	m.state.Busy, m.state.CanAnswer = false, false
	m.state.State, m.state.Reason = "Llamada finalizada", reason
	e.cancel()
	a := e.audio
	v := e.video
	m.mu.Unlock()
	if a != nil {
		_ = a.Close()
	}
	if v != nil {
		_ = v.Close()
	}
}

func (m *Manager) stop(reason string, reject bool) error {
	m.mu.Lock()
	e := m.current
	var c session
	if e != nil {
		c = e.call
		reject = reject && e.incoming && !e.accepted
	}
	m.mu.Unlock()
	if e == nil {
		return nil
	}
	m.finish(e, reason)
	if c == nil {
		return nil
	}
	e.op.Lock()
	defer e.op.Unlock()
	if reject {
		return c.Reject()
	}
	return c.Hangup()
}

func (m *Manager) End() error { return m.stop("Finalizada por ti", true) }

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	if m.removeHandler != nil {
		m.removeHandler()
	}
	done := make(chan struct{})
	go func() { _ = m.End(); close(done) }()
	// Don't hold the terminal open if the socket disappeared while hanging up.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func (s Snapshot) Description() string {
	text := s.State
	if s.Video && s.Busy {
		if s.Camera {
			text += " · cámara activa"
		} else {
			text += " · cámara apagada"
		}
		if s.VideoError != "" {
			text += " · " + s.VideoError
		}
	}
	if s.Muted && s.Busy {
		text += " · micrófono silenciado"
	}
	if !s.Started.IsZero() && s.Busy {
		seconds := int(time.Since(s.Started).Seconds())
		text += fmt.Sprintf(" · %d:%02d", seconds/60, seconds%60)
	}
	if !s.Busy && s.Reason != "" {
		text += " · " + s.Reason
	}
	return text
}
