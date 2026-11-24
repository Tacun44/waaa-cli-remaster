package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

func TestVoicePlaybackWaitsForComposerBeforeSendingPlayedReceipt(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.client = &whatsapp.Client{}
	m.audioPlaySeq = 1
	voice := msgstore.Message{ID: "voice-1", ChatJID: "573001234567@s.whatsapp.net", SenderJID: "573001234567@s.whatsapp.net", MediaType: "voice note"}
	m.selectedJID = voice.ChatJID
	m.messages = []msgstore.Message{voice}

	updated, cmd := m.Update(audioPlayedMsg{msgID: voice.ID, chatJID: voice.ChatJID, message: voice, seq: 1})
	m = updated.(model)
	if !m.messages[0].Played {
		t.Fatal("voice note was not marked played locally")
	}
	if _, ok := cmd().(tea.BatchMsg); ok {
		t.Fatal("inactive composer sent a played receipt")
	}
	if len(m.pendingPlayedReceipts()) != 0 {
		t.Fatal("inactive composer scheduled a played receipt")
	}

	m.focus = focusInput
	cmds := m.pendingPlayedReceipts()
	if len(cmds) != 1 {
		t.Fatalf("activating composer scheduled %d receipts, want 1", len(cmds))
	}
	if _, ok := cmds[0]().(playedReceiptResultMsg); !ok {
		t.Fatal("pending command did not attempt the played receipt")
	}
	if len(m.pendingPlayedReceipts()) != 0 {
		t.Fatal("receipt was scheduled twice while pending")
	}

	m.messages[0].PlayedReceiptSent = true
	m.playedReceiptPending = nil
	if len(m.pendingPlayedReceipts()) != 0 {
		t.Fatal("confirmed receipt was scheduled again")
	}
}

func TestActiveVoicePlaybackSchedulesPlayedReceipt(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.client = &whatsapp.Client{}
	m.focus = focusInput
	m.audioPlaySeq = 1
	voice := msgstore.Message{ID: "voice-1", ChatJID: "573001234567@s.whatsapp.net", SenderJID: "573001234567@s.whatsapp.net", MediaType: "voice note"}
	m.selectedJID = voice.ChatJID
	m.messages = []msgstore.Message{voice}
	_, cmd := m.Update(audioPlayedMsg{msgID: voice.ID, chatJID: voice.ChatJID, message: voice, seq: 1})
	if batch, ok := cmd().(tea.BatchMsg); !ok || len(batch) != 2 {
		t.Fatal("active playback did not schedule the local marker and remote receipt")
	}

	voice.FromMe = true
	_, cmd = m.Update(audioPlayedMsg{msgID: voice.ID, chatJID: voice.ChatJID, message: voice, seq: 1})
	if _, ok := cmd().(tea.BatchMsg); ok {
		t.Fatal("own voice note scheduled a played receipt")
	}
}

func TestTabIntoComposerSendsPreviouslyPlayedReceipt(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.client = &whatsapp.Client{}
	m.focus = focusChatList
	voice := msgstore.Message{ID: "voice-1", ChatJID: "573001234567@s.whatsapp.net", SenderJID: "573001234567@s.whatsapp.net", MediaType: "voice note", Played: true}
	m.selectedJID = voice.ChatJID
	m.messages = []msgstore.Message{voice}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !updated.(model).activeChat() {
		t.Fatal("Tab did not activate the composer")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 3 {
		t.Fatalf("activating composer scheduled %#v, want read, presence, and played receipt", cmd())
	}
	if _, ok := batch[2]().(playedReceiptResultMsg); !ok {
		t.Fatal("activating composer did not attempt the pending played receipt")
	}
}
