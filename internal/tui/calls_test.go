package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/codebyoketch/wa-cli/internal/calls"
)

func TestCallPanelKeepsDraftAndRequiresExplicitCall(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "123@s.whatsapp.net"
	m.focus = focusInput
	m.input.SetValue("borrador sin enviar")
	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyF4})
	m = updated.(model)
	if cmd != nil || !m.callPanel || m.callTarget != m.selectedJID {
		t.Fatal("F4 must only open call panel")
	}
	updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if cmd != nil || m.callPanel || m.input.Value() != "borrador sin enviar" {
		t.Fatal("call panel changed the draft")
	}
}

func TestIncomingCallVisibleWithoutOpeningModal(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.ready, m.width, m.height = true, 110, 35
	m.callState = calls.Snapshot{Busy: true, Incoming: true, CanAnswer: true, Peer: "123@s.whatsapp.net", State: "Llamada entrante"}
	m.layout()
	view := m.View()
	if !strings.Contains(view, "Llamada entrante") || !strings.Contains(view, "F4") {
		t.Fatal("incoming call hidden")
	}
	if m.callPanel || lipgloss.Height(view) > m.height {
		t.Fatal("incoming call stole focus or broke layout")
	}
}

func TestCallPanelIgnoresBackgroundClicks(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.callPanel = true
	updated, cmd := m.Update(tea.MouseMsg{X: 1, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !updated.(model).callPanel || cmd != nil {
		t.Fatal("background handled a click through modal")
	}
}
