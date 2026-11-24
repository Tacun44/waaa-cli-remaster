package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestDictationResultStaysInDraftUntilEnter(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "chat-a"
	m.focus = focusInput
	m.input.SetValue("Hola mundo")
	m.input.SetCursor(5)
	m.dictationSeq = 1
	m.dictationProcessing = true

	updated, _ := m.Update(dictationResultMsg{seq: 1, chatJID: "chat-a", text: "voz"})
	got := updated.(model)
	if got.input.Value() != "Hola voz mundo" {
		t.Fatalf("borrador = %q", got.input.Value())
	}
	if len(got.messages) != 0 || got.dictationProcessing {
		t.Fatal("el dictado se envió o quedó procesando")
	}
}

func TestDictationWaitsForOriginalChat(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "chat-b"
	m.dictationChatJID = "chat-a"
	m.dictationSeq = 1
	m.dictationProcessing = true

	updated, _ := m.Update(dictationResultMsg{seq: 1, chatJID: "chat-a", text: "mensaje privado"})
	got := updated.(model)
	if got.input.Value() != "" || got.dictationPendingText == "" {
		t.Fatal("el dictado se insertó en el chat equivocado o se perdió")
	}
	got.selectedJID = "chat-a"
	got.focus = focusInput
	updated, _ = got.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	got = updated.(model)
	if got.input.Value() != "mensaje privado" || got.dictationPendingText != "" {
		t.Fatalf("borrador recuperado = %q, pendiente = %q", got.input.Value(), got.dictationPendingText)
	}
}

func TestDictationWarmupPreservesExistingText(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.input.SetValue("Antes    después")
	m.input.SetCursor(9)
	m.removeDictationWarmupSpaces(4)
	if m.input.Value() != "Antesdespués" || m.input.Position() != 5 {
		t.Fatalf("borrador = %q, cursor = %d", m.input.Value(), m.input.Position())
	}
}

func TestLiveDictationPreviewIsReplacedByFinalTranscript(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "chat-a"
	m.dictationChatJID = "chat-a"
	m.dictationSeq = 1
	m.isDictating = true
	m.focus = focusInput
	m.input.SetValue("Buenos")
	m.dictationBaseDraft = m.input.Value()
	m.dictationInsertPos = m.input.Position()
	path := filepath.Join(t.TempDir(), "stream.txt")
	m.dictationStreamFile = path
	if err := os.WriteFile(path, []byte("hola\nhola mun\nhola mundo\nhola mundo\nqué\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.refreshDictationPreview()
	if m.input.Value() != "Buenos hola mundo qué" {
		t.Fatalf("vista previa = %q", m.input.Value())
	}
	m.isDictating = false
	m.dictationProcessing = true
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if len(m.messages) != 0 || m.input.Value() != "Buenos hola mundo qué" {
		t.Fatal("Enter envió el texto provisional")
	}
	updated, _ = m.Update(dictationResultMsg{seq: 1, chatJID: "chat-a", text: "hola mundo, ¿qué tal?"})
	m = updated.(model)
	if m.input.Value() != "Buenos hola mundo, ¿qué tal?" || len(m.messages) != 0 {
		t.Fatalf("borrador final = %q, mensajes = %d", m.input.Value(), len(m.messages))
	}
}

func TestDictationKeepsPreviewIfFinalTranscriptionFails(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.selectedJID = "chat-a"
	m.dictationSeq = 1
	m.dictationProcessing = true
	m.focus = focusInput
	m.dictationBaseDraft = "Buenos"
	m.dictationInsertPos = len([]rune(m.dictationBaseDraft))
	m.dictationPreview = "días"
	m.showDictationPreview(m.dictationPreview)

	updated, _ := m.Update(dictationResultMsg{seq: 1, chatJID: "chat-a", err: errors.New("timeout")})
	got := updated.(model)
	if got.input.Value() != "Buenos días" || got.dictationProcessing || len(got.messages) != 0 {
		t.Fatalf("borrador después del error = %q, procesando = %v", got.input.Value(), got.dictationProcessing)
	}
}

func TestLiveDictationDoesNotDropFirstWordsAsWindowSlides(t *testing.T) {
	m := newModel(context.Background(), nil, nil, nil, nil)
	m.dictationBaseDraft = ""
	m.dictationInsertPos = 0
	path := filepath.Join(t.TempDir(), "stream.txt")
	m.dictationStreamFile = path
	var output string
	for _, partial := range []string{
		"Hola buenos días", "buenos días cómo", "días cómo estás", "cómo estás hoy", "", "hoy amigos", "amigos míos", "míos queridos",
	} {
		output += partial + "\n"
		if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
			t.Fatal(err)
		}
		m.refreshDictationPreview()
		if !strings.HasPrefix(m.input.Value(), "Hola buenos días") {
			t.Fatalf("el inicio desapareció con %q: %q", partial, m.input.Value())
		}
	}
	if got := m.input.Value(); got != "Hola buenos días cómo estás hoy amigos míos queridos" {
		t.Fatalf("vista previa final = %q", got)
	}
}
