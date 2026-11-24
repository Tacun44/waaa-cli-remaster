// Package tuisend lets CLI media commands reuse the running TUI's WhatsApp
// connection. WhatsApp permits only one active connection per device.
package tuisend

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/polymorfa/hypermeow/types"
)

type Request struct {
	Kind      string `json:"kind"`
	JID       string `json:"jid"`
	Text      string `json:"text"`
	QuotedID  string `json:"quoted_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Composing bool   `json:"composing"`
	Path      string `json:"path"`
	MimeType  string `json:"mime_type"`
	Caption   string `json:"caption"`
}

// CheckBotRecipient rereads bot_on for every bot request.
func CheckBotRecipient(dataDir string, jid types.JID) error {
	active, err := os.ReadFile(filepath.Join(dataDir, "bot_on"))
	if err != nil || jid.IsEmpty() || jid.Server == types.GroupServer ||
		string(active) != jid.ToNonAD().String() {
		return errors.New("recipient not allowed for bot sends")
	}
	return nil
}

type response struct {
	Error string `json:"error,omitempty"`
}

func socketPath(dataDir string) string { return filepath.Join(dataDir, "tui-send.sock") }

// Send returns handled=false only when no TUI is listening. A request accepted
// by the TUI must never fall back to a second connection, even if sending fails.
func Send(dataDir string, request Request) (handled bool, err error) {
	conn, err := net.DialTimeout("unix", socketPath(dataDir), 500*time.Millisecond)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return true, fmt.Errorf("connecting to running TUI: %w", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return true, err
	}
	var result response
	if err := json.NewDecoder(conn).Decode(&result); err != nil {
		return true, fmt.Errorf("waiting for TUI send result: %w", err)
	}
	if result.Error != "" {
		return true, errors.New(result.Error)
	}
	return true, nil
}

// Listen owns the socket until close is called. The socket is private to the
// current user and is removed on normal TUI exit.
func Listen(dataDir string, send func(Request) error) (close func(), err error) {
	path := socketPath(dataDir)
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("TUI send path is not a socket: %s", path)
		}
		conn, dialErr := net.DialTimeout("unix", path, 500*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("another TUI is already listening at %s", path)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("checking TUI send socket: %w", dialErr)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var request Request
				if err := json.NewDecoder(conn).Decode(&request); err != nil {
					_ = json.NewEncoder(conn).Encode(response{Error: err.Error()})
					return
				}
				result := response{}
				if err := send(request); err != nil {
					result.Error = err.Error()
				}
				_ = json.NewEncoder(conn).Encode(result)
			}(conn)
		}
	}()
	return func() { listener.Close(); _ = os.Remove(path) }, nil
}
