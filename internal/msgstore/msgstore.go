// Package msgstore persists recent messages per chat. It uses SQLite so a
// message append or a paged history read never needs to rewrite the complete
// local history.
package msgstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// MaxPerChat caps how many recent messages are kept per chat.
const MaxPerChat = 100000

// Message is one stored message.
type Message struct {
	ID                string `json:"id"`
	ChatJID           string `json:"chatJid"`
	SenderJID         string `json:"senderJid"`
	Timestamp         int64  `json:"timestamp"`
	Text              string `json:"text"`
	FromMe            bool   `json:"fromMe"`
	MediaType         string `json:"mediaType,omitempty"`
	RawProto          string `json:"rawProto,omitempty"`
	Transcript        string `json:"transcript,omitempty"`
	Status            string `json:"status,omitempty"`
	ReadAt            int64  `json:"readAt,omitempty"`
	Played            bool   `json:"played,omitempty"`
	PlayedReceiptSent bool   `json:"playedReceiptSent,omitempty"`
}

// Store is a SQLite-backed per-chat message log. legacyPath is only used once
// to import installations created before the SQLite store existed.
type Store struct {
	path       string
	legacyPath string
	mu         sync.Mutex
	db         *sql.DB
}

func New(dataDir string) *Store {
	return &Store{
		path:       filepath.Join(dataDir, "messages.db"),
		legacyPath: filepath.Join(dataDir, "messages.json"),
	}
}

