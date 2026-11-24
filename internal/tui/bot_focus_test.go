package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSyncBotFocusAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot_focus")
	if err := syncBotFocus(path, "111:7@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", first.Mode())
	}
	if err := syncBotFocus(path, "222@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(first, second) {
		t.Fatal("replacement was not atomic rename")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "222@s.whatsapp.net" {
		t.Fatalf("focus = %q, %v", content, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "bot_focus" {
		t.Fatalf("temporary files left: %v, %v", entries, err)
	}
	if err := syncBotFocus(path, "222@s.whatsapp.net", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("focus remains: %v", err)
	}
}

func TestBotFocusFollowsComposerAndTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot_focus")
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.botFocusPath = path
	m.selectedJID = "111@s.whatsapp.net"
	m.focus = focusInput
	m.input.Focus()
	updated, _ := m.Update(tea.FocusMsg{})
	m = updated.(model)
	if content, err := os.ReadFile(path); err != nil || string(content) != m.selectedJID {
		t.Fatalf("focused = %q, %v", content, err)
	}
	updated, _ = m.Update(tea.BlurMsg{})
	m = updated.(model)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("blur left focus: %v", err)
	}
	m.focus = focusInput
	m.input.Focus()
	updated, _ = m.Update(tea.FocusMsg{})
	m = updated.(model)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("refocus: %v", err)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(model)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("tab left focus: %v", err)
	}
}
