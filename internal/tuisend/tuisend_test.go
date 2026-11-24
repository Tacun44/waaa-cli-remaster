package tuisend

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polymorfa/hypermeow/types"
)

func TestRequestKindJSON(t *testing.T) {
	for _, tc := range []struct {
		name, payload, kind, quotedID string
		composing                     bool
	}{
		{"legacy media", `{"jid":"123@s.whatsapp.net","path":"/tmp/video.mp4","mime_type":"video/mp4"}`, "", "", false},
		{"explicit media", `{"kind":"media"}`, "media", "", false},
		{"text", `{"kind":"text","text":"hello"}`, "text", "", false},
		{"quoted text", `{"kind":"text","text":"hello","quoted_id":"ABC123"}`, "text", "ABC123", false},
		{"composing", `{"kind":"chat_presence","composing":true}`, "chat_presence", "", true},
		{"paused", `{"kind":"chat_presence","composing":false}`, "chat_presence", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request Request
			if err := json.Unmarshal([]byte(tc.payload), &request); err != nil {
				t.Fatal(err)
			}
			if request.Kind != tc.kind || request.Composing != tc.composing || request.QuotedID != tc.quotedID {
				t.Fatalf("decoded kind=%q composing=%t quoted_id=%q", request.Kind, request.Composing, request.QuotedID)
			}
			if tc.name == "legacy media" && (request.Path != "/tmp/video.mp4" || request.MimeType != "video/mp4") {
				t.Fatalf("legacy media fields lost: %+v", request)
			}
			if tc.name == "text" && request.Text != "hello" {
				t.Fatalf("text = %q", request.Text)
			}
		})
	}
}

func TestCheckBotRecipientRereadsBotOn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot_on")
	parse := func(raw string) types.JID {
		t.Helper()
		jid, err := types.ParseJID(raw)
		if err != nil {
			t.Fatal(err)
		}
		return jid
	}
	check := func(name, raw string, allowed bool) {
		t.Helper()
		err := CheckBotRecipient(dir, parse(raw))
		if allowed && err != nil || !allowed && (err == nil || err.Error() != "recipient not allowed for bot sends") {
			t.Fatalf("%s: CheckBotRecipient(%q) = %v, allowed=%v", name, raw, err, allowed)
		}
	}
	check("missing", "111@s.whatsapp.net", false)
	if err := os.WriteFile(path, []byte("222@s.whatsapp.net"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("other chat", "111@s.whatsapp.net", false)
	if err := os.WriteFile(path, []byte("111@s.whatsapp.net"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("matching", "111@s.whatsapp.net", true)
	check("device suffix", "111:7@s.whatsapp.net", true)
	if err := os.WriteFile(path, []byte("123@g.us"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("group", "123@g.us", false)
}

func TestSendUsesRunningTUIAndReturnsItsResult(t *testing.T) {
	dir := t.TempDir()
	want := Request{JID: "123@s.whatsapp.net", Path: "/tmp/video.mp4", MimeType: "video/mp4"}
	close, err := Listen(dir, func(got Request) error {
		if got != want {
			t.Errorf("request = %+v, want %+v", got, want)
		}
		return errors.New("upload failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	handled, err := Send(dir, want)
	if !handled || err == nil || !strings.Contains(err.Error(), "upload failed") {
		t.Fatalf("Send = (%t, %v), want handled upload error", handled, err)
	}
}

func TestSendFallsBackOnlyWithoutListener(t *testing.T) {
	handled, err := Send(t.TempDir(), Request{})
	if handled || err != nil {
		t.Fatalf("Send = (%t, %v), want unhandled without error", handled, err)
	}
}
