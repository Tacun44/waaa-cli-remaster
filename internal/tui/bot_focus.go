package tui

import (
	"os"
	"path/filepath"

	"github.com/polymorfa/hypermeow/types"
)

// syncBotFocus publishes the chat only while its composer and terminal are focused.
func syncBotFocus(path, chatJID string, focused bool) error {
	if path == "" {
		return nil
	}
	if !focused || chatJID == "" {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	jid, err := types.ParseJID(chatJID)
	if err != nil || jid.IsEmpty() {
		return syncBotFocus(path, "", false)
	}
	want := jid.ToNonAD().String()
	if current, err := os.ReadFile(path); err == nil && string(current) == want {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bot_focus-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.WriteString(want); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
