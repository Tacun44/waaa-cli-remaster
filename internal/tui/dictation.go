package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func dictationTickCmd(seq uint64) tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
		return dictationTickMsg{seq: seq}
	})
}

func (m *model) startDictation() bool {
	if m.dictationPendingText != "" {
		m.statusLine = "Vuelve al chat anterior para recuperar el dictado pendiente"
		return false
	}
	if m.isRecordingAudio || m.dictationProcessing {
		m.statusLine = "Espera a que termine la grabación o transcripción actual"
		return false
	}
	if m.calls != nil && m.calls.Snapshot().Busy {
		m.statusLine = "Termina la llamada antes de dictar"
		return false
	}
	if m.selectedJID == "" {
		m.statusLine = "Elige un chat antes de dictar"
		return false
	}
	whisper, modelPath := whisperPaths()
	if whisper == "" || modelPath == "" {
		m.statusLine = "Falta Whisper o el modelo en ~/.local/share/wa-cli/models"
		return false
	}
	file, err := os.CreateTemp("", "wa-cli-dictation-*.wav")
	if err != nil {
		m.statusLine = "No se pudo preparar el dictado: " + err.Error()
		return false
	}
	path := file.Name()
	_ = file.Close()
	args := []string{"-nostdin", "-y", "-loglevel", "error", "-f", "pulse", "-i", "default"}
	if runtime.GOOS == "darwin" {
		args = []string{"-nostdin", "-y", "-loglevel", "error", "-f", "avfoundation", "-i", ":0"}
	}
	args = append(args, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", path)
	cmd := exec.Command("ffmpeg", args...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = os.Remove(path)
		m.statusLine = "No se pudo abrir el micrófono: " + err.Error()
		return false
	}
	m.isDictating = true
	m.dictationCmd = cmd
	m.dictationStderr = stderr
	m.dictationPath = path
	m.dictationChatJID = m.selectedJID
	m.recordingStartTime = time.Now()
	m.dictationSeq++
	m.dictationLineCount = 0
	m.dictationSegmentCount = 0
	m.dictationCommitted = ""
	m.dictationWindowText = ""
	m.dictationPreview = ""
	m.startDictationStream()
	if m.dictationStream != nil {
		m.statusLine = "🎙 Dictando… el texto provisional aparece mientras hablas · Esc cancela"
	} else {
		m.statusLine = "🎙 Dictando… transcribiré al soltar Espacio · Esc cancela"
	}
	return true
}

func (m *model) stopDictation() (tea.Model, tea.Cmd) {
	if !m.isDictating {
		return m, nil
	}
	m.isDictating = false
	m.dictationProcessing = true
	m.lastSpaceTime = time.Time{}
	m.stopDictationStream()
	m.statusLine = "Transcribiendo dictado con Whisper local…"
	cmd, path, stderr := m.dictationCmd, m.dictationPath, m.dictationStderr
	seq, chatJID := m.dictationSeq, m.dictationChatJID
	m.dictationCmd, m.dictationStderr, m.dictationPath = nil, nil, ""
	ctx, cancel := context.WithCancel(m.ctx)
	m.dictationCancel = cancel
	return m, transcribeDictationCmd(ctx, cmd, path, stderr, seq, chatJID)
}

func (m *model) cancelDictation() {
	if !m.isDictating {
		return
	}
	m.isDictating = false
	m.dictationSeq++
	m.lastSpaceTime = time.Time{}
	m.stopDictationStream()
	if m.dictationCmd != nil && m.dictationCmd.Process != nil {
		_ = m.dictationCmd.Process.Kill()
		_ = m.dictationCmd.Wait()
	}
	_ = os.Remove(m.dictationPath)
	m.dictationCmd, m.dictationStderr, m.dictationPath = nil, nil, ""
	m.restoreDictationDraft()
	m.statusLine = "Dictado cancelado"
}

func dictationStreamPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, path := range []string{
		filepath.Join(home, ".local", "share", "wa-cli", "whisper-live-build", "bin", "whisper-stream"),
		filepath.Join(home, ".local", "share", "wa-cli", "whisper.cpp", "build", "bin", "whisper-stream"),
	} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func (m *model) startDictationStream() {
	stream := dictationStreamPath()
	if stream == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	modelPath := filepath.Join(home, ".local", "share", "wa-cli", "models", "ggml-tiny-q5_1.bin")
	if _, err := os.Stat(modelPath); err != nil {
		return
	}
	file, err := os.CreateTemp("", "wa-cli-live-*.txt")
	if err != nil {
		return
	}
	path := file.Name()
	_ = file.Close()
	cmd := exec.Command(stream, "-m", modelPath, "-l", "es", "-t", "4", "--step", "1000", "--length", "5000", "--keep", "500", "-f", path)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = os.Remove(path)
		return
	}
	m.dictationStream = cmd
	m.dictationStreamFile = path
}

