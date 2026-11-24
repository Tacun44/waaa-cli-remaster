package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow/types"
)

func isBotToggleKey(key string) bool {
	switch key {
	case "ctrl+ñ", "alt+ñ", "ctrl+b":
		return true
	}
	return false
}

func normalizedChatJID(raw string) string {
	jid, err := types.ParseJID(raw)
	if err != nil || jid.IsEmpty() {
		return ""
	}
	return jid.ToNonAD().String()
}

func controlBotService(on bool) error {
	action := "stop"
	if on {
		action = "start"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", "--user", action, "wa-bot.service").CombinedOutput()
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return fmt.Errorf("%s wa-bot.service: %w: %s", action, err, detail)
		}
		return fmt.Errorf("%s wa-bot.service: %w", action, err)
	}
	return nil
}

func (m model) handleBotToggleKey(key string) (model, bool) {
	if !isBotToggleKey(key) || m.renameJID != "" || m.helpVisible || m.contextMenuOpen ||
		m.deleteConfirm || m.forwardConfirm || m.documentPicker || m.callPanel {
		return m, false
	}
	jid, err := types.ParseJID(m.selectedJID)
	if m.selectedJID == "" || err != nil || jid.IsEmpty() ||
		(jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
		m.statusLine = "el bot solo funciona en chats individuales"
		return m, true
	}
	previous, previousErr := os.ReadFile(m.botPath)
	if previousErr != nil && !os.IsNotExist(previousErr) {
		m.statusLine = "No se pudo leer el estado del bot: " + previousErr.Error()
		return m, true
	}
	if previousErr == nil && string(previous) == jid.ToNonAD().String() {
		m.botLevelPending = false
		m.botLevelChat = ""
	} else {
		m.botLevelPending = true
		m.botLevelChat = jid.ToNonAD().String()
		m.statusLine = "Nivel del bot: 1 alto (rápido) · 2 medio · 3 bajo (hasta 2 min) · Esc cancelar"
		return m, true
	}
	on, err := toggleBotFile(m.botPath, jid.ToNonAD().String())
	if err != nil {
		m.statusLine = "No se pudo cambiar el estado del bot: " + err.Error()
	} else {
		if m.botServiceControl != nil {
			if err := m.botServiceControl(on); err != nil {
				if on {
					if previousErr == nil {
						_ = os.WriteFile(m.botPath, previous, 0o600)
					} else {
						_ = os.Remove(m.botPath)
					}
				}
				m.statusLine = "No se pudo iniciar o detener el bot: " + err.Error()
				return m, true
			}
		}
		if on {
			m.statusLine = "bot activado"
		} else {
			m.statusLine = "bot desactivado"
		}
	}
	return m, true
}

func (m model) handleBotLevelKey(key string) (model, bool) {
	if !m.botLevelPending {
		return m, false
	}
	if key == "esc" || key == "ctrl+b" {
		m.botLevelPending = false
		m.botLevelChat = ""
		m.statusLine = "Activación del bot cancelada"
		return m, true
	}
	if key != "1" && key != "2" && key != "3" {
		return m, true
	}
	if normalizedChatJID(m.selectedJID) != m.botLevelChat {
		m.botLevelPending = false
		m.botLevelChat = ""
		m.statusLine = "Selecciona de nuevo el nivel en el chat actual"
		return m, true
	}
	if err := os.WriteFile(m.botPath+".level", []byte(key), 0o600); err != nil {
		m.statusLine = "No se pudo guardar el nivel del bot: " + err.Error()
		return m, true
	}
	jid := m.botLevelChat
	m.botLevelPending = false
	m.botLevelChat = ""
	on, err := toggleBotFile(m.botPath, jid)
	if err != nil {
		m.statusLine = "No se pudo activar el bot: " + err.Error()
		return m, true
	}
	if on && m.botServiceControl != nil {
		if err := m.botServiceControl(true); err != nil {
			_ = os.Remove(m.botPath)
			m.statusLine = "No se pudo iniciar el bot: " + err.Error()
			return m, true
		}
	}
	m.statusLine = "bot activado · nivel " + key
	return m, true
}

// toggleBotFile uses the marker file as the source of truth for bot state.
func toggleBotFile(path, jid string) (bool, error) {
	content, err := os.ReadFile(path)
	if err == nil && string(content) == jid {
		if err := os.Remove(path); err != nil {
			return true, err
		}
		return false, nil
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := os.WriteFile(path, []byte(jid), 0o600); err != nil {
		return false, err
	}
	return true, nil
}