func (s *Store) open() (*sql.DB, error) {
	if s.db != nil {
		return s.db, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, fmt.Errorf("creating message store directory: %w", err)
	}
	// Changing journal_mode is a write operation. Doing it in the open URI
	// caused every fresh CLI process to attempt that write again, including
	// read-only operations such as Ctrl+F. Existing stores were already
	// created in WAL mode, so request it only when this is a new database.
	_, statErr := os.Stat(s.path)
	newDatabase := os.IsNotExist(statErr)
	if statErr != nil && !newDatabase {
		return nil, fmt.Errorf("checking message store: %w", statErr)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)", s.path))
	if err != nil {
		return nil, fmt.Errorf("opening message store: %w", err)
	}
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			row_id INTEGER PRIMARY KEY,
			id TEXT NOT NULL DEFAULT '', chat_jid TEXT NOT NULL, sender_jid TEXT NOT NULL,
			timestamp INTEGER NOT NULL, text TEXT NOT NULL, from_me INTEGER NOT NULL,
			media_type TEXT NOT NULL DEFAULT '', raw_proto TEXT NOT NULL DEFAULT '',
			transcript TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', read_at INTEGER NOT NULL DEFAULT 0,
			played INTEGER NOT NULL DEFAULT 0, played_receipt_sent INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS messages_chat_timestamp ON messages(chat_jid, timestamp, row_id);
		CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating message store schema: %w", err)
	}
	// Existing installations predate the local "already listened" marker.
	// SQLite has no ADD COLUMN IF NOT EXISTS, so only ignore that exact case.
	if _, err = db.Exec(`ALTER TABLE messages ADD COLUMN played INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		_ = db.Close()
		return nil, fmt.Errorf("adding played marker: %w", err)
	}
	if _, err = db.Exec(`ALTER TABLE messages ADD COLUMN played_receipt_sent INTEGER NOT NULL DEFAULT 0`); err == nil {
		// Older versions sent played receipts as soon as playback started.
		if _, err = db.Exec(`UPDATE messages SET played_receipt_sent=1 WHERE played=1`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrating played receipts: %w", err)
		}
	} else if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		_ = db.Close()
		return nil, fmt.Errorf("adding played receipt marker: %w", err)
	}
	if newDatabase {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("setting message store journal mode: %w", err)
		}
	}
	// Do not rebuild indexes at every application start. Aside from needlessly
	// writing the whole database, an interrupted rebuild can make opening the
	// message store fail before the UI loads any history. The timestamp index
	// above is sufficient for all timeline reads; lookups by ID remain correct
	// without a separate chat_jid/id index.
	if err = s.importLegacy(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = s.reconcileLegacy(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.db = db
	return db, nil
}

func (s *Store) importLegacy(db *sql.DB) error {
	var done string
	err := db.QueryRow(`SELECT value FROM metadata WHERE key = 'legacy-json-imported'`).Scan(&done)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("checking message-store migration: %w", err)
	}
	data, err := os.ReadFile(s.legacyPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading legacy message store: %w", err)
	}
	legacy := map[string][]Message{}
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &legacy); err != nil {
			return fmt.Errorf("parsing legacy message store: %w", err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for chatJID, messages := range legacy {
		for _, msg := range messages {
			if msg.ChatJID == "" {
				msg.ChatJID = chatJID
			}
			// Legacy JSON may contain repeated WhatsApp IDs from historic syncs.
			// Import every record verbatim; collapsing them would silently lose
			// history during a migration.
			if err := insert(tx, msg); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrating legacy messages: %w", err)
			}
		}
	}
	if _, err = tx.Exec(`INSERT INTO metadata(key, value) VALUES('legacy-json-imported', '1')`); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// reconcileLegacy makes upgrades from the first SQLite build lossless. It
// preserves any database-only records while restoring every JSON record that
// an earlier UNIQUE message-ID index may have collapsed.
func (s *Store) reconcileLegacy(db *sql.DB) error {
	var done string
	err := db.QueryRow(`SELECT value FROM metadata WHERE key = 'legacy-json-reconciled-v2'`).Scan(&done)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("checking message-store reconciliation: %w", err)
	}
	data, err := os.ReadFile(s.legacyPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading legacy message store for reconciliation: %w", err)
	}
	legacy := map[string][]Message{}
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &legacy); err != nil {
			return fmt.Errorf("parsing legacy message store for reconciliation: %w", err)
		}
	}
	rows, err := db.Query(`SELECT ` + messageColumns + ` FROM messages`)
	if err != nil {
		return err
	}
	existing, err := collect(rows)
	_ = rows.Close()
	if err != nil {
		return err
	}
	available := make(map[string]int, len(existing))
	for _, msg := range existing {
		available[messageKey(msg)]++
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for chatJID, messages := range legacy {
		for _, msg := range messages {
			if msg.ChatJID == "" {
				msg.ChatJID = chatJID
			}
			key := messageKey(msg)
			if available[key] > 0 {
				available[key]--
				continue
			}
			if err := insert(tx, msg); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("reconciling legacy messages: %w", err)
			}
		}
	}
	if _, err = tx.Exec(`INSERT INTO metadata(key, value) VALUES('legacy-json-reconciled-v2', '1')`); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func messageKey(msg Message) string {
	data, _ := json.Marshal(msg)
	return string(data)
}

func upsert(tx *sql.Tx, msg Message) error {
	if msg.ID != "" {
		result, err := tx.Exec(`UPDATE messages SET sender_jid=?, timestamp=?, text=?, from_me=?, media_type=?, raw_proto=?, transcript=?, status=?, read_at=?, played=played OR ?, played_receipt_sent=played_receipt_sent OR ? WHERE row_id=(SELECT row_id FROM messages WHERE chat_jid=? AND id=? ORDER BY row_id LIMIT 1)`,
			msg.SenderJID, msg.Timestamp, msg.Text, msg.FromMe, msg.MediaType, msg.RawProto, msg.Transcript, msg.Status, msg.ReadAt, msg.Played, msg.PlayedReceiptSent, msg.ChatJID, msg.ID)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed > 0 {
			return nil
		}
	}
	return insert(tx, msg)
}

func insert(tx *sql.Tx, msg Message) error {
	_, err := tx.Exec(`INSERT INTO messages(id, chat_jid, sender_jid, timestamp, text, from_me, media_type, raw_proto, transcript, status, read_at, played, played_receipt_sent) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		msg.ID, msg.ChatJID, msg.SenderJID, msg.Timestamp, msg.Text, msg.FromMe, msg.MediaType, msg.RawProto, msg.Transcript, msg.Status, msg.ReadAt, msg.Played, msg.PlayedReceiptSent)
	return err
}

