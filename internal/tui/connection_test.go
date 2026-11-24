package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestConnectionLossBlocksTypingAndPreservesDraft(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.focus = focusInput
	m.input.Focus()
	m.input.SetValue("borrador")
	lost, _ := m.Update(connectionMsg{connected: false, reason: "Conexión cerrada. Reinicia wa-cli."})
	m = lost.(model)
	if m.connected || !strings.Contains(m.renderInputBar(), "escritura bloqueada") {
		t.Fatal("the composer was not disabled after connection loss")
	}
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'x'}},
		{Type: tea.KeyEnter},
	} {
		updated, cmd := m.handleKey(key)
		m = updated.(model)
		if cmd != nil || m.input.Value() != "borrador" {
			t.Fatalf("offline key changed draft or sent a command: draft=%q", m.input.Value())
		}
	}
	restored, _ := m.Update(connectionMsg{connected: true})
	m = restored.(model)
	if !m.connected || m.input.Value() != "borrador" {
		t.Fatal("draft was not restored when connection returned")
	}
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if !strings.Contains(updated.(model).input.Value(), "x") {
		t.Fatal("typing did not resume after reconnection")
	}
}
