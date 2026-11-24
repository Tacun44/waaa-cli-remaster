package msgstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPlayedReceiptMarkerPersistsAndLegacyPlaybackMigrates(t *testing.T) {
	dir := t.TempDir()
	jid := "a@s.whatsapp.net"
	s := New(dir)
	if err := s.Append(Message{ID: "new", ChatJID: jid, MediaType: "voice note"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPlayed(jid, "new"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPlayedReceiptSent(jid, "new"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := New(dir).Get(jid, "new")
	if err != nil || !ok || !got.Played || !got.PlayedReceiptSent {
		t.Fatalf("persisted played receipt = (%+v, %v, %v)", got, ok, err)
	}

	legacyDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(legacyDir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE messages (row_id INTEGER PRIMARY KEY, id TEXT, chat_jid TEXT, sender_jid TEXT, timestamp INTEGER, text TEXT, from_me INTEGER, media_type TEXT, raw_proto TEXT, transcript TEXT, status TEXT, read_at INTEGER, played INTEGER);
		INSERT INTO messages VALUES (1, 'old', 'a@s.whatsapp.net', 'a@s.whatsapp.net', 1, '', 0, 'voice note', '', '', '', 0, 1);`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	got, ok, err = New(legacyDir).Get(jid, "old")
	if err != nil || !ok || !got.PlayedReceiptSent {
		t.Fatalf("legacy played receipt = (%+v, %v, %v)", got, ok, err)
	}
}

func TestAppendThenList(t *testing.T) {
	s := New(t.TempDir())
	jid := "a@s.whatsapp.net"

	_ = s.Append(Message{ID: "1", ChatJID: jid, Text: "hello", Timestamp: 100})
	_ = s.Append(Message{ID: "2", ChatJID: jid, Text: "world", Timestamp: 200})

	msgs, err := s.List(jid)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Text != "hello" || msgs[1].Text != "world" {
		t.Fatalf("unexpected order: %+v", msgs)
	}
}

func TestSetTranscriptPersistsOnAudioWithoutReplacingReceipt(t *testing.T) {
	dir := t.TempDir()
	jid := "a@s.whatsapp.net"
	s := New(dir)
	if err := s.Append(Message{ID: "voice", ChatJID: jid, MediaType: "voice note"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPlayed(jid, "voice"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTranscript(jid, "voice", "hola desde el audio"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := New(dir).Get(jid, "voice")
	if err != nil || !ok || got.Transcript != "hola desde el audio" || !got.Played {
		t.Fatalf("stored audio after reopening = (%+v, %v, %v)", got, ok, err)
	}
}

func TestListDualReadOnly(t *testing.T) {
	s := New(t.TempDir())
	jid := "a@s.whatsapp.net"
	for _, msg := range []Message{
		{ID: "one", ChatJID: jid, Text: "first"},
		{ID: "two", ChatJID: jid, Text: "second"},
	} {
		if err := s.Append(msg); err != nil {
			t.Fatal(err)
		}
	}

	msgs, err := s.ListDualReadOnly(jid, "")
	if err != nil {
		t.Fatalf("ListDualReadOnly: %v", err)
	}
	if len(msgs) != 2 || msgs[0].ID != "one" || msgs[1].ID != "two" {
		t.Fatalf("messages = %#v, want both messages in order", msgs)
	}
}

func TestSearchReadOnlyReturnsOnlyNewestMatches(t *testing.T) {
	s := New(t.TempDir())
	jid := "a@s.whatsapp.net"
	for i, text := range []string{"match old", "unrelated", "match new", "match newest"} {
		if err := s.Append(Message{ID: fmt.Sprintf("message-%d", i), ChatJID: jid, Text: text, Timestamp: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	msgs, err := s.SearchReadOnly(jid, "match", 2)
	if err != nil {
		t.Fatalf("SearchReadOnly: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Text != "match new" || msgs[1].Text != "match newest" {
		t.Fatalf("messages = %#v, want the two newest matching messages", msgs)
	}
}

func TestListPreservesArrivalOrderWhenWhatsAppTimestampArrivesLate(t *testing.T) {
	s := New(t.TempDir())
	jid := "a@s.whatsapp.net"
	// A remote event can arrive after an outgoing message while carrying an
	// earlier server timestamp. The visible timeline must preserve append order.
	for _, msg := range []Message{
		{ID: "mar-1", ChatJID: jid, Text: "Mar 1", Timestamp: 1710000000000},
		{ID: "me-2", ChatJID: jid, Text: "Yo 2", Timestamp: 1710000000750, FromMe: true},
		{ID: "mar-3", ChatJID: jid, Text: "Mar 3", Timestamp: 1710000000000},
	} {
		if err := s.Append(msg); err != nil {
			t.Fatal(err)
		}
	}

	msgs, _, err := s.ListDualPage(jid, "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].ID != "mar-1" || msgs[1].ID != "me-2" || msgs[2].ID != "mar-3" {
		t.Fatalf("message order = %#v, want Mar 1, Yo 2, Mar 3", msgs)
	}
}

func TestAppend_TrimsToMaxPerChat(t *testing.T) {
	s := New(t.TempDir())
	jid := "a@s.whatsapp.net"

	limit := 50
	if MaxPerChat < limit {
		limit = MaxPerChat
	}

	for i := 0; i < limit+10; i++ {
		_ = s.Append(Message{ID: fmt.Sprintf("msg-%d", i), ChatJID: jid, Timestamp: int64(i)})
	}

	msgs, err := s.List(jid)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	expected := limit + 10
	if MaxPerChat < expected {
		expected = MaxPerChat
	}
	if len(msgs) != expected {
		t.Fatalf("expected %d messages, got %d", expected, len(msgs))
	}
}

func TestList_UnknownChatReturnsEmpty(t *testing.T) {
	s := New(t.TempDir())
	msgs, err := s.List("nonexistent@s.whatsapp.net")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected empty, got %d messages", len(msgs))
	}
}

func TestGet_FindsByID(t *testing.T) {
	s := New(t.TempDir())
	jid := "a@s.whatsapp.net"
	_ = s.Append(Message{ID: "abc123", ChatJID: jid, Text: "target"})
	_ = s.Append(Message{ID: "def456", ChatJID: jid, Text: "other"})

	msg, ok, err := s.Get(jid, "abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || msg.Text != "target" {
		t.Fatalf("expected to find target message, got ok=%v msg=%+v", ok, msg)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := New(t.TempDir())
	_, ok, err := s.Get("a@s.whatsapp.net", "nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected not found")
	}
}

func TestChatsAreIsolated(t *testing.T) {
	s := New(t.TempDir())
	_ = s.Append(Message{ID: "1", ChatJID: "a@s.whatsapp.net", Text: "a-msg"})
	_ = s.Append(Message{ID: "2", ChatJID: "b@s.whatsapp.net", Text: "b-msg"})

	aMsgs, _ := s.List("a@s.whatsapp.net")
	bMsgs, _ := s.List("b@s.whatsapp.net")

	if len(aMsgs) != 1 || len(bMsgs) != 1 {
		t.Fatalf("expected 1 message each, got a=%d b=%d", len(aMsgs), len(bMsgs))
	}
}

func TestPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	jid := "a@s.whatsapp.net"

	s1 := New(dir)
	_ = s1.Append(Message{ID: "1", ChatJID: jid, Text: "hi"})

	s2 := New(dir)
	msgs, err := s2.List(jid)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("expected state to persist, got %+v", msgs)
	}
}

func TestAppend_DeduplicatesByID(t *testing.T) {
	s := New(t.TempDir())
	jid := "a@s.whatsapp.net"

	_ = s.Append(Message{ID: "m1", ChatJID: jid, Text: "original", Timestamp: 100})
	_ = s.Append(Message{ID: "m1", ChatJID: jid, Text: "updated", Timestamp: 105})

	msgs, err := s.List(jid)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message due to deduplication, got %d", len(msgs))
	}
	if msgs[0].Text != "updated" {
		t.Fatalf("expected updated text, got %q", msgs[0].Text)
	}
}

func TestMigratesEveryLegacyRecordIncludingDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	jid := "a@s.whatsapp.net"
	legacy := map[string][]Message{
		jid: {
			{ID: "same-id", ChatJID: jid, Text: "first", Timestamp: 1},
			{ID: "same-id", ChatJID: jid, Text: "second", Timestamp: 2},
			{ID: "other", ChatJID: jid, Text: "third", Timestamp: 3},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "messages.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	msgs, err := New(dir).List(jid)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("migration retained %d messages, want 3", len(msgs))
	}
	if msgs[0].Text != "first" || msgs[1].Text != "second" || msgs[2].Text != "third" {
		t.Fatalf("unexpected migrated messages: %+v", msgs)
	}
}
