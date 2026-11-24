package metrics

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecorderFlushesAndCachesChats(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir, func(err error) { t.Errorf("metrics writer: %v", err) })
	at := time.UnixMilli(123456789)
	r.RecordAt(at, ChatOpen, "alice@s.whatsapp.net", "", 3, 0)
	r.RecordAt(at.Add(time.Millisecond), ImageOpen, "alice@s.whatsapp.net", "msg-1", 123, 1)
	r.Close()
	db, err := sql.Open("sqlite", filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM chats`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("chats=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM events`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("events=%d err=%v", count, err)
	}
	var ts, kind, a, b int64
	var jid, msg string
	err = db.QueryRow(`SELECT e.ts,e.kind,c.jid,e.msg,e.a,e.b FROM events e JOIN chats c ON e.chat=c.id WHERE e.kind=?`, ImageOpen).Scan(&ts, &kind, &jid, &msg, &a, &b)
	if err != nil || ts != at.Add(time.Millisecond).UnixMilli() || kind != ImageOpen || jid != "alice@s.whatsapp.net" || msg != "msg-1" || a != 123 || b != 1 {
		t.Fatalf("event=%d %d %q %q %d %d err=%v", ts, kind, jid, msg, a, b, err)
	}
	var version string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "1" {
		t.Fatalf("version=%q err=%v", version, err)
	}
}

func TestDisabledCreatesNoFile(t *testing.T) {
	t.Setenv("WA_METRICS", "0")
	dir := t.TempDir()
	r := Open(dir, nil)
	r.Record(ChatOpen, "chat", "", 0, 0)
	r.Close()
	if _, err := os.Stat(filepath.Join(dir, "metrics.db")); !os.IsNotExist(err) {
		t.Fatalf("metrics.db exists or stat failed: %v", err)
	}
}

func TestFullQueueDoesNotBlock(t *testing.T) {
	r := &Recorder{queue: make(chan event, 1), done: make(chan struct{})}
	r.Record(ChatOpen, "chat", "", 0, 0)
	done := make(chan struct{})
	go func() { r.Record(ChatOpen, "chat", "", 0, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Record blocked on full queue")
	}
	if len(r.queue) != 1 {
		t.Fatalf("queue length=%d", len(r.queue))
	}
}
