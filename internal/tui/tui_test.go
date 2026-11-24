package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	waProto "github.com/polymorfa/hypermeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func TestViewKeepsComposerInsideTerminalAfterResize(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.ready = true
	m.selectedJID = "contact@s.whatsapp.net"
	m.width, m.height = 93, 35
	m.layout()
	m.viewport.SetContent(m.renderMessages())

	view := m.View()
	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("view height = %d, terminal height = %d", got, m.height)
	}
	if !strings.Contains(view, "Type a message") {
		t.Fatal("composer is missing from view")
	}
	if strings.Contains(view, "\x1b[1;1H") {
		t.Fatal("view moves the cursor; Bubble Tea positions each frame itself")
	}
}

func TestVideoSelectionSkipsNonVideosAndMovesOlder(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.messages = []msgstore.Message{
		{ID: "photo", MediaType: "image"},
		{ID: "video-1", MediaType: "video/mp4"},
		{ID: "text", Text: "hola"},
		{ID: "video-2", MediaType: "video/mp4"},
	}

	if !m.moveVideoSelection(-1) || m.selectedVideoID != "video-2" {
		t.Fatalf("first video selection = %q, want latest video-2", m.selectedVideoID)
	}
	if !m.moveVideoSelection(-1) || m.selectedVideoID != "video-1" {
		t.Fatalf("previous video selection = %q, want video-1", m.selectedVideoID)
	}
	if m.moveVideoSelection(-1) {
		t.Fatal("selection moved before the oldest video")
	}
}

func TestDroppedMediaKind(t *testing.T) {
	tests := []struct {
		path, mime string
		want       int64
	}{
		{"clip.mp4", "application/octet-stream", 2},
		{"clip.MOV", "application/octet-stream", 2},
		{"clip.mkv", "application/octet-stream", 2},
		{"clip.webm", "video/webm", 2},
		{"clip.avi", "application/octet-stream", 2},
		{"clip.m4v", "application/octet-stream", 2},
		{"clip.3gp", "application/octet-stream", 2},
		{"clip.bin", "video/mp4", 2},
		{"photo.jpg", "image/jpeg", 1},
		{"photo.PNG", "application/octet-stream", 1},
		{"photo.png", "image/png", 1},
		{"file.pdf", "application/pdf", 5},
	}
	for _, tt := range tests {
		if got := droppedMediaKind(tt.path, tt.mime); got != tt.want {
			t.Errorf("droppedMediaKind(%q, %q) = %d, want %d", tt.path, tt.mime, got, tt.want)
		}
	}
}

func TestCreateUniqueDownloadDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(original, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		path, file, err := createUniqueDownload(dir, "video.mp4")
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, fmt.Sprintf("video (%d).mp4", i))
		if path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
		file.Close()
	}
	data, err := os.ReadFile(original)
	if err != nil || string(data) != "original" {
		t.Fatalf("original changed: %q, %v", data, err)
	}
}

func TestParseDroppedFilesHandlesPathsWithSpacesAndFileURIs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archivo de prueba.pdf")
	if err := os.WriteFile(path, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, input := range []string{path, `"` + path + `"`, strings.ReplaceAll(path, " ", `\ `)} {
		got, ok := parseDroppedFiles(input)
		if !ok || len(got) != 1 || got[0] != path {
			t.Fatalf("parseDroppedFiles(%q) = (%v, %v), want [%q], true", input, got, ok, path)
		}
	}

	fileURI := (&url.URL{Scheme: "file", Path: path}).String()
	got, ok := parseDroppedFiles(fileURI)
	if !ok || len(got) != 1 || got[0] != path {
		t.Fatalf("parseDroppedFiles(%q) = (%v, %v), want [%q], true", fileURI, got, ok, path)
	}
}

func TestPastedDroppedFileStartsAttachmentInsteadOfFillingComposer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(path, []byte("not a real png"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "chat@s.whatsapp.net"
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(path), Paste: true})
	got := updated.(model)
	if cmd == nil || !strings.Contains(got.statusLine, "Enviando 1 archivo") {
		t.Fatalf("drop status/cmd = (%q, %v), want attachment send", got.statusLine, cmd != nil)
	}
	if got.input.Value() != "" {
		t.Fatalf("dropped path leaked into composer: %q", got.input.Value())
	}
}