func (m *model) stopDictationStream() {
	if m.dictationStream != nil && m.dictationStream.Process != nil {
		_ = m.dictationStream.Process.Kill()
		_ = m.dictationStream.Wait()
	}
	m.dictationStream = nil
	if m.dictationStreamFile != "" {
		_ = os.Remove(m.dictationStreamFile)
		m.dictationStreamFile = ""
	}
}

func (m *model) refreshDictationPreview() {
	if m.dictationStreamFile == "" {
		return
	}
	data, err := os.ReadFile(m.dictationStreamFile)
	if err != nil || len(data) == 0 {
		return
	}
	lines := strings.Split(string(data), "\n")
	if !strings.HasSuffix(string(data), "\n") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= m.dictationLineCount {
		return
	}
	for i := m.dictationLineCount; i < len(lines); i++ {
		part := strings.TrimSpace(lines[i])
		if strings.HasPrefix(part, "[") {
			if end := strings.IndexByte(part, ']'); end >= 0 {
				part = strings.TrimSpace(part[end+1:])
			}
		}
		m.dictationSegmentCount++
		if part != "" {
			m.dictationWindowText = mergeDictationWindow(m.dictationWindowText, part)
		}
		if m.dictationSegmentCount%4 == 0 {
			m.dictationCommitted = appendDictationSegment(m.dictationCommitted, m.dictationWindowText)
			m.dictationWindowText = ""
			m.dictationPreview = m.dictationCommitted
		} else {
			m.dictationPreview = appendDictationSegment(m.dictationCommitted, m.dictationWindowText)
		}
	}
	m.dictationLineCount = len(lines)
	if m.dictationPreview != "" {
		m.showDictationPreview(m.dictationPreview)
	}
}

func mergeDictationWindow(previous, next string) string {
	if previous == "" {
		return next
	}
	oldWords, newWords := strings.Fields(previous), strings.Fields(next)
	common := 0
	for common < len(oldWords) && common < len(newWords) && sameDictationWord(oldWords[common], newWords[common]) {
		common++
	}
	if common > 0 {
		if len(newWords) >= len(oldWords) {
			return next
		}
		return previous
	}
	return appendDictationSegment(previous, next)
}

func appendDictationSegment(previous, next string) string {
	previous, next = strings.TrimSpace(previous), strings.TrimSpace(next)
	if previous == "" {
		return next
	}
	if next == "" {
		return previous
	}
	oldWords, newWords := strings.Fields(previous), strings.Fields(next)
	for overlap := min(len(oldWords), len(newWords)); overlap > 0; overlap-- {
		matched := true
		for i := 0; i < overlap; i++ {
			if !sameDictationWord(oldWords[len(oldWords)-overlap+i], newWords[i]) {
				matched = false
				break
			}
		}
		if matched {
			if overlap == len(newWords) {
				return previous
			}
			return previous + " " + strings.Join(newWords[overlap:], " ")
		}
	}
	return previous + " " + next
}

func sameDictationWord(left, right string) bool {
	return strings.EqualFold(strings.Trim(left, ".,!?¿¡;:…\"'()"), strings.Trim(right, ".,!?¿¡;:…\"'()"))
}

