package tui

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	zone "github.com/lrstanley/bubblezone"
)

func audioClickModel(t *testing.T) (model, tea.MouseMsg) {
	t.Helper()
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.ready = true
	m.selectedJID = "123@s.whatsapp.net"
	m.width, m.height = 100, 30
	m.messages = []msgstore.Message{
		{ID: "old-voice", ChatJID: m.selectedJID, MediaType: "voice note", Timestamp: time.Now().UnixMilli()},
		{ID: "text", ChatJID: m.selectedJID, Text: "un texto", Timestamp: time.Now().UnixMilli()},
		{ID: "new-voice", ChatJID: m.selectedJID, MediaType: "voice note", Timestamp: time.Now().UnixMilli()},
	}
	m.layout()
	m.viewport.SetContent(m.renderMessages())
	_ = m.View()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		z := zone.Get(msgZoneID("old-voice"))
		if z != nil && !z.IsZero() && z.StartX <= z.EndX && z.StartY <= z.EndY {
			return m, tea.MouseMsg{X: z.StartX, Y: z.StartY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("voice message zone was not rendered")
	return model{}, tea.MouseMsg{}
}

func TestLeftClickPlaysClickedVoiceInsteadOfNewest(t *testing.T) {
	m, click := audioClickModel(t)
	m.replyToID = "text"
	m.audioSelectMode = true
	m.audioPlayMode = false // transcript selector must not override a direct click
	updated, cmd := m.Update(click)
	m = updated.(model)
	if cmd == nil || m.audioPlaySeq != 1 || m.selectedAudioID != "" || m.audioSelectMode || m.replyToID != "text" {
		t.Fatalf("click state: seq=%d selected=%q selector=%t reply=%q", m.audioPlaySeq, m.selectedAudioID, m.audioSelectMode, m.replyToID)
	}
	result, ok := cmd().(audioPlayedMsg)
	if !ok || result.msgID != "old-voice" {
		t.Fatalf("clicked voice not played: %#v", result)
	}
}

func TestLeftClickPausesAndResumesPlayingVoice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP is Unix-only")
	}
	m, click := audioClickModel(t)
	player := exec.Command("sleep", "30")
	if err := player.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = player.Process.Kill(); _ = player.Wait() }()
	m.playingAudioID = "old-voice"
	m.audioPlayer = player
	updated, cmd := m.Update(click)
	m = updated.(model)
	if cmd != nil || !m.audioPaused || m.statusLine != "Audio pausado" {
		t.Fatal("first click did not pause", m.statusLine)
	}
	updated, cmd = m.Update(click)
	m = updated.(model)
	if cmd != nil || m.audioPaused || m.statusLine != "Reproduciendo nota de voz" {
		t.Fatal("second click did not resume", m.statusLine)
	}
}

func TestMultiSelectionStillTakesPrecedenceOverAudioClick(t *testing.T) {
	m, click := audioClickModel(t)
	m.multiSelectMode = true
	m.multiSelectIDs = map[string]bool{}
	updated, _ := m.Update(click)
	m = updated.(model)
	if !m.multiSelectIDs["old-voice"] || m.audioPlaySeq != 0 {
		t.Fatal("multi selection was replaced by playback")
	}
}