func TestDocumentRenderShowsFilenameAndPurpleBorder(t *testing.T) {
	raw, err := proto.Marshal(&waProto.Message{DocumentMessage: &waProto.DocumentMessage{
		FileName: proto.String("informe.pdf"),
		Mimetype: proto.String("application/pdf"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "chat@s.whatsapp.net"
	m.width, m.height = 80, 20
	m.messages = []msgstore.Message{{
		ID: "document", MediaType: "document", FromMe: true,
		RawProto: base64.StdEncoding.EncodeToString(raw), Timestamp: time.Now().UnixMilli(),
	}}
	m.layout()
	view := ansi.Strip(m.renderMessages())
	if !strings.Contains(view, "informe.pdf") {
		t.Fatalf("document filename is missing from transcript: %q", view)
	}
	if !strings.Contains(view, "╭") || !strings.Contains(view, "╰") {
		t.Fatalf("document does not have a visible bordered card: %q", view)
	}
}

func TestViewLayoutWithLongMessagesAndSidebar(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.ready = true
	m.width, m.height = 133, 37
	m.selectedJID = "juan@s.whatsapp.net"
	m.chats = []chatstore.Chat{{JID: m.selectedJID, Name: "Contacto de prueba"}, {JID: "other@g.us", Name: "Grupo de prueba"}}
	m.messages = []msgstore.Message{
		{ID: "one", Timestamp: time.Now().AddDate(0, 0, -2).UnixMilli(), Text: "mensaje de prueba largo para comprobar el ajuste de línea dentro de la burbuja del chat\nSegunda línea de ejemplo con texto suficiente para forzar el salto de línea", SenderJID: m.selectedJID},
		{ID: "two", Timestamp: time.Now().AddDate(0, 0, -1).UnixMilli(), Text: "otro mensaje de ejemplo", SenderJID: m.selectedJID},
		{ID: "three", Timestamp: time.Now().UnixMilli(), Text: "buenas", FromMe: true},
	}
	m.layout()
	m.viewport.SetContent(m.renderMessages())
	m.viewport.GotoBottom()
	view := ansi.Strip(m.View())
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		if width := ansi.StringWidth(line); width > m.width {
			t.Fatalf("line %d is %d columns wide in a %d-column terminal", i, width, m.width)
		}
	}
	if len(lines) != m.height {
		t.Fatalf("view has %d rows, terminal has %d", len(lines), m.height)
	}
	if !strings.Contains(lines[1], "Chats") {
		t.Fatalf("sidebar header is missing: %q", lines[1])
	}
}

func TestWindowSizeRefreshesLayoutOnlyWhenItChanges(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.ready = true
	m.width, m.height = 93, 30
	m.layout()

	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 93, Height: 30})
	if cmd != nil {
		t.Fatal("unchanged terminal size should not clear the screen")
	}
	m = updated.(model)

	updated, cmd = m.Update(tea.WindowSizeMsg{Width: 133, Height: 37})
	m = updated.(model)
	if cmd == nil || m.width != 133 || m.height != 37 || m.viewport.Width <= 59 {
		t.Fatal("changed terminal size did not update the layout")
	}
}

func TestAudioDuration(t *testing.T) {
	raw, err := proto.Marshal(&waProto.Message{AudioMessage: &waProto.AudioMessage{Seconds: proto.Uint32(125)}})
	if err != nil {
		t.Fatal(err)
	}
	msg := msgstore.Message{MediaType: "voice note", RawProto: base64.StdEncoding.EncodeToString(raw)}
	if got := audioDurationSeconds(msg); got != 125 {
		t.Fatalf("audioDurationSeconds() = %d, want 125", got)
	}
	if got := formatAudioDuration(125); got != "2:05" {
		t.Fatalf("formatAudioDuration() = %q, want 2:05", got)
	}
}

func TestAudioDurationToleratesOldOrInvalidMessages(t *testing.T) {
	for _, msg := range []msgstore.Message{
		{MediaType: "voice note"},
		{MediaType: "voice note", RawProto: "not-base64"},
		{MediaType: "text", RawProto: "not-base64"},
	} {
		if got := audioDurationSeconds(msg); got != 0 {
			t.Fatalf("audioDurationSeconds(%+v) = %d, want 0", msg, got)
		}
	}
}

func TestViewOnceAudioDoesNotPlayOrTranscribe(t *testing.T) {
	msg := msgstore.Message{ID: "once", MediaType: "audio de una sola vez (abrir en celular)"}
	m := model{messages: []msgstore.Message{msg}, selectedAudioID: msg.ID}
	played, playCmd := m.handlePlayAudio()
	if playCmd != nil || !strings.Contains(played.(model).statusLine, "celular") {
		t.Fatalf("view-once playback should direct the user to the phone: %q", played.(model).statusLine)
	}
	transcribed, transcribeCmd := m.handleTranscribeAudio()
	if transcribeCmd != nil || !strings.Contains(transcribed.(model).statusLine, "celular") {
		t.Fatalf("view-once transcription should direct the user to the phone: %q", transcribed.(model).statusLine)
	}
}

func TestSameContactDoesNotInferIdentityFromSelectedChatName(t *testing.T) {
	const (
		contactoAJID = "11111111111111@lid"
		marJID      = "22222222222222@lid"
	)
	m := model{
		selectedJID: marJID,
		chats: []chatstore.Chat{
			{JID: contactoAJID, Name: "Contacto A"},
			{JID: marJID, Name: "Mar"},
		},
		senderNames: map[string]string{
			contactoAJID: "Contacto A",
			marJID:      "Mar",
		},
	}

	if m.sameContact(contactoAJID, marJID) {
		t.Fatal("different JIDs were treated as the selected contact")
	}
	if !m.sameContact("11111111111111:13@lid", contactoAJID) {
		t.Fatal("device-qualified and base JIDs for one contact should match")
	}
}

func TestMarkVisibleMessagesReadClearsLocalUnread(t *testing.T) {
	const chatJID = "22222222222222@lid"
	cs := chatstore.New(t.TempDir())
	if err := cs.IncrementUnread(chatJID); err != nil {
		t.Fatal(err)
	}

	result := markVisibleMessagesReadCmd(nil, cs, nil, chatJID, []msgstore.Message{
		{ID: "incoming-1", ChatJID: chatJID, SenderJID: chatJID},
	})()
	if msg, ok := result.(readResultMsg); !ok || msg.err != nil {
		t.Fatalf("markVisibleMessagesReadCmd() = %#v", result)
	}
	chat, ok, err := cs.Get(chatJID)
	if err != nil || !ok {
		t.Fatalf("Get() = (%#v, %v, %v)", chat, ok, err)
	}
	if chat.UnreadCount != 0 {
		t.Fatalf("UnreadCount = %d, want 0", chat.UnreadCount)
	}
}

func TestMarkVisibleMessagesReadSkipsAlreadyConfirmedMessages(t *testing.T) {
	const chatJID = "22222222222222@lid"
	result := markVisibleMessagesReadCmd(nil, nil, nil, chatJID, []msgstore.Message{
		{ID: "confirmed", ChatJID: chatJID, SenderJID: chatJID, Status: "read"},
		{ID: "pending", ChatJID: chatJID, SenderJID: "22222222222222:38@lid"},
	})()
	msg, ok := result.(readResultMsg)
	if !ok || msg.err != nil {
		t.Fatalf("markVisibleMessagesReadCmd() = %#v", result)
	}
	if len(msg.msgIDs) != 1 || msg.msgIDs[0] != "pending" {
		t.Fatalf("msgIDs = %#v, want only pending", msg.msgIDs)
	}
}

func TestLastProcessError(t *testing.T) {
	stderr := bytes.NewBufferString("input initialized\nError opening input file default.\n")
	if got := lastProcessError(stderr); got != "Error opening input file default." {
		t.Fatalf("lastProcessError() = %q", got)
	}
}

func TestSidebarToggleExpandsChatViewport(t *testing.T) {
	m := model{
		width:  100,
		height: 30,
		focus:  focusChatList,
	}
	m.layout()
	withSidebar := m.viewport.Width

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if cmd == nil {
		t.Fatal("sidebar toggle should clear the screen")
	}
	m = updated.(model)
	if !m.sidebarHidden {
		t.Fatal("sidebarHidden = false, want true after pressing b")
	}
	if m.viewport.Width <= withSidebar {
		t.Fatalf("viewport width = %d, want more than %d with sidebar hidden", m.viewport.Width, withSidebar)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = updated.(model)
	if m.sidebarHidden {
		t.Fatal("sidebarHidden = true, want false after pressing b again")
	}
	if m.viewport.Width != withSidebar {
		t.Fatalf("viewport width = %d, want %d after restoring sidebar", m.viewport.Width, withSidebar)
	}
}

func TestReplySelectionDoesNotScrollToEarlierTranscriptMarker(t *testing.T) {
	m := model{
		selectedJID: "contact@s.whatsapp.net",
		messages: []msgstore.Message{
			{ID: "voice", MediaType: "voice note", Transcript: "texto transcrito"},
			{ID: "target", Text: "mensaje visible"},
		},
		senderNames: map[string]string{},
	}
	m.viewport.Width = 60
	m.viewport.Height = 4
	m.viewport.SetContent(m.renderMessages())
	m.viewport.SetYOffset(1)
	m.replyToID = "target"
	start, ok := m.messageStartLine("target")
	if !ok || start < 1 {
		t.Fatalf("messageStartLine(target) = (%d, %v), want a line after the transcript", start, ok)
	}

	m.ensureReplyMessageVisible(-1)
	if m.viewport.YOffset != 1 {
		t.Fatalf("YOffset = %d, want 1; earlier transcript must not move the viewport", m.viewport.YOffset)
	}
}

func TestIncomingMessageDoesNotMoveReaderWhoIsReviewingHistory(t *testing.T) {
	const chatJID = "contact@s.whatsapp.net"
	m := model{
		selectedJID: chatJID,
		messages: []msgstore.Message{
			{ID: "one", ChatJID: chatJID, Text: "uno"},
			{ID: "two", ChatJID: chatJID, Text: "dos"},
			{ID: "three", ChatJID: chatJID, Text: "tres"},
			{ID: "four", ChatJID: chatJID, Text: "cuatro"},
		},
		senderNames: map[string]string{},
	}
	m.viewport.Width = 60
	m.viewport.Height = 2
	m.viewport.SetContent(m.renderMessages())
	m.viewport.SetYOffset(1)
	if m.viewport.AtBottom() {
		t.Fatal("test setup must be above the bottom of the timeline")
	}

	updated, _ := m.Update(incomingMsg{msg: msgstore.Message{
		ID: "five", ChatJID: chatJID, Text: "cinco", Timestamp: time.Now().UnixMilli(),
	}})
	m = updated.(model)
	if m.viewport.YOffset != 1 {
		t.Fatalf("YOffset = %d, want preserved offset 1", m.viewport.YOffset)
	}
}

func TestSelectReplyMessageNavigatesPersistedMessages(t *testing.T) {
	m := model{
		messages: []msgstore.Message{
			{ID: "first", Text: "primer mensaje"},
			{Text: "todavía no persistido"},
			{ID: "last", Text: "último mensaje"},
		},
	}

	if !m.selectReplyMessage(-1) {
		t.Fatal("selectReplyMessage(-1) = false, want newest message")
	}
	if m.replyToID != "last" {
		t.Fatalf("replyToID = %q, want last", m.replyToID)
	}

	if !m.selectReplyMessage(-1) {
		t.Fatal("selectReplyMessage(-1) = false, want previous persisted message")
	}
	if m.replyToID != "first" {
		t.Fatalf("replyToID = %q, want first", m.replyToID)
	}

	if !m.selectReplyMessage(1) {
		t.Fatal("selectReplyMessage(1) = false, want newer persisted message")
	}
	if m.replyToID != "last" {
		t.Fatalf("replyToID = %q, want last", m.replyToID)
	}
}

func TestSearchStartsAtNewestMatchAndUpWalksOlderMatches(t *testing.T) {
	m := model{
		selectedJID: "contact@s.whatsapp.net",
		messages: []msgstore.Message{
			{ID: "old", Text: "hola mundo"},
			{ID: "middle", Text: "sin coincidencia"},
			{ID: "new", Text: "HOLA de nuevo"},
		},
		senderNames:      map[string]string{},
		searchTerm:       "hola",
		searchMatchIndex: -1,
	}
	m.viewport.Width = 60
	m.viewport.Height = 3

	if !m.moveSearchMatch(-1) {
		t.Fatal("moveSearchMatch(-1) = false, want newest match")
	}
	if m.searchMatchIndex != 2 {
		t.Fatalf("searchMatchIndex = %d, want 2", m.searchMatchIndex)
	}
	if !m.moveSearchMatch(-1) {
		t.Fatal("second moveSearchMatch(-1) = false, want older match")
	}
	if m.searchMatchIndex != 0 {
		t.Fatalf("searchMatchIndex = %d, want 0", m.searchMatchIndex)
	}
}

func TestSearchMessagesCmdReadsHistoryOutsideVisiblePage(t *testing.T) {
	const chatJID = "contact@s.whatsapp.net"
	store := msgstore.New(t.TempDir())
	for i := 0; i <= messagesPerPage; i++ {
		text := "mensaje reciente"
		if i == 0 {
			text = "coincidencia antigua"
		}
		if err := store.Append(msgstore.Message{
			ID:        fmt.Sprintf("message-%d", i),
			ChatJID:   chatJID,
			Text:      text,
			Timestamp: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The visible page would contain only the newest 100 messages, whereas a
	// Ctrl+F search must also be able to find the first persisted message.
	result := searchMessagesCmd(context.Background(), store, nil, chatJID, "coincidencia")()
	loaded, ok := result.(searchMessagesLoadedMsg)
	if !ok {
		t.Fatalf("searchMessagesCmd() returned %T, want searchMessagesLoadedMsg", result)
	}
	if loaded.err != nil {
		t.Fatalf("searchMessagesCmd() error: %v", loaded.err)
	}
	if len(loaded.msgs) != 1 {
		t.Fatalf("loaded %d messages, want only the matching message", len(loaded.msgs))
	}
	if loaded.msgs[0].Text != "coincidencia antigua" {
		t.Fatalf("oldest message = %q, want persisted match", loaded.msgs[0].Text)
	}
}

func TestCtrlFShowsSearchPromptWithoutChangingDraft(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.input.SetValue("mensaje sin enviar")

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlF})
	m = updated.(model)
	if !m.searching {
		t.Fatal("searching = false after Ctrl+F")
	}
	if got := m.input.Value(); got != "mensaje sin enviar" {
		t.Fatalf("draft = %q, want preserved", got)
	}
}

func TestDeleteSelectionOnlyTargetsOwnMessages(t *testing.T) {
	m := model{
		selectedJID: "contact@s.whatsapp.net",
		messages: []msgstore.Message{
			{ID: "received", Text: "recibido"},
			{ID: "own-old", Text: "mío anterior", FromMe: true},
			{ID: "own-new", Text: "mío reciente", FromMe: true},
		},
		senderNames: map[string]string{},
	}
	m.viewport.Width = 60
	m.viewport.Height = 3

	if !m.moveDeleteSelection(-1) {
		t.Fatal("moveDeleteSelection(-1) = false, want newest own message")
	}
	if m.selectedDeleteID != "own-new" {
		t.Fatalf("selectedDeleteID = %q, want own-new", m.selectedDeleteID)
	}
	if !m.moveDeleteSelection(-1) {
		t.Fatal("second moveDeleteSelection(-1) = false, want older own message")
	}
	if m.selectedDeleteID != "own-old" {
		t.Fatalf("selectedDeleteID = %q, want own-old", m.selectedDeleteID)
	}
}

func TestCtrlDOpensDeleteSelection(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.messages = []msgstore.Message{{ID: "own", Text: "mensaje", FromMe: true}}
	m.viewport.Width = 60
	m.viewport.Height = 3

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = updated.(model)
	if !m.deleteSelectMode || m.selectedDeleteID != "own" {
		t.Fatalf("delete selection = (%v, %q), want (true, own)", m.deleteSelectMode, m.selectedDeleteID)
	}
}

func TestForwardSelectionChoosesNewestPersistedMessageWithRawProto(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.messages = []msgstore.Message{
		{ID: "old", Text: "anterior", RawProto: "cHJvdG8="},
		{ID: "missing", Text: "sin contenido original"},
		{ID: "new", Text: "reciente", RawProto: "cHJvdG8="},
	}
	m.viewport.Width = 60
	m.viewport.Height = 3

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = updated.(model)
	if !m.forwardSelectMode || m.selectedForwardID != "new" {
		t.Fatalf("forward selection = (%v, %q), want newest forwardable message", m.forwardSelectMode, m.selectedForwardID)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(model)
	if m.selectedForwardID != "old" {
		t.Fatalf("selectedForwardID = %q, want old after up", m.selectedForwardID)
	}
}

func TestForwardSelectionMovesToDestinationWithoutChangingSourceChat(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "source@s.whatsapp.net"
	m.chats = []chatstore.Chat{
		{JID: "source@s.whatsapp.net", Name: "Origen"},
		{JID: "destination@s.whatsapp.net", Name: "Destino"},
	}
	m.messages = []msgstore.Message{{ID: "message", Text: "reenviar", RawProto: "cHJvdG8="}}
	m.forwardSelectMode = true
	m.selectedForwardID = "message"

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.forwardSelectMode || !m.forwardDestinationMode {
		t.Fatalf("forward phases = select:%v destination:%v, want destination only", m.forwardSelectMode, m.forwardDestinationMode)
	}
	if m.forwardDestinationJID != "destination@s.whatsapp.net" {
		t.Fatalf("forwardDestinationJID = %q", m.forwardDestinationJID)
	}
	if m.selectedJID != "source@s.whatsapp.net" {
		t.Fatalf("selectedJID = %q, source chat should remain visible", m.selectedJID)
	}
}

func TestForwardDestinationEscReturnsToMessageSelection(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.forwardDestinationMode = true
	m.forwardDestinationJID = "destination@s.whatsapp.net"

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.forwardDestinationMode || !m.forwardSelectMode || m.forwardDestinationJID != "" {
		t.Fatalf("forward state after Esc = destination:%v select:%v jid:%q", m.forwardDestinationMode, m.forwardSelectMode, m.forwardDestinationJID)
	}
}

func TestCtrlHOpensDocumentSelection(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.messages = []msgstore.Message{
		{ID: "older", MediaType: "document"},
		{ID: "latest", MediaType: "document/pdf"},
	}

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlH})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("Ctrl+H did not deactivate the active chat for selection")
	}
	if !m.documentSelectMode || m.selectedDocumentID != "latest" {
		t.Fatalf("document selection = (%v, %q), want latest", m.documentSelectMode, m.selectedDocumentID)
	}
}

func TestCtrlHReportsWhenChatHasNoDocuments(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.messages = []msgstore.Message{{ID: "photo", MediaType: "image"}}

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlH})
	m = updated.(model)
	if cmd != nil {
		t.Fatal("Ctrl+H started a download without a document")
	}
	if m.statusLine != "No hay documentos para descargar en este chat" {
		t.Fatalf("statusLine = %q", m.statusLine)
	}
}

