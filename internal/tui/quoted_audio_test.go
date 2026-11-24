package tui

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	waProto "github.com/polymorfa/hypermeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

func audioProto(seconds uint32) string {
	data, _ := proto.Marshal(&waProto.Message{
		AudioMessage: &waProto.AudioMessage{Seconds: proto.Uint32(seconds), PTT: proto.Bool(true)},
	})
	return base64.StdEncoding.EncodeToString(data)
}

func TestQuotedVoiceShowsDurationWithoutOriginalLoaded(t *testing.T) {
	quoted := &waProto.Message{AudioMessage: &waProto.AudioMessage{
		Seconds: proto.Uint32(73), PTT: proto.Bool(true),
	}}
	raw, err := proto.Marshal(&waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text: proto.String("respuesta"), ContextInfo: &waProto.ContextInfo{
			StanzaID: proto.String("audio-1"), QuotedMessage: quoted,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, nil, nil, nil)
	text, _, id, ok := m.getQuotedInfo(msgstore.Message{ID: "reply-1", RawProto: base64.StdEncoding.EncodeToString(raw)})
	if !ok || id != "audio-1" || text != "Audio · 1:13" {
		t.Fatalf("quote = (%q, %q, %t), want Audio · 1:13", text, id, ok)
	}
}

func TestJumpHighlightClearsOnlyAfterLatestTimer(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "123@s.whatsapp.net"
	m.messages = []msgstore.Message{{ID: "audio-1", ChatJID: m.selectedJID, MediaType: "voice note", RawProto: audioProto(42)}}
	m.viewport.Width, m.viewport.Height = 70, 10
	m.viewport.SetContent(m.renderMessages())
	if !m.jumpToMessage("audio-1") || !strings.Contains(m.renderMessages(), "┃") {
		t.Fatal("jump did not highlight the target audio")
	}
	first := m.jumpHighlightSeq
	m.jumpToMessage("audio-1")
	updated, _ := m.Update(clearJumpHighlightMsg{seq: first})
	m = updated.(model)
	if m.highlightedMessageID == "" {
		t.Fatal("an older timer cleared the newer highlight")
	}
	updated, _ = m.Update(clearJumpHighlightMsg{seq: m.jumpHighlightSeq})
	m = updated.(model)
	if m.highlightedMessageID != "" || strings.Contains(m.renderMessages(), "┃") {
		t.Fatal("highlight did not disappear")
	}
}