func (m *model) showDictationPreview(text string) {
	base := []rune(m.dictationBaseDraft)
	pos := m.dictationInsertPos
	if pos < 0 || pos > len(base) {
		pos = len(base)
	}
	preview := []rune(text)
	if pos > 0 && !strings.ContainsRune(" \n", base[pos-1]) {
		preview = append([]rune{' '}, preview...)
	}
	if pos < len(base) && !strings.ContainsRune(" \n.,!?;:", base[pos]) {
		preview = append(preview, ' ')
	}
	value := append(append([]rune{}, base[:pos]...), append(preview, base[pos:]...)...)
	m.input.SetValue(string(value))
	m.input.SetCursor(pos + len(preview))
}

func (m *model) restoreDictationDraft() {
	if m.dictationPreview == "" {
		return
	}
	m.input.SetValue(m.dictationBaseDraft)
	m.input.SetCursor(m.dictationInsertPos)
	m.dictationPreview = ""
	m.dictationCommitted = ""
	m.dictationWindowText = ""
}

func transcribeDictationCmd(ctx context.Context, recorder *exec.Cmd, path string, stderr *bytes.Buffer, seq uint64, chatJID string) tea.Cmd {
	return func() tea.Msg {
		result := dictationResultMsg{seq: seq, chatJID: chatJID}
		defer os.Remove(path)
		if err := finishAudioRecording(recorder, 3*time.Second); err != nil {
			if info, statErr := os.Stat(path); statErr != nil || info.Size() <= 44 {
				result.err = fmt.Errorf("grabar audio: %v (%s)", err, lastProcessError(stderr))
				return result
			}
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() <= 44 {
			result.err = fmt.Errorf("el micrófono no produjo audio: %s", lastProcessError(stderr))
			return result
		}
		whisper, modelPath := whisperPaths()
		if whisper == "" || modelPath == "" {
			result.err = fmt.Errorf("falta Whisper o el modelo")
			return result
		}
		if home, err := os.UserHomeDir(); err == nil {
			fastModel := filepath.Join(home, ".local", "share", "wa-cli", "models", "ggml-base-q5_1.bin")
			if info, err := os.Stat(fastModel); err == nil && !info.IsDir() {
				modelPath = fastModel
			}
		}
		outputBase := path + "-result"
		defer os.Remove(outputBase + ".txt")
		transcribeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(transcribeCtx, whisper, "-m", modelPath, "-f", path, "-l", "es", "-t", "8", "-bs", "1", "-bo", "1", "-np", "-nt", "-otxt", "-of", outputBase)
		if output, err := command.CombinedOutput(); err != nil {
			result.err = fmt.Errorf("Whisper: %v (%s)", err, lastProcessError(bytes.NewBuffer(output)))
			return result
		}
		data, err := os.ReadFile(outputBase + ".txt")
		if err != nil {
			result.err = err
			return result
		}
		result.text = strings.TrimSpace(string(data))
		if result.text == "" {
			result.err = fmt.Errorf("Whisper no reconoció palabras")
		}
		return result
	}
}

func (m *model) removeDictationWarmupSpaces(count int) {
	value := []rune(m.input.Value())
	pos := m.input.Position()
	if count <= 0 || pos < count || pos > len(value) {
		return
	}
	for _, r := range value[pos-count : pos] {
		if r != ' ' {
			return
		}
	}
	value = append(value[:pos-count], value[pos:]...)
	m.input.SetValue(string(value))
	m.input.SetCursor(pos - count)
}

func (m *model) insertDictation(transcript string) {
	value := []rune(m.input.Value())
	pos := m.input.Position()
	if pos < 0 || pos > len(value) {
		pos = len(value)
	}
	text := []rune(strings.TrimSpace(transcript))
	if pos > 0 && !strings.ContainsRune(" \n", value[pos-1]) {
		text = append([]rune{' '}, text...)
	}
	if pos < len(value) && !strings.ContainsRune(" \n.,!?;:", value[pos]) {
		text = append(text, ' ')
	}
	value = append(value[:pos], append(text, value[pos:]...)...)
	m.input.SetValue(string(value))
	m.input.SetCursor(pos + len(text))
}
