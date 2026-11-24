// Package metrics records local, content-free TUI usage events.
package metrics

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	SessionStart    = 1
	SessionEnd      = 2
	Focus           = 3
	Blur            = 4
	IdleStart       = 5
	IdleEnd         = 6
	ChatOpen        = 10
	ImageOpen       = 20
	VideoOpen       = 21
	DocumentOpen    = 22
	AudioPlayStart  = 30
	AudioPlayEnd    = 31
	TranscriptShown = 32
	ComposeStart    = 40
	Send            = 41
	ComposeAbandon  = 42
	DictationUsed   = 43
	MediaSend       = 44
	ReplyQuote      = 45
	ComposeKeys     = 46
)

const description = `1 session_start a=pid; 2 session_end; 3 focus; 4 blur; 5 idle_start; 6 idle_end
10 chat_open a=unread_count
20 image_open, 21 video_open, 22 document_open: a=message_timestamp_ms b=from_me
30 audio_play_start a=duration_seconds b=message_timestamp_ms; 31 audio_play_end a=listened_ms b=completed; 32 transcript_shown
40 compose_start; 41 send a=compose_ms b=characters; 42 compose_abandon a=compose_ms b=characters
43 dictation_used a=dictation_ms b=characters; 44 media_send a=1:image 2:video 3:audio 4:sticker 5:document; 45 reply_quote; 46 compose_keys a=keys b=backspaces`

type event struct {
	ts        int64
	kind      int
	chat, msg string
	a, b      int64
}

type Recorder struct {
	mu      sync.Mutex
	queue   chan event
	done    chan struct{}
	closed  bool
	logOnce sync.Once
	logf    func(error)
}

// Open starts one background writer. WA_METRICS=0 is a complete no-op.
func Open(dataDir string, logf func(error)) *Recorder {
	if os.Getenv("WA_METRICS") == "0" {
		return nil
	}
	r := &Recorder{queue: make(chan event, 1024), done: make(chan struct{}), logf: logf}
	go r.run(filepath.Join(dataDir, "metrics.db"))
	return r
}

func (r *Recorder) Record(kind int, chatJID, msgID string, a, b int64) {
	r.RecordAt(time.Now(), kind, chatJID, msgID, a, b)
}

func (r *Recorder) RecordAt(at time.Time, kind int, chatJID, msgID string, a, b int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.queue <- event{at.UnixMilli(), kind, chatJID, msgID, a, b}:
	default:
	}
}

// Close drains accepted events, appends session_end, and waits for the writer.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
}

func (r *Recorder) fail(err error) {
	if err != nil {
		r.logOnce.Do(func() {
			if r.logf != nil {
				r.logf(err)
			}
		})
	}
}

func (r *Recorder) run(path string) {
	defer close(r.done)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.fail(err)
		for range r.queue {
		}
		return
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		r.fail(err)
		for range r.queue {
		}
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS chats (id INTEGER PRIMARY KEY, jid TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS events (ts INTEGER NOT NULL, kind INTEGER NOT NULL, chat INTEGER, msg TEXT, a INTEGER, b INTEGER)`,
		`CREATE INDEX IF NOT EXISTS events_ts ON events(ts)`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT OR REPLACE INTO meta(key,value) VALUES ('schema_version','1')`,
		`INSERT OR REPLACE INTO meta(key,value) VALUES ('kinds',?)`,
	} {
		var e error
		if q == `INSERT OR REPLACE INTO meta(key,value) VALUES ('kinds',?)` {
			_, e = db.Exec(q, description)
		} else {
			_, e = db.Exec(q)
		}
		if e != nil {
			r.fail(e)
			for range r.queue {
			}
			return
		}
	}
	ids := map[string]int64{}
	batch := make([]event, 0, 100)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		defer func() { batch = batch[:0] }()
		tx, err := db.Begin()
		if err != nil {
			r.fail(err)
			return
		}
		for _, e := range batch {
			var chat any
			if e.chat != "" {
				id, ok := ids[e.chat]
				if !ok {
					if _, err = tx.Exec(`INSERT OR IGNORE INTO chats(jid) VALUES (?)`, e.chat); err != nil {
						break
					}
					err = tx.QueryRow(`SELECT id FROM chats WHERE jid=?`, e.chat).Scan(&id)
					if err != nil {
						break
					}
					ids[e.chat] = id
				}
				chat = id
			}
			var msg any
			if e.msg != "" {
				msg = e.msg
			}
			if _, err = tx.Exec(`INSERT INTO events(ts,kind,chat,msg,a,b) VALUES (?,?,?,?,?,?)`, e.ts, e.kind, chat, msg, e.a, e.b); err != nil {
				break
			}
		}
		if err != nil {
			_ = tx.Rollback()
			// Chat ids learned inside the rolled-back transaction are invalid.
			ids = map[string]int64{}
			r.fail(err)
			return
		}
		r.fail(tx.Commit())
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case e, ok := <-r.queue:
			if !ok {
				batch = append(batch, event{ts: time.Now().UnixMilli(), kind: SessionEnd})
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
