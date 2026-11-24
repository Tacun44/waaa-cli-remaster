package calls

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purpshell/meowcaller"
)

type fakeSession struct {
	mu                                   sync.Mutex
	phase                                meowcaller.CallPhase
	end                                  func(string)
	accept                               func()
	answered, rejected, hungup, attached atomic.Int32
}

func (f *fakeSession) Peer() string                { return "123@s.whatsapp.net" }
func (f *fakeSession) Supported() bool             { return true }
func (f *fakeSession) State() meowcaller.CallPhase { f.mu.Lock(); defer f.mu.Unlock(); return f.phase }
func (f *fakeSession) OnEnd(fn func(string))       { f.mu.Lock(); f.end = fn; f.mu.Unlock() }
func (f *fakeSession) OnPeerAccept(fn func())      { f.mu.Lock(); f.accept = fn; f.mu.Unlock() }
func (f *fakeSession) Answer() error {
	f.answered.Add(1)
	f.mu.Lock()
	f.phase = meowcaller.CallPhaseActive
	f.mu.Unlock()
	return nil
}
func (f *fakeSession) Reject() error { f.rejected.Add(1); f.finish("rejected"); return nil }
func (f *fakeSession) Hangup() error { f.hungup.Add(1); f.finish("hangup"); return nil }
func (f *fakeSession) Attach(Audio)  { f.attached.Add(1) }
func (f *fakeSession) finish(reason string) {
	f.mu.Lock()
	f.phase = meowcaller.CallPhaseEnded
	fn := f.end
	f.mu.Unlock()
	if fn != nil {
		fn(reason)
	}
}
func (f *fakeSession) peerAccept() {
	f.mu.Lock()
	f.phase = meowcaller.CallPhaseActive
	fn := f.accept
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
}

type fakeAudio struct{ closed, muted atomic.Bool }

func (a *fakeAudio) Source() meowcaller.AudioSource { return nil }
func (a *fakeAudio) Sink() meowcaller.AudioSink     { return nil }
func (a *fakeAudio) Close() error                   { a.closed.Store(true); return nil }
func (a *fakeAudio) Mute(v bool)                    { a.muted.Store(v) }
func (a *fakeAudio) Err() error                     { return nil }

func newTestManager(t *testing.T) (*Manager, *fakeSession, *fakeAudio) {
	t.Helper()
	f, a := &fakeSession{phase: meowcaller.CallPhaseRinging}, &fakeAudio{}
	m := &Manager{
		connected: func() bool { return true }, check: func() error { return nil },
		dial: func(context.Context, string) (session, error) { return f, nil },
		open: func(context.Context) (Audio, error) { return a, nil },
	}
	t.Cleanup(m.Close)
	return m, f, a
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestIncomingMicrophoneRequiresAnswerAndClosesOnRemoteHangup(t *testing.T) {
	m, f, a := newTestManager(t)
	m.receive(f)
	time.Sleep(150 * time.Millisecond)
	if f.attached.Load() != 0 {
		t.Fatal("microphone opened before answering")
	}
	if !m.Snapshot().CanAnswer {
		t.Fatal("incoming call cannot be answered")
	}
	if err := m.Answer(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return f.attached.Load() == 1 })
	m.ToggleMute()
	if !a.muted.Load() {
		t.Fatal("mute did not reach audio")
	}
	f.finish("remote_hangup")
	if !a.closed.Load() || m.Snapshot().Busy {
		t.Fatal("remote hangup leaked audio or active state")
	}
}

func TestOutgoingWaitsForPeerAcceptance(t *testing.T) {
	m, f, _ := newTestManager(t)
	if err := m.Start(f.Peer()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if f.attached.Load() != 0 {
		t.Fatal("microphone opened while ringing")
	}
	f.peerAccept()
	eventually(t, func() bool { return f.attached.Load() == 1 })
	if err := m.End(); err != nil {
		t.Fatal(err)
	}
	if f.hungup.Load() != 1 {
		t.Fatal("outgoing call not hung up")
	}
}

func TestOutgoingOfferDoesNotClaimPeerIsRinging(t *testing.T) {
	m, f, _ := newTestManager(t)
	f.phase = meowcaller.CallPhaseCalling
	if err := m.Start(f.Peer()); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().State; got != "Solicitud enviada · esperando respuesta" {
		t.Fatalf("state after offer = %q", got)
	}
}

func TestCancelPendingDialDiscardsLateCall(t *testing.T) {
	m, f, _ := newTestManager(t)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	m.dial = func(ctx context.Context, _ string) (session, error) {
		close(started)
		<-release // Simulate an offer completing just as cancellation arrives.
		if ctx.Err() == nil {
			t.Error("dial context was not cancelled")
		}
		return f, nil
	}
	go func() { done <- m.Start(f.Peer()) }()
	<-started
	if err := m.End(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Busy || f.hungup.Load() != 1 || f.attached.Load() != 0 {
		t.Fatal("late result resurrected cancelled call")
	}
}

func TestSecondCallRejectedAndStaleEndCannotFinishNewCall(t *testing.T) {
	m, first, _ := newTestManager(t)
	m.receive(first)
	second := &fakeSession{}
	m.receive(second)
	eventually(t, func() bool { return second.rejected.Load() == 1 })
	if err := m.End(); err != nil {
		t.Fatal(err)
	}
	third := &fakeSession{phase: meowcaller.CallPhaseRinging}
	m.receive(third)
	first.finish("late termination")
	if !m.Snapshot().Busy {
		t.Fatal("stale callback ended replacement call")
	}
}

func TestAudioFailureEndsCall(t *testing.T) {
	m, f, _ := newTestManager(t)
	m.open = func(context.Context) (Audio, error) { return nil, errors.New("no microphone") }
	m.receive(f)
	if err := m.Answer(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return !m.Snapshot().Busy && f.hungup.Load() > 0 })
	if m.Snapshot().Reason != "no microphone" {
		t.Fatal(m.Snapshot())
	}
}

func TestDisconnectClosesAudio(t *testing.T) {
	m, f, a := newTestManager(t)
	m.receive(f)
	if err := m.Answer(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return f.attached.Load() == 1 })
	_ = m.stop("disconnected", false)
	if !a.closed.Load() || m.Snapshot().Busy {
		t.Fatal("disconnect did not clean up")
	}
}

func TestValidateTargetRejectsGroupsAndEmpty(t *testing.T) {
	for _, jid := range []string{"", "123@g.us", "status@broadcast", "123@newsletter"} {
		if ValidateTarget(jid) == nil {
			t.Errorf("accepted %q", jid)
		}
	}
	for _, jid := range []string{"123@s.whatsapp.net", "123@lid"} {
		if err := ValidateTarget(jid); err != nil {
			t.Error(err)
		}
	}
}
