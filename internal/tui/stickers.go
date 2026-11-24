package tui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

const stickerColumns = 8
const stickerRows = 4

var stickerDiacritics = [...]rune{'\u0305', '\u030d', '\u030e', '\u0310', '\u0312', '\u033d', '\u033e', '\u033f'}

// synchronizedTerminal keeps Kitty transfer sequences intact while Bubble Tea
// writes its normal screen. The image itself is positioned by text placeholders.
type synchronizedTerminal struct {
	mu  sync.Mutex
	out io.Writer
}

var _ term.File = (*synchronizedTerminal)(nil)

// Bubble Tea requires Fd to recognize its output as a terminal and deliver
// the first WindowSizeMsg. Without it the app stays at "Loading wa-cli...".
func (t *synchronizedTerminal) Fd() uintptr {
	if file, ok := t.out.(interface{ Fd() uintptr }); ok {
		return file.Fd()
	}
	return ^uintptr(0)
}

func (t *synchronizedTerminal) Read(p []byte) (int, error) {
	if reader, ok := t.out.(io.Reader); ok {
		return reader.Read(p)
	}
	return 0, io.EOF
}

func (t *synchronizedTerminal) Close() error {
	if closer, ok := t.out.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (t *synchronizedTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out.Write(p)
}

func kittyStickersAvailable() bool {
	return os.Getenv("TMUX") == "" && (os.Getenv("KITTY_WINDOW_ID") != "" || strings.Contains(os.Getenv("TERM"), "kitty"))
}

func isStickerMessage(msg msgstore.Message) bool {
	return msg.ID != "" && strings.EqualFold(strings.TrimSpace(msg.MediaType), "sticker")
}

func stickerImageID() (uint32, error) {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, err
	}
	id := uint32(raw[0])<<16 | uint32(raw[1])<<8 | uint32(raw[2])
	if id == 0 {
		id = 1
	}
	return id, nil
}

// Kitty's Unicode placeholders behave like ordinary text inside the viewport:
// they scroll, clip and disappear with the message instead of floating on top.
func stickerPlaceholder(id uint32) string {
	var b strings.Builder
	color := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", id>>16, (id>>8)&255, id&255)
	for row := 0; row < stickerRows; row++ {
		b.WriteString(color)
		for col := 0; col < stickerColumns; col++ {
			b.WriteRune('\U0010eeee')
			b.WriteRune(stickerDiacritics[row])
			b.WriteRune(stickerDiacritics[col])
		}
		b.WriteString("\x1b[39m")
		if row < stickerRows-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func firstStickerPNG(ctx context.Context, webp []byte) ([]byte, error) {
	convertCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(convertCtx, "ffmpeg", "-v", "error", "-i", "pipe:0", "-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "pipe:1")
	cmd.Stdin = bytes.NewReader(webp)
	return cmd.Output()
}

func uploadStickerPNG(out *synchronizedTerminal, id uint32, png []byte) error {
	if out == nil {
		return fmt.Errorf("terminal gráfico no disponible")
	}
	encoded := base64.StdEncoding.EncodeToString(png)
	var transfer strings.Builder
	for offset := 0; offset < len(encoded); offset += 4096 {
		end := offset + 4096
		if end > len(encoded) {
			end = len(encoded)
		}
		more := 0
		if end < len(encoded) {
			more = 1
		}
		if offset == 0 {
			fmt.Fprintf(&transfer, "\x1b_Ga=t,f=100,i=%d,q=2,m=%d;%s\x1b\\", id, more, encoded[offset:end])
		} else {
			fmt.Fprintf(&transfer, "\x1b_Gm=%d;%s\x1b\\", more, encoded[offset:end])
		}
	}
	fmt.Fprintf(&transfer, "\x1b_Ga=p,U=1,i=%d,c=%d,r=%d,q=2\x1b\\", id, stickerColumns, stickerRows)
	_, err := io.WriteString(out, transfer.String())
	return err
}

type stickerLoadedMsg struct {
	key string
	id  uint32
	err error
}

func stickerKey(msg msgstore.Message) string { return msg.ChatJID + "\x00" + msg.ID }

func loadStickerCmd(ctx context.Context, client *whatsapp.Client, out *synchronizedTerminal, msg msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		result := stickerLoadedMsg{key: stickerKey(msg)}
		if client == nil {
			result.err = fmt.Errorf("sin conexión")
			return result
		}
		webp, _, err := client.DownloadMedia(ctx, msg)
		if err != nil {
			result.err = err
			return result
		}
		png, err := firstStickerPNG(ctx, webp)
		if err != nil {
			result.err = err
			return result
		}
		result.id, err = stickerImageID()
		if err == nil {
			err = uploadStickerPNG(out, result.id, png)
		}
		result.err = err
		return result
	}
}

func stickerPreviewLine(id uint32, width int, right bool) string {
	placeholder := stickerPlaceholder(id)
	if !right || width <= stickerColumns {
		return placeholder
	}
	padding := strings.Repeat(" ", width-stickerColumns)
	lines := strings.Split(placeholder, "\n")
	for i := range lines {
		lines[i] = padding + lines[i]
	}
	return strings.Join(lines, "\n")
}

// revealVisibleStickers snapshots the stickers on screen when the user presses
// s. Images elsewhere remain hidden and are not downloaded in the background.
func (m *model) revealVisibleStickers() tea.Cmd {
	if m.stickerTerminal == nil {
		m.statusLine = "Los stickers en pantalla requieren Kitty"
		return nil
	}
	if m.viewport.Height <= 0 || m.selectedJID == "" {
		m.statusLine = "Abre un chat para mostrar sus stickers"
		return nil
	}
	visible := 0
	for _, message := range m.messages {
		if !isStickerMessage(message) || message.RawProto == "" {
			continue
		}
		start, ok := m.messageStartLine(message.ID)
		if !ok || start > m.viewport.YOffset+m.viewport.Height || start+2 < m.viewport.YOffset {
			continue
		}
		visible++
		key := stickerKey(message)
		m.stickerReveal[key] = true
		if m.stickerImages[key] != 0 || key == m.stickerLoading {
			continue
		}
		m.stickerFailed[key] = false // pressing s explicitly retries prior failures
		queued := false
		for _, pending := range m.stickerPending {
			if stickerKey(pending) == key {
				queued = true
				break
			}
		}
		if !queued {
			m.stickerPending = append(m.stickerPending, message)
		}
	}
	if visible == 0 {
		m.statusLine = "No hay stickers visibles en pantalla"
		return nil
	}
	y := m.viewport.YOffset
	m.viewport.SetContent(m.renderMessages())
	m.viewport.SetYOffset(y)
	m.statusLine = fmt.Sprintf("Mostrando %d sticker(s) visibles...", visible)
	return m.nextPendingStickerCmd()
}

func (m *model) nextPendingStickerCmd() tea.Cmd {
	if m.stickerTerminal == nil || m.stickerLoading != "" || m.client == nil || m.client.WA == nil || !m.client.WA.IsConnected() {
		return nil
	}
	for len(m.stickerPending) > 0 {
		message := m.stickerPending[0]
		m.stickerPending = m.stickerPending[1:]
		key := stickerKey(message)
		if m.stickerImages[key] != 0 || m.stickerFailed[key] {
			continue
		}
		m.stickerLoading = key
		return loadStickerCmd(m.ctx, m.client, m.stickerTerminal, message)
	}
	return nil
}