func TestCtrlHSelectsNewestDocumentAndCanMoveToOlderOne(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.messages = []msgstore.Message{
		{ID: "old-document", MediaType: "document/pdf"},
		{ID: "photo", MediaType: "image"},
		{ID: "new-document", MediaType: "document"},
	}
	m.viewport.Width = 60
	m.viewport.Height = 3

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlH})
	m = updated.(model)
	if !m.documentSelectMode || m.selectedDocumentID != "new-document" {
		t.Fatalf("document selection = (%v, %q), want newest document", m.documentSelectMode, m.selectedDocumentID)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(model)
	if m.selectedDocumentID != "old-document" {
		t.Fatalf("selectedDocumentID = %q, want older document", m.selectedDocumentID)
	}
}

func TestSendResultAssignsIDToOptimisticMessage(t *testing.T) {
	m := model{
		messages: []msgstore.Message{
			{ID: "sent", FromMe: true, Text: "anterior"},
			{FromMe: true, Text: "recién enviado"},
		},
	}
	updated, _ := m.Update(sendResultMsg{messageID: "server-id"})
	m = updated.(model)
	if got := m.messages[1].ID; got != "server-id" {
		t.Fatalf("optimistic message ID = %q, want server-id", got)
	}
}

func TestBotSentReloadsOnlySelectedChat(t *testing.T) {
	const chatJID = "contact@s.whatsapp.net"
	cs := chatstore.New(t.TempDir())
	ms := msgstore.New(t.TempDir())
	botMessage := msgstore.Message{ID: "bot-id", ChatJID: chatJID, FromMe: true, Text: "hola", Timestamp: time.Now().UnixMilli()}
	if err := ms.Append(botMessage); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name       string
		selected   string
		wantReload bool
	}{
		{name: "selected chat", selected: chatJID, wantReload: true},
		{name: "other chat", selected: "other@s.whatsapp.net"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(context.Background(), nil, cs, ms, nil)
			m.selectedJID = tt.selected
			m.viewport.Width = 60
			m.viewport.Height = 2
			updated, cmd := m.Update(botSentMsg{chatJID: chatJID, messageID: botMessage.ID})
			m = updated.(model)
			if m.statusLine != "bot: mensaje enviado" {
				t.Fatalf("statusLine = %q", m.statusLine)
			}
			if cmd == nil {
				t.Fatal("missing reload command")
			}
			if !tt.wantReload {
				if _, ok := cmd().(chatsLoadedMsg); !ok {
					t.Fatal("other chat should reload only the sidebar")
				}
				return
			}
			batch, ok := cmd().(tea.BatchMsg)
			if !ok || len(batch) != 2 {
				t.Fatalf("batch = %#v, want sidebar and transcript commands", batch)
			}
			loaded := batch[1]()
			updated, _ = m.Update(loaded)
			m = updated.(model)
			if len(m.messages) != 1 || m.messages[0].ID != botMessage.ID {
				t.Fatalf("messages after reload = %#v", m.messages)
			}
			if !m.viewport.AtBottom() {
				t.Fatal("new message did not keep the viewport at the bottom")
			}
		})
	}
}

