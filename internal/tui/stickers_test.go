package tui

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

func TestStickerPlaceholderFitsViewportAndStaysWithMessage(t *testing.T) {
	id := uint32(0x123456)
	placeholder := stickerPlaceholder(id)
	for _, line := range strings.Split(placeholder, "\n") {
		if width := lipgloss.Width(line); width != stickerColumns {
			t.Fatalf("placeholder width = %d, want %d", width, stickerColumns)
		}
	}
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "123@s.whatsapp.net"
	m.viewport = viewport.New(60, 5)
	sticker := msgstore.Message{ID: "sticker-1", ChatJID: m.selectedJID, MediaType: "sticker"}
	photo := msgstore.Message{ID: "photo-1", ChatJID: m.selectedJID, MediaType: "image"}
	m.messages = []msgstore.Message{photo, sticker}
	m.stickerImages[stickerKey(sticker)] = id
	hidden := m.renderMessages()
	if strings.Contains(hidden, "\U0010eeee") {
		t.Fatal("sticker image should be hidden until s is pressed")
	}
	m.stickerReveal[stickerKey(sticker)] = true
	view := m.renderMessages()
	if strings.Count(view, "\U0010eeee") != stickerColumns*stickerRows {
		t.Fatal("sticker image cells were not included in message view")
	}
	if strings.Contains(view, "\x1b_G") {
		t.Fatal("graphics commands must stay outside Bubble Tea's viewport")
	}
	m.viewport.SetContent(view)
	if !strings.Contains(m.viewport.View(), "\U0010eeee") {
		t.Fatal("viewport removed Kitty placeholder characters")
	}
	m.ready, m.width, m.height = true, 90, 20
	if !strings.Contains(m.View(), "\U0010eeee") {
		t.Fatal("Bubble Tea view removed Kitty placeholder characters")
	}
}

func TestSRevealsOnlyStickersVisibleAtPress(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "123@s.whatsapp.net"
	m.viewport = viewport.New(60, 8)
	m.stickerTerminal = &synchronizedTerminal{out: &bytes.Buffer{}}
	first := msgstore.Message{ID: "first", ChatJID: m.selectedJID, MediaType: "sticker", RawProto: "raw"}
	last := msgstore.Message{ID: "last", ChatJID: m.selectedJID, MediaType: "sticker", RawProto: "raw"}
	m.messages = append(m.messages, first)
	for i := 0; i < 20; i++ {
		m.messages = append(m.messages, msgstore.Message{ID: string(rune('a' + i)), ChatJID: m.selectedJID, Text: "message"})
	}
	m.messages = append(m.messages, last)
	m.viewport.SetContent(m.renderMessages())
	m.viewport.GotoBottom()
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(model)
	if m.stickerReveal[stickerKey(first)] || !m.stickerReveal[stickerKey(last)] {
		t.Fatal("s did not select only visible stickers")
	}
	if len(m.stickerPending) != 1 || m.stickerPending[0].ID != last.ID {
		t.Fatalf("pending stickers = %#v, want last only", m.stickerPending)
	}
}

func TestSStillTypesInComposer(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.focus = focusInput
	m.input.Focus()
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if updated.(model).input.Value() != "s" {
		t.Fatal("s shortcut intercepted text entry")
	}
}

func TestStickerUploadUsesVirtualPlacementWithoutCursorMovement(t *testing.T) {
	var output bytes.Buffer
	terminal := &synchronizedTerminal{out: &output}
	if err := uploadStickerPNG(terminal, 42, []byte("PNG")); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "a=t,f=100,i=42,q=2") || !strings.Contains(got, "a=p,U=1,i=42,c=8,r=4,q=2") {
		t.Fatalf("unexpected graphics commands: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatal("graphics transfer moved the terminal cursor")
	}
}

func TestSynchronizedTerminalExposesTTYFileDescriptor(t *testing.T) {
	wrapped := &synchronizedTerminal{out: os.Stdout}
	if wrapped.Fd() != os.Stdout.Fd() {
		t.Fatal("Bubble Tea cannot detect terminal size through synchronized output")
	}
}

func TestOnlyStickerMessagesAreEligible(t *testing.T) {
	for _, mediaType := range []string{"image", "video", "audio", "document"} {
		if isStickerMessage(msgstore.Message{ID: "x", MediaType: mediaType}) {
			t.Fatalf("%s was treated as a sticker", mediaType)
		}
	}
	if !isStickerMessage(msgstore.Message{ID: "x", MediaType: "sticker"}) {
		t.Fatal("sticker was not recognized")
	}
}
