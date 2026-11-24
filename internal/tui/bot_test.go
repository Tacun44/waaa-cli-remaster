package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/polymorfa/hypermeow/types"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/tuisend"
)

func TestResolveBotTextRequest(t *testing.T) {
	jid, err := types.ParseJID("111:7@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	quoted := msgstore.Message{ID: "quoted-message", ChatJID: "111@s.whatsapp.net", Text: "original"}
	lookupErr := errors.New("store failed")
	for _, tc := range []struct {
		name, text, quotedID string
		found                bool
		lookupErr            error
		wantLookup           bool
		wantReply            bool
		wantErr              string
	}{
		{name: "plain text remains plain", text: "hello"},
		{name: "quoted reply", text: "hello", quotedID: quoted.ID, found: true, wantLookup: true, wantReply: true},
		{name: "missing quote", text: "hello", quotedID: "missing", wantLookup: true, wantErr: "quoted message not found"},
		{name: "lookup error", text: "hello", quotedID: quoted.ID, lookupErr: lookupErr, wantLookup: true, wantErr: "store failed"},
		{name: "empty text", quotedID: quoted.ID, wantErr: "text must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			get := func(chatJID, id string) (msgstore.Message, bool, error) {
				called = true
				if chatJID != "111@s.whatsapp.net" || id != tc.quotedID {
					t.Fatalf("lookup key = (%q, %q)", chatJID, id)
				}
				return quoted, tc.found, tc.lookupErr
			}
			got, reply, err := resolveBotTextRequest(tuisend.Request{Text: tc.text, QuotedID: tc.quotedID}, jid, get)
			if called != tc.wantLookup || reply != tc.wantReply {
				t.Fatalf("lookup=%t reply=%t, want lookup=%t reply=%t", called, reply, tc.wantLookup, tc.wantReply)
			}
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if reply && got != quoted {
				t.Fatalf("quoted message = %+v, want %+v", got, quoted)
			}
		})
	}
}

func TestToggleBotFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot_on")
	for _, tc := range []struct {
		jid, want string
		on        bool
	}{
		{"111@s.whatsapp.net", "111@s.whatsapp.net", true},
		{"222@s.whatsapp.net", "222@s.whatsapp.net", true},
		{"222@s.whatsapp.net", "", false},
		{"111@s.whatsapp.net", "111@s.whatsapp.net", true},
		{"111@s.whatsapp.net", "", false},
	} {
		got, err := toggleBotFile(path, tc.jid)
		if err != nil || got != tc.on {
			t.Fatalf("toggleBotFile(%q) = (%v, %v), want (%v, nil)", tc.jid, got, err, tc.on)
		}
		content, err := os.ReadFile(path)
		if tc.on && (err != nil || string(content) != tc.want) || !tc.on && !os.IsNotExist(err) {
			t.Fatalf("marker after toggle to %q: %q, %v", tc.want, content, err)
		}
	}
}

func TestBotToggleControlsService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot_on")
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.botPath = path
	m.selectedJID = "111@s.whatsapp.net"
	var actions []bool
	m.botServiceControl = func(on bool) error {
		actions = append(actions, on)
		return nil
	}
	m, _ = m.handleBotToggleKey("ctrl+b")
	m, _ = m.handleBotLevelKey("2")
	m, _ = m.handleBotToggleKey("ctrl+b")
	if len(actions) != 2 || !actions[0] || actions[1] {
		t.Fatalf("service actions = %v, want [true false]", actions)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bot marker remains after deactivation: %v", err)
	}
}

func TestBotLevelSelectionCanBeCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot_on")
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.botPath = path
	m.selectedJID = "111@s.whatsapp.net"
	m, handled := m.handleBotToggleKey("ctrl+b")
	if !handled || !m.botLevelPending {
		t.Fatal("Ctrl+B did not request a level")
	}
	m, handled = m.handleBotLevelKey("esc")
	if !handled || m.botLevelPending {
		t.Fatal("Esc did not cancel level selection")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancelled selection created bot marker: %v", err)
	}
}