func (s *Store) Append(msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err = upsert(tx, msg); err == nil {
		_, err = tx.Exec(`DELETE FROM messages WHERE row_id IN (SELECT row_id FROM messages WHERE chat_jid=? ORDER BY row_id DESC LIMIT -1 OFFSET ?)`, msg.ChatJID, MaxPerChat)
	}
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("writing message store: %w", err)
	}
	return tx.Commit()
}

func scanMessage(scanner interface{ Scan(...any) error }) (Message, error) {
	var msg Message
	var fromMe, played, playedReceiptSent int
	err := scanner.Scan(&msg.ID, &msg.ChatJID, &msg.SenderJID, &msg.Timestamp, &msg.Text, &fromMe, &msg.MediaType, &msg.RawProto, &msg.Transcript, &msg.Status, &msg.ReadAt, &played, &playedReceiptSent)
	msg.FromMe = fromMe != 0
	msg.Played = played != 0
	msg.PlayedReceiptSent = playedReceiptSent != 0
	return msg, err
}

const messageColumns = `id, chat_jid, sender_jid, timestamp, text, from_me, media_type, raw_proto, transcript, status, read_at, played, played_receipt_sent`

func (s *Store) List(chatJID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+messageColumns+` FROM messages WHERE chat_jid=? ORDER BY row_id`, chatJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// ListDual returns both known JID variants, deduplicated and oldest first.
func (s *Store) ListDual(chatJID, altJID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := dualQuery(db, chatJID, altJID, `ORDER BY row_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return dedupe(collect(rows))
}

// ListDualReadOnly reads a complete chat history through an isolated
// read-only SQLite connection. It is intended for operations such as search:
// they must remain available even when the long-lived writer connection is
// busy receiving messages or has just encountered a write error. The ordering
// matches the chat/timestamp index so SQLite does not create a large temporary
// table while searching an entire conversation.
func (s *Store) ListDualReadOnly(chatJID, altJID string) ([]Message, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(10000)", s.path))
	if err != nil {
		return nil, fmt.Errorf("opening read-only message store: %w", err)
	}
	defer db.Close()
	rows, err := dualQuery(db, chatJID, altJID, `ORDER BY timestamp, row_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return dedupe(collect(rows))
}

// SearchReadOnly returns only matching messages from one chat. Keeping the
// filtering in SQLite prevents the TUI from loading and rendering an entire
// conversation just to locate a short phrase.
func (s *Store) SearchReadOnly(chatJID, query string, limit int) ([]Message, error) {
	if limit < 1 {
		limit = 1
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(10000)", s.path))
	if err != nil {
		return nil, fmt.Errorf("opening read-only message store: %w", err)
	}
	defer db.Close()
	pattern := "%" + escapeLike(query) + "%"
	rows, err := db.Query(`SELECT `+messageColumns+` FROM messages
		WHERE chat_jid=? AND (text LIKE ? ESCAPE '\' COLLATE NOCASE OR transcript LIKE ? ESCAPE '\' COLLATE NOCASE)
		ORDER BY timestamp DESC, row_id DESC LIMIT ?`, chatJID, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages, err := collect(rows)
	if err != nil {
		return nil, err
	}
	// The indexed query reads newest first so it can stop at the requested
	// limit. Restore chronological order for the transcript renderer.
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, nil
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

// ListDualPage reads only one page from the newest end of a chat. It is the
// fast path used by the TUI; it avoids constructing/rendering full histories.
func (s *Store) ListDualPage(chatJID, altJID string, offset, limit int) ([]Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset < 0 {
		offset = 0
	}
	if limit < 1 {
		limit = 1
	}
	db, err := s.open()
	if err != nil {
		return nil, false, err
	}
	query := `ORDER BY row_id DESC LIMIT ? OFFSET ?`
	rows, err := dualQuery(db, chatJID, altJID, query, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	messages, err := collect(rows)
	if err != nil {
		return nil, false, err
	}
	hasOlder := len(messages) > limit
	if hasOlder {
		messages = messages[:limit]
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, hasOlder, nil
}

func dualQuery(db *sql.DB, chatJID, altJID, suffix string, extras ...any) (*sql.Rows, error) {
	args := []any{chatJID}
	where := `chat_jid=?`
	if altJID != "" && altJID != chatJID {
		where += ` OR chat_jid=?`
		args = append(args, altJID)
	}
	args = append(args, extras...)
	return db.Query(`SELECT `+messageColumns+` FROM messages WHERE `+where+` `+suffix, args...)
}

func collect(rows *sql.Rows) ([]Message, error) {
	var messages []Message
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func dedupe(messages []Message, err error) ([]Message, error) {
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := make([]Message, 0, len(messages))
	for _, msg := range messages {
		key := msg.ID
		if key == "" {
			key = fmt.Sprintf("%d-%s", msg.Timestamp, msg.Text)
		}
		if !seen[key] {
			seen[key] = true
			result = append(result, msg)
		}
	}
	// Keep the order supplied by the query: row_id is the sequence in which the
	// client observed or sent the messages. WhatsApp can deliver a message late
	// with an earlier server timestamp, so timestamp sorting can put a reply
	// before the local message that triggered it.
	return result, nil
}

func (s *Store) UpdateStatus(chatJIDs []string, msgIDs []string, status string, readAt int64) error {
	if len(msgIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return err
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(msgIDs)), ",")
	args := []any{status, readAt}
	args = append(args, stringSliceToAny(msgIDs)...)
	query := `UPDATE messages SET status=?, read_at=CASE WHEN ? > 0 THEN ? ELSE read_at END WHERE id IN (` + placeholders + `)`
	// Match both LID/PN forms as the previous JSON implementation did.
	args = []any{status, readAt, readAt}
	args = append(args, stringSliceToAny(msgIDs)...)
	if len(chatJIDs) > 0 {
		conditions := make([]string, 0, len(chatJIDs))
		for _, jid := range chatJIDs {
			conditions = append(conditions, `(chat_jid=? OR substr(chat_jid, 1, instr(chat_jid, '@')-1)=?)`)
			args = append(args, jid, strings.Split(jid, "@")[0])
		}
		query += ` AND (` + strings.Join(conditions, ` OR `) + `)`
	}
	_, err = db.Exec(query, args...)
	return err
}

// MarkPlayed persists the local fact that the user started listening to an
// audio. It is intentionally separate from WhatsApp delivery/read receipts.
func (s *Store) MarkPlayed(chatJID, msgID string) error {
	if chatJID == "" || msgID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE messages SET played=1 WHERE chat_jid=? AND id=?`, chatJID, msgID)
	return err
}

// MarkPlayedReceiptSent records a successful WhatsApp played receipt.
func (s *Store) MarkPlayedReceiptSent(chatJID, msgID string) error {
	if chatJID == "" || msgID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE messages SET played_receipt_sent=1 WHERE chat_jid=? AND id=?`, chatJID, msgID)
	return err
}

func stringSliceToAny(values []string) []any {
	result := make([]any, len(values))
	for i := range values {
		result[i] = values[i]
	}
	return result
}

func (s *Store) Get(chatJID, id string) (Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return Message{}, false, err
	}
	msg, err := scanMessage(db.QueryRow(`SELECT `+messageColumns+` FROM messages WHERE chat_jid=? AND id=? LIMIT 1`, chatJID, id))
	if err == sql.ErrNoRows {
		return Message{}, false, nil
	}
	return msg, err == nil, err
}

// SetTranscript updates only the transcript, preserving receipt and status
// changes that may have arrived while transcription was in progress.
func (s *Store) SetTranscript(chatJID, id, transcript string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return err
	}
	result, err := db.Exec(`UPDATE messages SET transcript=? WHERE chat_jid=? AND id=? AND from_me=0 AND media_type IN ('voice note', 'audio')`, transcript, chatJID, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("incoming audio not found")
	}
	return nil
}