func TestBotAudioTranscriptAppearsBelowVoiceNote(t *testing.T) {
	jid := "contact@s.whatsapp.net"
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = jid
	m.viewport.Width = 70
	m.viewport.Height = 8
	m.messages = []msgstore.Message{{ID: "voice", ChatJID: jid, MediaType: "voice note"}}
	m.viewport.SetContent(m.renderMessages())
	updated, cmd := m.Update(botAudioTranscriptMsg{chatJID: jid, messageID: "voice", transcript: "hola desde el audio"})
	m = updated.(model)
	if cmd != nil {
		t.Fatal("transcript already in view should redraw without reload")
	}
	view := ansi.Strip(m.viewport.View())
	audio := strings.Index(view, "voice note")
	label := strings.Index(view, "Transcripción")
	words := strings.Index(view, "hola desde el audio")
	if audio < 0 || label <= audio || words <= label {
		t.Fatalf("transcript is not below the audio: %q", view)
	}
}

func TestDocumentCaptionWrapsInsideBubble(t *testing.T) {
	raw, err := proto.Marshal(&waProto.Message{DocumentMessage: &waProto.DocumentMessage{
		FileName: proto.String("documento.pdf"),
		Mimetype: proto.String("application/pdf"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	caption := "Hola, este es un texto de ejemplo para el pie de un documento largo\n\nSegundo párrafo de ejemplo con suficiente longitud para comprobar el ajuste dentro de la burbuja. Más texto de relleno"
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "chat@s.whatsapp.net"
	m.width, m.height = 80, 20
	m.messages = []msgstore.Message{{
		ID: "document", MediaType: "document", FromMe: true, Text: caption,
		RawProto: base64.StdEncoding.EncodeToString(raw), Timestamp: time.Now().UnixMilli(),
	}}
	m.layout()
	view := ansi.Strip(m.renderMessages())
	for _, word := range []string{"ejemplo", "burbuja", "relleno"} {
		if !strings.Contains(view, word) {
			t.Fatalf("caption word %q was cut instead of wrapped: %q", word, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > m.viewport.Width {
			t.Fatalf("line wider than viewport (%d > %d): %q", w, m.viewport.Width, line)
		}
		if strings.Contains(line, "│") && !strings.HasSuffix(strings.TrimRight(line, " "), "│") {
			t.Fatalf("bubble right border missing: %q", line)
		}
	}
}

func TestDocumentPickerMarksMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"uno.pdf", "dos.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("contenido"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := model{documentPicker: true, documentDir: dir, selectedDocuments: map[string]bool{}}
	m.loadDocumentEntries()
	if len(m.documentEntries) != 2 {
		t.Fatalf("entries = %d, want 2", len(m.documentEntries))
	}

	updated, _ := m.handleDocumentPickerKey(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(model)
	updated, _ = m.handleDocumentPickerKey(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(model)
	updated, _ = m.handleDocumentPickerKey(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(model)
	if got := lenSelectedDocuments(m.selectedDocuments); got != 2 {
		t.Fatalf("selected documents = %d, want 2", got)
	}
}

func TestQuestionMarkTogglesHelpModal(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = updated.(model)
	if !m.helpVisible {
		t.Fatal("helpVisible = false after ?")
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.helpVisible {
		t.Fatal("helpVisible = true after Esc")
	}
}

func TestPOpensAudioSelectorInsteadOfPlayingImmediately(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.messages = []msgstore.Message{{ID: "voice", MediaType: "voice note"}}
	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(model)
	if cmd != nil {
		t.Fatal("P should select audio, not start playback")
	}
	if !m.audioSelectMode || !m.audioPlayMode || m.selectedAudioID != "voice" {
		t.Fatalf("audio selector state = (%v, %v, %q), want (true, true, voice)", m.audioSelectMode, m.audioPlayMode, m.selectedAudioID)
	}
}

func TestPTypingIsPreservedWhenInputHasFocus(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.focus = focusInput
	m.input.Focus()
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(model)
	if m.audioSelectMode || m.audioPlayMode {
		t.Fatal("P should not open audio selector while typing")
	}
	if got := m.input.Value(); got != "p" {
		t.Fatalf("input = %q, want p", got)
	}
}

func TestQuestionMarkTypingIsPreservedWhenInputHasFocus(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.focus = focusInput
	m.input.Focus()
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = updated.(model)
	if m.helpVisible {
		t.Fatal("? should not open help while typing")
	}
	if got := m.input.Value(); got != "?" {
		t.Fatalf("input = %q, want ?", got)
	}
}

func TestSelectedReplyMessageClearsWhenNoLongerInHistory(t *testing.T) {
	m := model{
		replyToID: "gone",
		messages:  []msgstore.Message{{ID: "present"}},
	}
	if _, ok := m.selectedReplyMessage(); ok {
		t.Fatal("selectedReplyMessage() found a message that is not in history")
	}
}

func TestOptimisticReplyShowsItsQuoteImmediately(t *testing.T) {
	quoted := msgstore.Message{ID: "incoming", Text: "mensaje original", SenderJID: "contact@s.whatsapp.net"}
	m := model{
		replyQuotes: map[string]msgstore.Message{"optimistic": quoted},
		messages:    []msgstore.Message{quoted},
		senderNames: map[string]string{"contact@s.whatsapp.net": "Contacto"},
	}
	text, sender, quotedID, ok := m.getQuotedInfo(msgstore.Message{ID: "optimistic", FromMe: true})
	if !ok || text != "mensaje original" || sender != "Contacto" || quotedID != "incoming" {
		t.Fatalf("getQuotedInfo() = (%q, %q, %q, %v), want original text, sender, and quoted ID", text, sender, quotedID, ok)
	}
}

func TestLoadingSelectedChatDoesNotMarkItReadWithoutInputFocus(t *testing.T) {
	const chatJID = "22222222222222@lid"
	dataDir := t.TempDir()
	cs := chatstore.New(dataDir)
	ms := msgstore.New(dataDir)
	if err := cs.IncrementUnread(chatJID); err != nil {
		t.Fatal(err)
	}

	result := loadMessagesCmd(context.Background(), cs, ms, nil, chatJID, 0)()
	if _, ok := result.(messagesLoadedMsg); !ok {
		t.Fatalf("loadMessagesCmd() = %#v", result)
	}
	chat, ok, err := cs.Get(chatJID)
	if err != nil || !ok {
		t.Fatalf("Get() = (%#v, %v, %v)", chat, ok, err)
	}
	if chat.UnreadCount != 1 {
		t.Fatalf("UnreadCount = %d, want 1 while input is not focused", chat.UnreadCount)
	}
}

func TestLoadMessagesCmdPaginatesNewestFirst(t *testing.T) {
	const chatJID = "chat@s.whatsapp.net"
	ms := msgstore.New(t.TempDir())
	for i := 0; i < 250; i++ {
		if err := ms.Append(msgstore.Message{
			ID:        fmt.Sprintf("m-%03d", i),
			ChatJID:   chatJID,
			Timestamp: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	load := func(offset int) messagesLoadedMsg {
		t.Helper()
		result := loadMessagesCmd(context.Background(), nil, ms, nil, chatJID, offset)()
		page, ok := result.(messagesLoadedMsg)
		if !ok {
			t.Fatalf("loadMessagesCmd() = %#v", result)
		}
		return page
	}

	newest := load(0)
	if len(newest.msgs) != 100 || newest.msgs[0].ID != "m-150" || newest.msgs[99].ID != "m-249" || !newest.hasOlder {
		t.Fatalf("newest page = %#v", newest)
	}
	previous := load(100)
	if len(previous.msgs) != 100 || previous.msgs[0].ID != "m-050" || previous.msgs[99].ID != "m-149" || !previous.prepend || !previous.hasOlder {
		t.Fatalf("previous page = %#v", previous)
	}
	oldest := load(200)
	if len(oldest.msgs) != 50 || oldest.msgs[0].ID != "m-000" || oldest.msgs[49].ID != "m-049" || oldest.hasOlder {
		t.Fatalf("oldest page = %#v", oldest)
	}
}

func TestIncomingMessageIsReadOnlyWithInputFocus(t *testing.T) {
	const chatJID = "22222222222222@lid"
	for _, tt := range []struct {
		name            string
		focus           focusArea
		terminalBlurred bool
		want            int
	}{
		{name: "pink input focused", focus: focusInput, want: 0},
		{name: "pink input focused but terminal blurred", focus: focusInput, terminalBlurred: true, want: 1},
		{name: "chat list focused", focus: focusChatList, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := chatstore.New(t.TempDir())
			if err := cs.IncrementUnread(chatJID); err != nil {
				t.Fatal(err)
			}
			m := model{
				ctx:             context.Background(),
				cs:              cs,
				selectedJID:     chatJID,
				focus:           tt.focus,
				terminalBlurred: tt.terminalBlurred,
				lastActivity:    map[string]time.Time{},
			}
			_, cmd := m.Update(incomingMsg{msg: msgstore.Message{
				ID:        "incoming-1",
				ChatJID:   chatJID,
				SenderJID: chatJID,
				Timestamp: time.Now().UnixMilli(),
			}})
			runTeaCommand(cmd)

			chat, ok, err := cs.Get(chatJID)
			if err != nil || !ok {
				t.Fatalf("Get() = (%#v, %v, %v)", chat, ok, err)
			}
			if chat.UnreadCount != tt.want {
				t.Fatalf("UnreadCount = %d, want %d", chat.UnreadCount, tt.want)
			}
		})
	}
}

func runTeaCommand(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			if child != nil {
				child()
			}
		}
	}
}

func TestFormatReadReceipt(t *testing.T) {
	now := time.Date(2026, time.August, 5, 8, 20, 0, 0, time.Local)
	tests := []struct {
		name   string
		readAt int64
		want   string
	}{
		{name: "missing timestamp", want: "[api] view"},
		{name: "now", readAt: now.Add(-30 * time.Second).UnixMilli(), want: "[api] view now"},
		{name: "minutes", readAt: now.Add(-2 * time.Minute).UnixMilli(), want: "[api] view 2m"},
		{name: "clock time", readAt: now.Add(-2 * time.Hour).UnixMilli(), want: "[api] view 06:20"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatReadReceipt(tt.readAt, now); got != tt.want {
				t.Fatalf("formatReadReceipt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPossibleBlockWarning(t *testing.T) {
	now := time.Date(2026, time.August, 5, 9, 30, 0, 0, time.Local)
	message := func(age time.Duration, status string) msgstore.Message {
		return msgstore.Message{
			ID:        "msg-1",
			FromMe:    true,
			Timestamp: now.Add(-age).UnixMilli(),
			Status:    status,
		}
	}

	tests := []struct {
		name     string
		messages []msgstore.Message
		want     bool
	}{
		{name: "one check long enough", messages: []msgstore.Message{message(3*time.Minute, "sent")}, want: true},
		{name: "empty status is also one check", messages: []msgstore.Message{message(3*time.Minute, "")}, want: true},
		{name: "still inside grace period", messages: []msgstore.Message{message(time.Minute, "sent")}},
		{name: "delivered", messages: []msgstore.Message{message(3*time.Minute, "delivered")}},
		{name: "read", messages: []msgstore.Message{message(3*time.Minute, "read")}},
		{name: "optimistic message without server id", messages: []msgstore.Message{{FromMe: true, Timestamp: now.Add(-3 * time.Minute).UnixMilli()}}},
		{name: "recipient replied", messages: []msgstore.Message{message(3*time.Minute, "sent"), {ID: "reply", FromMe: false, Timestamp: now.UnixMilli()}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := possibleBlockWarning(tt.messages, now); got != tt.want {
				t.Fatalf("possibleBlockWarning() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLatestIncomingActivity(t *testing.T) {
	older := time.Date(2026, time.August, 5, 9, 34, 0, 0, time.Local)
	newer := older.Add(29 * time.Minute)
	messages := []msgstore.Message{
		{FromMe: false, Timestamp: older.UnixMilli()},
		{FromMe: true, Timestamp: newer.Add(time.Minute).UnixMilli()},
		{FromMe: false, Timestamp: newer.UnixMilli()},
	}

	if got := latestIncomingActivity(messages); !got.Equal(newer) {
		t.Fatalf("latestIncomingActivity() = %v, want %v", got, newer)
	}
}