func TestBotToggleRollsBackWhenServiceCannotStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot_on")
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.botPath = path
	m.selectedJID = "111@s.whatsapp.net"
	m.botServiceControl = func(bool) error { return errors.New("start failed") }
	m, _ = m.handleBotToggleKey("ctrl+b")
	m, _ = m.handleBotLevelKey("3")
	if _, err := os.Stat(path); !os.IsNotExist(err) || !strings.Contains(m.statusLine, "start failed") {
		t.Fatalf("failed start left marker or hid error: %v, %q", err, m.statusLine)
	}
}

func TestBotToggleKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot_on")
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.botPath = path
	m.selectedJID = "111:7@s.whatsapp.net"
	m.ready = true
	m.width, m.height = 93, 30
	m.layout()

	// Bubble Tea v1 cannot construct ctrl+ñ as a KeyMsg on terminals without
	// extended keyboard reporting, so test its dispatch string directly.
	for _, key := range []string{"ctrl+ñ", "alt+ñ", "ctrl+b"} {
		for _, focus := range []focusArea{focusChatList, focusInput} {
			m.focus = focus
			m.input.SetValue("borrador")
			if focus == focusInput {
				m.input.Focus()
			} else {
				m.input.Blur()
			}
			var handled bool
			m, handled = m.handleBotToggleKey(key)
			m, _ = m.handleBotLevelKey("3")
			if !handled || m.statusLine != "bot activado · nivel 3" {
				t.Fatalf("%s with focus %v did not activate bot: handled=%v status=%q", key, focus, handled, m.statusLine)
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != "111@s.whatsapp.net" {
				t.Fatalf("%s marker = %q, %v", key, content, err)
			}
			if m.input.Value() != "borrador" {
				t.Fatalf("%s changed composer: %q", key, m.input.Value())
			}
			if !strings.Contains(ansi.Strip(m.renderChatHeader()), "BOT ON") {
				t.Fatalf("%s did not show indicator", key)
			}
			m.selectedJID = "222@s.whatsapp.net"
			if strings.Contains(ansi.Strip(m.renderChatHeader()), "BOT ON") {
				t.Fatalf("%s showed indicator in another chat", key)
			}
			m.selectedJID = "111:7@s.whatsapp.net"
			m, handled = m.handleBotToggleKey(key)
			if !handled || m.statusLine != "bot desactivado" {
				t.Fatalf("%s did not deactivate bot: handled=%v status=%q", key, handled, m.statusLine)
			}
			if strings.Contains(ansi.Strip(m.renderChatHeader()), "BOT ON") {
				t.Fatalf("%s left indicator visible", key)
			}
		}
	}

	for _, key := range []tea.KeyMsg{{Type: tea.KeyCtrlB}, {Type: tea.KeyRunes, Runes: []rune{'ñ'}, Alt: true}} {
		m.focus = focusInput
		updated, cmd := m.Update(key)
		m = updated.(model)
		m, _ = m.handleBotLevelKey("1")
		if _, err := os.Stat(path); err != nil || cmd != nil || m.input.Value() != "borrador" {
			t.Fatalf("Update(%s) did not consume toggle cleanly", key.String())
		}
		m, _ = m.handleBotToggleKey(key.String())
	}
}

func TestBotToggleRejectsNonIndividualChats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot_on")
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.botPath = path
	for _, jid := range []string{"", "123@g.us", "status@broadcast", "123@newsletter"} {
		m.selectedJID = jid
		var handled bool
		m, handled = m.handleBotToggleKey("ctrl+b")
		if !handled || m.statusLine != "el bot solo funciona en chats individuales" {
			t.Fatalf("toggle %q: handled=%v status=%q", jid, handled, m.statusLine)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("toggle %q created bot_on: %v", jid, err)
		}
	}
}

// Ctrl+V runs a pointer-receiver handler that returns *model; Update must not
// panic on that (it did once the bot_focus wrapper asserted a model value).
func TestUpdateAcceptsPointerModelFromHandlers(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.botFocusPath = filepath.Join(t.TempDir(), "bot_focus")
	m.ready = true
	m.width, m.height = 93, 30
	m.layout()
	m.focus = focusInput
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	if _, ok := updated.(model); !ok {
		t.Fatalf("Update returned %T, want tui.model", updated)
	}
}
