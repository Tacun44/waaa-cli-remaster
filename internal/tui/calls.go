package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codebyoketch/wa-cli/internal/calls"
)

type callTickMsg struct{}
type callActionMsg struct{ err error }

func callTickCmd() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return callTickMsg{} })
}

func (m model) callName(jid string) string {
	for _, chat := range m.chats {
		if chat.JID == jid && chat.Name != "" {
			return chat.Name
		}
	}
	if name := m.senderNames[jid]; name != "" {
		return name
	}
	return jid
}

func (m model) callAction(action string) (tea.Model, tea.Cmd) {
	if m.calls == nil {
		m.statusLine = "Llamadas no disponibles"
		return m, nil
	}
	if m.callActionBusy && action != "end" {
		return m, nil
	}
	state := m.calls.Snapshot()
	if action == "start" || action == "start-video" {
		if state.Busy {
			return m, nil
		}
		if err := calls.ValidateTarget(m.callTarget); err != nil {
			m.statusLine = err.Error()
			return m, nil
		}
		if m.isRecordingAudio {
			m.statusLine = "Termina o cancela la nota de voz antes de llamar"
			return m, nil
		}
	}
	if action == "answer" && m.isRecordingAudio {
		m.statusLine = "Termina o cancela la nota de voz antes de contestar"
		return m, nil
	}
	if action == "start" || action == "start-video" || action == "answer" {
		stopAudioPlayer(m.audioPlayer)
		m.audioPlayer, m.playingAudioID = nil, ""
		m.audioPlaySeq++
	}
	m.callActionBusy = true
	manager, target := m.calls, m.callTarget
	return m, func() tea.Msg {
		var err error
		switch action {
		case "start":
			err = manager.Start(target)
		case "start-video":
			err = manager.StartVideo(target)
		case "camera":
			err = manager.ToggleCamera()
		case "answer":
			err = manager.Answer()
		case "end":
			err = manager.End()
		case "mute":
			manager.ToggleMute()
		}
		return callActionMsg{err}
	}
}

func (m model) handleCallKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "f4":
		m.callPanel = false
		return m, nil
	case "f6", "h":
		return m.callAction("end")
	case "m":
		return m.callAction("mute")
	case "v":
		if m.calls == nil {
			return m, nil
		}
		if m.calls.Snapshot().Busy {
			return m.callAction("camera")
		}
		return m.callAction("start-video")
	case "enter", "f5":
		if m.calls == nil {
			return m, nil
		}
		state := m.calls.Snapshot()
		if state.CanAnswer {
			return m.callAction("answer")
		}
		if !state.Busy && msg.String() == "enter" {
			return m.callAction("start")
		}
	}
	return m, nil
}

func (m model) renderCallPanel() string {
	width := min(62, max(20, m.width-6))
	s := m.callState
	peer := m.callTarget
	if s.Busy {
		peer = s.Peer
	}
	title := "☎ Llamadas"
	if s.Busy && s.Video {
		title = "▣ Videollamada"
	}
	lines := []string{title, "", ansi.Truncate(m.callName(peer), width-4, "…"), ""}
	if s.Busy {
		lines = append(lines, s.Description(), "")
		if s.CanAnswer {
			answer := "Enter / F5: contestar"
			if s.Video {
				answer += " con cámara"
			}
			lines = append(lines, answer+" · F6: rechazar")
		} else {
			lines = append(lines, "F6: colgar · m: silenciar micrófono")
			if s.Video {
				lines = append(lines, "v: apagar / encender cámara")
			}
		}
	} else {
		if s.Reason != "" {
			lines = append(lines, s.Description(), "")
		}
		lines = append(lines, "Enter: llamada de audio · v: videollamada")
	}
	if m.callActionBusy {
		lines = append(lines, "Procesando…")
	}
	if strings.HasPrefix(m.statusLine, "Llamada:") {
		lines = append(lines, ansi.Truncate(m.statusLine, width-4, "…"))
	}
	lines = append(lines, "Esc / F4: cerrar panel", "", "Audio: dispositivos predeterminados del sistema")
	if s.Video {
		lines = append(lines, "Video remoto: ventana mpv")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("141")).Padding(1, 2).Width(width).Render(strings.Join(lines, "\n"))
}

func (m model) callFooter() string {
	if !m.callState.Busy {
		return ""
	}
	answer := "F5 contestar"
	if m.callState.Video {
		answer += " con cámara"
	}
	return fmt.Sprintf("☎ %s · %s · F4 controles · %s · F6 colgar", m.callName(m.callState.Peer), m.callState.Description(), answer)
}
