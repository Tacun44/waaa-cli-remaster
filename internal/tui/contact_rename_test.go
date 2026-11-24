package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
	zone "github.com/lrstanley/bubblezone"
)

func TestClickChatNameEditsAndPersistsIt(t *testing.T) {
	dir := t.TempDir()
	cs := chatstore.New(dir)
	chat := chatstore.Chat{JID: "573001234567@s.whatsapp.net", Name: "Nombre original"}
	if err := cs.Upsert(chat); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, cs, nil, nil)
	m.chats = []chatstore.Chat{chat}
	m.selectedJID = chat.JID
	m.chatNumbers[chat.JID] = "+573001234567"
	m.ready = true
	m.width, m.height = 100, 30
	m.layout()
	if sidebar := ansi.Strip(m.renderChatList()); strings.Contains(sidebar, "+573001234567") {
		t.Fatalf("sidebar unexpectedly showed the number: %q", sidebar)
	}
	if header := ansi.Strip(m.renderChatHeader()); !strings.Contains(header, "Nombre original · +573001234567") {
		t.Fatalf("header did not show name and number: %q", header)
	}
	_ = m.View()
	var z *zone.ZoneInfo
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		z = zone.Get(chatHeaderZoneID(chat.JID))
		if z != nil && !z.IsZero() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if z == nil || z.IsZero() {
		t.Fatal("chat name is not clickable")
	}
	updated, _ := m.Update(tea.MouseMsg{X: z.StartX, Y: z.StartY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(model)
	if m.renameJID != chat.JID {
		t.Fatal("click did not open name editor")
	}
	m.renameInput.SetValue("Nombre elegido")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	updated, _ = m.Update(cmd())
	m = updated.(model)
	if m.renameJID != "" || m.chats[0].DisplayName() != "Nombre elegido" {
		t.Fatalf("editor state = (%q, %q)", m.renameJID, m.chats[0].DisplayName())
	}
	stored, ok, err := chatstore.New(dir).Get(chat.JID)
	if err != nil || !ok || stored.LocalName != "Nombre elegido" {
		t.Fatalf("stored name = (%+v, %v, %v)", stored, ok, err)
	}
}

func TestUnnamedChatUsesNumberAsItsName(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	chat := chatstore.Chat{JID: "573001234567@s.whatsapp.net"}
	m.chats = []chatstore.Chat{chat}
	m.selectedJID = chat.JID
	m.chatNumbers[chat.JID] = "+573001234567"
	m.width, m.height = 100, 30
	m.layout()
	if got := m.chatDisplayName(chat); got != "+573001234567" {
		t.Fatalf("unnamed chat label = %q", got)
	}
	if header := ansi.Strip(m.renderChatHeader()); strings.Count(header, "+573001234567") != 1 {
		t.Fatalf("header should show the number once: %q", header)
	}
	if sidebar := ansi.Strip(m.renderChatList()); !strings.Contains(sidebar, "+573001234567") {
		t.Fatalf("sidebar should use number as fallback name: %q", sidebar)
	}
}

func TestChatNumbersUsePhoneJIDsAndHideUnverifiedLIDs(t *testing.T) {
	cmd := loadChatNumbersCmd(context.Background(), &whatsapp.Client{}, []chatstore.Chat{
		{JID: "573001234567@s.whatsapp.net", Name: "Ana"},
		{JID: "123456@lid", Name: "LID"},
		{JID: "987654@g.us", Name: "Grupo", IsGroup: true},
	})
	result := cmd().(chatNumbersLoadedMsg)
	if result.numbers["573001234567@s.whatsapp.net"] != "+573001234567" {
		t.Fatalf("phone number = %q", result.numbers["573001234567@s.whatsapp.net"])
	}
	if result.numbers["123456@lid"] != "" || result.numbers["987654@g.us"] != "" {
		t.Fatalf("unverified identifiers shown as phone numbers: %#v", result.numbers)
	}
}
