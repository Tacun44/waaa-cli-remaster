// Package tui implements wa-cli's full-screen terminal interface,
// launched by running `wa` with no subcommand.
//
// Architecture: whatsmeow's connection stays open for the life of the
// program (like `wa watch`), but instead of printing to stdout directly,
// incoming messages are bridged into Bubble Tea's own event loop via
// tea.Program.Send from internal/whatsapp.Client's OnIncomingMessage
// callback. All chatstore/msgstore reads and the actual send happen in
// tea.Cmd functions (Bubble Tea's mechanism for async work), never
// directly in Update — keeping the UI responsive during network calls.
package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	zone "github.com/lrstanley/bubblezone"
	"github.com/muesli/termenv"
	waProto "github.com/polymorfa/hypermeow/binary/proto"
	"github.com/polymorfa/hypermeow/types"
	waLog "github.com/polymorfa/hypermeow/util/log"
	overlay "github.com/rmhubbert/bubbletea-overlay"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/app"
	"github.com/codebyoketch/wa-cli/internal/calls"
	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/metrics"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/notify"
	"github.com/codebyoketch/wa-cli/internal/safety"
	"github.com/codebyoketch/wa-cli/internal/store"
	"github.com/codebyoketch/wa-cli/internal/tuisend"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// init (not Run) initializes bubblezone's global manager so that
// renderMessages can call zone.Mark/zone.Scan from any caller, including unit
// tests that build a model and call renderMessages directly without going
// through Run — zone.Mark panics on an uninitialized manager.
func init() {
	zone.NewGlobal()
}

// Run connects, launches the full-screen TUI, and blocks until the user
// quits (Ctrl+C or q). Requires an existing login (`wa login`).
func Run(a *app.App) error {
	ctx := context.Background()
	lipgloss.SetColorProfile(termenv.TrueColor)
	botPath := filepath.Join(a.Config.DataDir, "bot_on")
	if err := os.Remove(botPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("desactivar bot al iniciar: %w", err)
	}
	_ = controlBotService(false)
	defer func() {
		_ = os.Remove(botPath)
		_ = controlBotService(false)
	}()
	botFocusPath := filepath.Join(a.Config.DataDir, "bot_focus")
	if err := os.Remove(botFocusPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("limpiar foco del bot al iniciar: %w", err)
	}
	defer os.Remove(botFocusPath)

	// whatsmeow logs warnings/errors directly to stdout by default,
	// which corrupts Bubble Tea's alt-screen rendering (raw writes
	// desync Bubble Tea's tracking of what's already on screen, showing
	// up as garbled/duplicated boxes). Redirect stdout to a log file
	// before constructing any waLog logger — including the one
	// internal/whatsapp.New builds for the whatsmeow client itself —
	// then hand Bubble Tea the real terminal explicitly so its own
	// rendering is unaffected by the swap.
	oldFd, err := syscall.Dup(int(os.Stdout.Fd()))
	var ttyOut *os.File
	if err == nil {
		ttyOut = os.NewFile(uintptr(oldFd), "/dev/stdout")
	} else {
		ttyOut = os.Stdout
	}

	logPath := filepath.Join(a.Config.DataDir, "tui.log")
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_ = syscall.Dup2(int(logFile.Fd()), int(os.Stdout.Fd()))
		os.Stdout = logFile
		defer func() {
			logFile.Close()
		}()
	}

	dbLog := waLog.Stdout("Database", "WARN", true)

	container, err := store.Open(ctx, a.Config.DataDir, dbLog)
	if err != nil {
		return err
	}

	cs := chatstore.New(a.Config.DataDir)
	ms := msgstore.New(a.Config.DataDir)
	guard := safety.New(a.Config.DataDir)

	client, err := whatsapp.New(ctx, container, dbLog, cs, ms)
	if err != nil {
		return err
	}
	if !client.IsLoggedIn() {
		return fmt.Errorf("not logged in: run 'wa login' first")
	}
	client.SetNotifications(a.Config.NotifyEnabled, a.Config.NotifyGroups, a.Config.NotifyShowPreview)

	defer zone.Close()

	m := newModel(ctx, client, cs, ms, guard)
	m.botPath = botPath
	m.botServiceControl = controlBotService
	m.botFocusPath = botFocusPath
	m.metrics = metrics.Open(a.Config.DataDir, func(err error) { fmt.Fprintln(os.Stdout, "metrics:", err) })
	m.metrics.Record(metrics.SessionStart, "", "", int64(os.Getpid()), 0)
	defer m.metrics.Close()
	callManager := calls.New(client.WA)
	m.calls = callManager
	m.callNotify = a.Config.NotifyEnabled
	m.callNotifyPreview = a.Config.NotifyShowPreview
	terminal := &synchronizedTerminal{out: ttyOut}
	if kittyStickersAvailable() {
		m.stickerTerminal = terminal
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithReportFocus(), tea.WithMouseCellMotion(), tea.WithOutput(terminal))

	client.OnIncomingMessage(func(msg msgstore.Message) {
		p.Send(incomingMsg{msg: msg})
	})
	client.OnConnectionChange(func(connected bool, reason string) {
		p.Send(connectionMsg{connected: connected, reason: reason})
	})
	client.OnPresence(func(pres whatsapp.PresenceState) {
		p.Send(presenceMsg{presence: pres})
	})
	client.OnReceipt(func(chatJID string, msgIDs []string, status string, readAt time.Time) {
		p.Send(receiptMsg{chatJID: chatJID, msgIDs: msgIDs, status: status, readAt: readAt})
	})

	if err := client.WA.Connect(); err != nil {
		return fmt.Errorf("connecting to WhatsApp: %w", err)
	}
	closeSendSocket, err := tuisend.Listen(a.Config.DataDir, func(request tuisend.Request) error {
		switch request.Kind {
		case "", "media", "text", "chat_presence", "transcribe_audio":
		default:
			return fmt.Errorf("unsupported TUI send kind: %q", request.Kind)
		}
		jid, err := types.ParseJID(request.JID)
		if err != nil {
			return fmt.Errorf("invalid recipient: %w", err)
		}
		switch request.Kind {
		case "text", "chat_presence", "transcribe_audio":
			if jid.IsEmpty() {
				return fmt.Errorf("invalid recipient: empty JID")
			}
			if err := tuisend.CheckBotRecipient(a.Config.DataDir, jid); err != nil {
				return err
			}
			if request.Kind == "transcribe_audio" {
				transcript, err := transcribeBotAudio(ctx, client, ms, jid.ToNonAD().String(), request.MessageID)
				if err == nil {
					p.Send(botAudioTranscriptMsg{chatJID: jid.ToNonAD().String(), messageID: request.MessageID, transcript: transcript})
				}
				return err
			}
			if request.Kind == "text" {
				quoted, isReply, err := resolveBotTextRequest(request, jid, ms.Get)
				if err != nil {
					return err
				}
				var messageID string
				if isReply {
					messageID, err = client.SendReply(ctx, jid, request.Text, quoted)
				} else {
					messageID, err = client.SendText(ctx, jid, request.Text)
				}
				if err == nil {
					m.metrics.Record(metrics.Send, jid.ToNonAD().String(), messageID, 0, int64(len([]rune(request.Text))))
					p.Send(botSentMsg{chatJID: jid.ToNonAD().String(), messageID: messageID})
				}
				return err
			}
			if request.Composing {
				if err := client.AnnounceAvailable(ctx); err != nil {
					return err
				}
			}
			return client.SetChatPresence(ctx, jid, request.Composing)
		default: // Empty kind and "media" retain the existing video send path.
			data, err := os.ReadFile(request.Path)
			if err != nil {
				return fmt.Errorf("reading video: %w", err)
			}
			if request.MimeType != "video/mp4" {
				return fmt.Errorf("unsupported video type: %s", request.MimeType)
			}
			_, err = client.SendVideo(ctx, jid, data, request.MimeType, request.Caption)
			if err == nil {
				m.metrics.Record(metrics.MediaSend, request.JID, "", 2, 0)
			}
			return err
		}
	})
	if err != nil {
		client.Disconnect()
		return fmt.Errorf("starting TUI send socket: %w", err)
	}
	defer closeSendSocket()
	defer func() {
		callManager.Close()
		_ = client.AnnounceUnavailable(context.Background())
		client.Disconnect()
	}()

	finalModel, err := p.Run()
	_ = os.Remove(botPath)
	_ = controlBotService(false)
	if fm, ok := finalModel.(model); ok {
		fm.endAudio(false)
		stopAudioPlayer(fm.audioPlayer)
		fm.abandonCompose()
	}
	return err
}

// resolveBotTextRequest validates text sends and looks up an optional quote in
// the same chat key used for bot send notifications.
func resolveBotTextRequest(request tuisend.Request, jid types.JID, get func(string, string) (msgstore.Message, bool, error)) (msgstore.Message, bool, error) {
	if request.Text == "" {
		return msgstore.Message{}, false, fmt.Errorf("text must not be empty")
	}
	if request.QuotedID == "" {
		return msgstore.Message{}, false, nil
	}
	quoted, found, err := get(jid.ToNonAD().String(), request.QuotedID)
	if err != nil {
		return msgstore.Message{}, false, err
	}
	if !found {
		return msgstore.Message{}, false, fmt.Errorf("quoted message not found")
	}
	return quoted, true, nil
}

type focusArea int

const (
	focusChatList focusArea = iota
	focusInput
)

type model struct {
	calls             *calls.Manager
	callState         calls.Snapshot
	callPanel         bool
	callTarget        string
	callActionBusy    bool
	callNotify        bool
	callNotifyPreview bool
	ctx               context.Context
	client            *whatsapp.Client
	cs                *chatstore.Store
	ms                *msgstore.Store
	guard             *safety.Guard
	metrics           *metrics.Recorder
	lastInput         time.Time
	idle              bool
	composeAt         time.Time
	composeKeys       int64
	composeBackspaces int64
	audioStarted      time.Time
	audioPausedAt     time.Time
	audioPausedTotal  time.Duration
	audioChatJID      string

	chats        []chatstore.Chat
	selectedJID  string // "" means none selected yet
	chatNumbers  map[string]string
	renameJID    string
	renameInput  textinput.Model
	renameSaving bool

	// senderNames maps a JID to its best-known display name (from local
	// contacts), used to show a real name instead of a generic "them"
	// label — mainly matters in groups, where messages come from
	// several different people.
	senderNames map[string]string
	// groupNames maps a group JID to its real name, fetched via
	// ListGroups since chatstore's own name field for groups only ever
	// comes from HistorySync, which isn't reliable.
	groupNames map[string]string
	presence   map[string]whatsapp.PresenceState
	// lastActivity tracks definite activity from received messages. It is
	// used only when WhatsApp withholds the contact's official last-seen.
	lastActivity map[string]time.Time

	// messages is only the visible history window. Keeping it bounded prevents
	// a chat with years of traffic from being rendered on every redraw.
	messages      []msgstore.Message
	messageOffset int  // number of newest persisted messages already loaded
	hasOlder      bool // true when another history page is available
	loadingOlder  bool
	// replyToID identifies the message the next text send will quote. An
	// ID, rather than a slice index, keeps the selection valid if new
	// messages arrive while the user is composing a reply.
	replyToID string
	// replyQuotes keeps the quote for an optimistic outgoing message until
	// WhatsApp/store data replaces it with the server-backed message.
	replyQuotes    map[string]msgstore.Message
	transcriptSeen map[string]bool
	// editingMessageID is set while the composer holds an existing message's
	// text for editing rather than a brand-new one; Enter then edits instead
	// of sending, and it takes priority over replyToID for that one Enter.
	editingMessageID string

	viewport viewport.Model
	// followBottom keeps the history pinned to the newest message. It only
	// turns off when a handler moves the scroll offset away from the bottom
	// (wheel, PgUp, jump, search); content or height changes never clear it.
	followBottom         bool
	highlightedMessageID string
	jumpHighlightSeq     uint64
	input                textinput.Model
	// Search is deliberately separate from the composer: Ctrl+F must never
	// replace a draft message while asking for the word to find.
	searchInput      textinput.Model
	searching        bool
	searchTerm       string
	searchMatchIndex int
	// Delete selection only targets messages sent by this account: a revoke
	// removes content for everyone and must never be offered for received text.
	deleteSelectMode bool
	selectedDeleteID string
	deleteConfirm    bool
	deleteConfirmed  bool
	// Forwarding has two selection phases: first a persisted source message,
	// then a destination chat. Keeping the destination separate preserves the
	// visible source conversation until the action completes.
	forwardSelectMode      bool
	selectedForwardID      string
	forwardDestinationMode bool
	forwardDestinationJID  string
	forwardConfirm         bool
	forwardConfirmed       bool
	// Copy selection is independent from replies/deletion, so copying a
	// message never changes the message that would be quoted on send.
	copySelectMode     bool
	selectedCopyID     string
	imageSelectMode    bool
	selectedImageID    string
	videoSelectMode    bool
	selectedVideoID    string
	documentSelectMode bool
	selectedDocumentID string
	documentPicker     bool
	documentDir        string
	documentEntries    []os.DirEntry
	documentCursor     int
	selectedDocuments  map[string]bool
	helpVisible        bool
	// Right-click context menu: opened by a mouse right-click on a message
	// bubble (bubblezone resolves which one), closed by Esc, a left click
	// outside it, or by committing one of its actions.
	contextMenuOpen   bool
	contextMenuMsgID  string
	contextMenuItems  []contextMenuItem
	contextMenuCursor int
	contextMenuClickX int
	contextMenuClickY int
	// Multi-select: started from the context menu's "Seleccionar varios",
	// then grown by clicking (left or right) more message bubbles. f/c/Ctrl+D
	// run the batched version of forward/copy/delete on everything marked.
	multiSelectMode bool
	multiSelectIDs  map[string]bool

	focus              focusArea
	terminalBlurred    bool
	keepPresenceOnBlur bool
	sidebarHidden      bool
	width, height      int
	statusLine         string
	botPath            string
	botServiceControl  func(bool) error
	botLevelPending    bool
	botLevelChat       string
	botFocusPath       string
	connected          bool
	connectionKnown    bool
	connectionNotice   string
	transcribingID     string // at most one on-demand transcription at a time
	// Audio selector node: ↑/↓ walks voice notes without changing chats; the
	// stable message ID keeps the marker valid across redraws and reloads.
	audioSelectMode      bool
	selectedAudioID      string
	audioPlayMode        bool   // true: Enter reproduces; false: Enter transcribes
	playingAudioID       string // message currently owned by mpv
	audioPaused          bool
	audioPlayer          *exec.Cmd
	audioPlaySeq         uint64 // ignores players superseded while downloading
	playedReceiptPending map[string]bool
	mediaPreviews        map[string]string // msgID -> rendered preview
	stickerTerminal      *synchronizedTerminal
	stickerImages        map[string]uint32
	stickerFailed        map[string]bool
	stickerReveal        map[string]bool
	stickerPending       []msgstore.Message
	stickerLoading       string
	mediaPaths           map[string]string // msgID -> temp file path
	mediaHidden          map[string]bool   // msgID -> true if user toggled preview hidden
	ready                bool
	quitting             bool

	isRecordingAudio      bool
	recordingCmd          *exec.Cmd
	recordingStderr       *bytes.Buffer
	audioFilePath         string
	recordingStartTime    time.Time
	lastSpaceTime         time.Time
	spaceRepeatCount      int
	spaceRunStart         time.Time
	spaceRapidCount       int
	isDictating           bool
	dictationProcessing   bool
	dictationCmd          *exec.Cmd
	dictationStderr       *bytes.Buffer
	dictationPath         string
	dictationSeq          uint64
	dictationChatJID      string
	dictationPendingText  string
	dictationCancel       context.CancelFunc
	dictationStream       *exec.Cmd
	dictationStreamFile   string
	dictationLineCount    int
	dictationSegmentCount int
	dictationCommitted    string
	dictationWindowText   string
	dictationPreview      string
	dictationBaseDraft    string
	dictationInsertPos    int
}

// --- tea.Msg types for async results ---

type chatsLoadedMsg struct{ chats []chatstore.Chat }
type contactsLoadedMsg struct{ names map[string]string }
type chatNumbersLoadedMsg struct{ numbers map[string]string }
type renameResultMsg struct {
	jid  string
	name string
	err  error
}
type groupsLoadedMsg struct{ names map[string]string }
type messagesLoadedMsg struct {
	chatJID  string
	msgs     []msgstore.Message
	offset   int
	prepend  bool
	hasOlder bool
}

// searchMessagesLoadedMsg contains the complete locally persisted history for
// the active chat. Search uses it so Ctrl+F is not limited to the visible page.
type searchMessagesLoadedMsg struct {
	chatJID string
	msgs    []msgstore.Message
	err     error
}
type incomingMsg struct{ msg msgstore.Message }
type botSentMsg struct {
	chatJID   string
	messageID string
}
type connectionMsg struct {
	connected bool
	reason    string
}
type clearJumpHighlightMsg struct{ seq uint64 }

func clearJumpHighlightCmd(seq uint64) tea.Cmd {
	return tea.Tick(2200*time.Millisecond, func(time.Time) tea.Msg {
		return clearJumpHighlightMsg{seq: seq}
	})
}

type sendResultMsg struct {
	err        error
	reply      bool
	messageID  string
	chatJID    string
	composeMS  int64
	charCount  int64
	keyCount   int64
	backspaces int64
	quotedID   string
}
type deleteResultMsg struct {
	message msgstore.Message
	err     error
}
type editResultMsg struct {
	message msgstore.Message
	err     error
}
type forwardResultMsg struct{ err error }
type sendDocumentsResultMsg struct {
	sent    int
	chatJID string
	err     error
}
type sendDroppedFilesResultMsg struct {
	sent     int
	codes    []int64
	chatJID  string
	warnings []string
	err      error
}
type documentsPickedMsg struct {
	paths []string
	err   error
}
type stickerPickedMsg struct {
	path string
	err  error
}
type audioPlayedMsg struct {
	msgID   string
	chatJID string
	message msgstore.Message
	seq     uint64
	player  *exec.Cmd
	path    string
	done    <-chan bool
	err     error
}
type audioFinishedMsg struct {
	msgID, chatJID string
	seq            uint64
	elapsed        int64
	completed      bool
}
type playedReceiptResultMsg struct {
	chatJID string
	msgID   string
	err     error
}
type mediaOpenedMsg struct {
	msgID        string
	path         string
	preview      string
	fallbackInfo string
	err          error
}
type mediaDownloadedMsg struct {
	path    string
	openErr error
	err     error
}
type videoSavedMsg struct {
	path string
	err  error
}
type documentFolderPickedMsg struct {
	folder string
	doc    msgstore.Message
	err    error
}
type transcriptionResultMsg struct {
	msg msgstore.Message
	err error
}
type botAudioTranscriptMsg struct {
	chatJID    string
	messageID  string
	transcript string
}
type dictationTickMsg struct{ seq uint64 }
type dictationResultMsg struct {
	seq     uint64
	chatJID string
	text    string
	err     error
}
type presenceMsg struct{ presence whatsapp.PresenceState }
type receiptMsg struct {
	chatJID string
	msgIDs  []string
	status  string
	readAt  time.Time
}
type errMsg struct{ err error }
type backgroundResultMsg struct{ err error }
type readResultMsg struct {
	chatJID string
	msgIDs  []string
	err     error
}
type presenceTickMsg time.Time

const possibleBlockDelay = 2 * time.Minute

func tickPresenceCmd() tea.Cmd {
	return tea.Every(5*time.Second, func(t time.Time) tea.Msg {
		return presenceTickMsg(t)
	})
}

func newModel(ctx context.Context, client *whatsapp.Client, cs *chatstore.Store, ms *msgstore.Store, guard *safety.Guard) model {
	ti := textinput.New()
	ti.Placeholder = "Type a message and press Enter..."
	ti.CharLimit = 4096
	ti.Prompt = "> "

	search := textinput.New()
	search.Placeholder = "palabra o frase"
	search.CharLimit = 4096
	search.Prompt = "Buscar: "
	rename := textinput.New()
	rename.Placeholder = "Nombre personalizado"
	rename.CharLimit = 80
	rename.Prompt = "Nombre: "

	vp := viewport.New(0, 0)

	return model{
		ctx:              ctx,
		client:           client,
		cs:               cs,
		ms:               ms,
		guard:            guard,
		lastInput:        time.Now(),
		input:            ti,
		searchInput:      search,
		renameInput:      rename,
		chatNumbers:      map[string]string{},
		searchMatchIndex: -1,
		viewport:         vp,
		followBottom:     true,
		focus:            focusChatList,
		statusLine:       "Connecting...",
		connected:        client == nil || client.WA.IsConnected(),
		connectionKnown:  true,
		senderNames:      map[string]string{},
		groupNames:       map[string]string{},
		presence:         map[string]whatsapp.PresenceState{},
		lastActivity:     map[string]time.Time{},
		mediaPreviews:    map[string]string{},
		stickerImages:    map[string]uint32{},
		stickerFailed:    map[string]bool{},
		stickerReveal:    map[string]bool{},
		mediaPaths:       map[string]string{},
		mediaHidden:      map[string]bool{},
		replyQuotes:      map[string]msgstore.Message{},
		transcriptSeen:   map[string]bool{},
	}
}

// Init satisfies tea.Model: it kicks off the initial local-cache loads
// (chats, contacts, groups) as async tea.Cmds so the first frame renders
// immediately rather than blocking on disk/network reads.
func (m model) Init() tea.Cmd {
	return tea.Batch(
		tea.ClearScreen,
		loadChatsCmd(m.cs),
		loadContactsCmd(m.ctx, m.client),
		loadGroupsCmd(m.ctx, m.client),
		tickPresenceCmd(),
		callTickCmd(),
	)
}

// loadContactsCmd reads local contacts (no network needed — see Phase 6)
// to build a JID -> display name lookup, used mainly in groups where
// messages come from several different senders.
func loadContactsCmd(ctx context.Context, client *whatsapp.Client) tea.Cmd {
	return func() tea.Msg {
		contacts, err := client.ListContacts(ctx)
		if err != nil {
			return errMsg{err}
		}
		names := make(map[string]string, len(contacts))
		for _, c := range contacts {
			if c.Name != "" {
				names[c.JID] = c.Name
			}
		}
		return contactsLoadedMsg{names}
	}
}

func loadChatNumbersCmd(ctx context.Context, client *whatsapp.Client, chats []chatstore.Chat) tea.Cmd {
	return func() tea.Msg {
		numbers := make(map[string]string)
		for _, chat := range chats {
			if chat.IsGroup {
				continue
			}
			jid, err := types.ParseJID(chat.JID)
			if err != nil {
				continue
			}
			if number := client.VerifiedPhoneNumber(ctx, jid); number != "" {
				numbers[chat.JID] = number
			}
		}
		return chatNumbersLoadedMsg{numbers: numbers}
	}
}

func renameChatCmd(cs *chatstore.Store, jid, name string) tea.Cmd {
	return func() tea.Msg {
		if cs == nil {
			return renameResultMsg{jid: jid, name: name, err: fmt.Errorf("chat store unavailable")}
		}
		return renameResultMsg{jid: jid, name: name, err: cs.SetLocalName(jid, name)}
	}
}

// loadGroupsCmd fetches real group names via the account's joined-groups
// list (internal/whatsapp.ListGroups, Phase 7). chatstore only ever gets
// a group's name from HistorySync, which has proven unreliable — this
// gives the TUI an authoritative fallback so groups aren't stuck showing
// a blank/JID-only name in the sidebar.
func loadGroupsCmd(ctx context.Context, client *whatsapp.Client) tea.Cmd {
	return func() tea.Msg {
		groups, err := client.ListGroups(ctx)
		if err != nil {
			return errMsg{err}
		}
		names := make(map[string]string, len(groups))
		for _, g := range groups {
			if g.Name != "" {
				names[g.JID] = g.Name
			}
		}
		return groupsLoadedMsg{names}
	}
}

func loadChatsCmd(cs *chatstore.Store) tea.Cmd {
	return func() tea.Msg {
		chats, err := cs.List()
		if err != nil {
			return errMsg{err}
		}
		return chatsLoadedMsg{chats}
	}
}

const messagesPerPage = 100

// searchResultsLimit bounds both memory use and what the terminal needs to
// render. The newest matching messages are returned first by the store.
const searchResultsLimit = 50

// loadMessagesCmd loads a page from the newest end of a chat. offset is the
// number of newest messages already in the UI; a non-zero offset prepends the
// preceding page instead of replacing the transcript.
func loadMessagesCmd(ctx context.Context, cs *chatstore.Store, ms *msgstore.Store, client *whatsapp.Client, chatJID string, offset int) tea.Cmd {
	return func() tea.Msg {
		pnJIDStr := ""
		if client != nil {
			if jid, err := types.ParseJID(chatJID); err == nil {
				pnJID := client.ResolvePNJID(ctx, jid)
				if !pnJID.IsEmpty() {
					pnJIDStr = pnJID.String()
				}
			}
		}
		msgs, hasOlder, err := ms.ListDualPage(chatJID, pnJIDStr, offset, messagesPerPage)
		if err != nil {
			return errMsg{err}
		}
		return messagesLoadedMsg{
			chatJID:  chatJID,
			msgs:     msgs,
			offset:   offset,
			prepend:  offset > 0,
			hasOlder: hasOlder,
		}
	}
}

func searchMessagesCmd(_ context.Context, ms *msgstore.Store, _ *whatsapp.Client, chatJID, query string) tea.Cmd {
	return func() tea.Msg {
		// Search must not touch WhatsApp's session store: resolving an
		// alternate JID can trigger background session writes. The selected
		// chat JID is the canonical key used by the TUI message store.
		msgs, err := ms.SearchReadOnly(chatJID, query, searchResultsLimit)
		return searchMessagesLoadedMsg{chatJID: chatJID, msgs: msgs, err: err}
	}
}

func sendCmd(ctx context.Context, client *whatsapp.Client, guard *safety.Guard, jidStr, text string) tea.Cmd {
	return func() tea.Msg {
		jid, err := types.ParseJID(jidStr)
		if err != nil {
			return sendResultMsg{err: err}
		}
		messageID, err := client.SendText(ctx, jid, text)
		if err != nil {
			return sendResultMsg{err: err}
		}
		if guard != nil {
			_ = guard.MarkKnown(jidStr)
		}
		return sendResultMsg{messageID: messageID}
	}
}

func sendReplyCmd(ctx context.Context, client *whatsapp.Client, guard *safety.Guard, jidStr, text string, quoted msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		jid, err := types.ParseJID(jidStr)
		if err != nil {
			return sendResultMsg{err: err}
		}
		messageID, err := client.SendReply(ctx, jid, text, quoted)
		if err != nil {
			return sendResultMsg{err: err}
		}
		if guard != nil {
			_ = guard.MarkKnown(jidStr)
		}
		return sendResultMsg{reply: true, messageID: messageID}
	}
}

func forwardMessageCmd(ctx context.Context, client *whatsapp.Client, guard *safety.Guard, destination string, message msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return forwardResultMsg{err: fmt.Errorf("whatsapp client not connected")}
		}
		jid, err := types.ParseJID(destination)
		if err != nil {
			return forwardResultMsg{err: err}
		}
		if _, err := client.ForwardMessage(ctx, jid, message); err != nil {
			return forwardResultMsg{err: err}
		}
		if guard != nil {
			_ = guard.MarkKnown(destination)
		}
		return forwardResultMsg{}
	}
}

func markReadCmd(ctx context.Context, cs *chatstore.Store, client *whatsapp.Client, msg msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		if msg.FromMe || msg.ID == "" {
			return readResultMsg{chatJID: msg.ChatJID}
		}
		if client == nil {
			if cs != nil {
				_ = cs.MarkRead(msg.ChatJID)
			}
			return readResultMsg{chatJID: msg.ChatJID, msgIDs: []string{msg.ID}}
		}
		chatJID, err := types.ParseJID(msg.ChatJID)
		if err != nil {
			return readResultMsg{chatJID: msg.ChatJID, msgIDs: []string{msg.ID}, err: err}
		}
		senderJID, err := types.ParseJID(msg.SenderJID)
		if err != nil {
			return readResultMsg{chatJID: msg.ChatJID, msgIDs: []string{msg.ID}, err: err}
		}
		err = client.MarkRead(ctx, chatJID, []string{msg.ID}, senderJID)
		if err == nil && cs != nil {
			_ = cs.MarkRead(msg.ChatJID)
		}
		return readResultMsg{chatJID: msg.ChatJID, msgIDs: []string{msg.ID}, err: err}
	}
}

func markVisibleMessagesReadCmd(ctx context.Context, cs *chatstore.Store, client *whatsapp.Client, chatJID string, messages []msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		idsBySender := make(map[string][]string)
		var allIDs []string
		for _, msg := range messages {
			if !msg.FromMe && msg.ID != "" && msg.Status != "read" {
				sender := msg.SenderJID
				if parsed, err := types.ParseJID(sender); err == nil {
					sender = parsed.ToNonAD().String()
				}
				idsBySender[sender] = append(idsBySender[sender], msg.ID)
				allIDs = append(allIDs, msg.ID)
			}
		}
		if len(allIDs) == 0 {
			return readResultMsg{chatJID: chatJID}
		}
		if client == nil {
			if cs != nil {
				_ = cs.MarkRead(chatJID)
			}
			return readResultMsg{chatJID: chatJID, msgIDs: allIDs}
		}
		parsedChatJID, err := types.ParseJID(chatJID)
		if err != nil {
			return readResultMsg{chatJID: chatJID, msgIDs: allIDs, err: err}
		}
		for sender, ids := range idsBySender {
			senderJID, parseErr := types.ParseJID(sender)
			if parseErr != nil {
				return readResultMsg{chatJID: chatJID, msgIDs: allIDs, err: parseErr}
			}
			if err = client.MarkRead(ctx, parsedChatJID, ids, senderJID); err != nil {
				return readResultMsg{chatJID: chatJID, msgIDs: allIDs, err: err}
			}
		}
		if cs != nil {
			_ = cs.MarkRead(chatJID)
		}
		return readResultMsg{chatJID: chatJID, msgIDs: allIDs}
	}
}

func chatActivityCmd(ctx context.Context, client *whatsapp.Client, chatJID string, composing bool) tea.Cmd {
	return func() tea.Msg {
		if client == nil || chatJID == "" {
			return backgroundResultMsg{}
		}
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			return backgroundResultMsg{err: err}
		}
		if err = client.SubscribePresence(ctx, jid); err == nil {
			err = client.SetChatPresence(ctx, jid, composing)
		}
		return backgroundResultMsg{err: err}
	}
}

func chatInactiveCmd(ctx context.Context, client *whatsapp.Client, chatJID string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return backgroundResultMsg{}
		}
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			return backgroundResultMsg{err: err}
		}
		return backgroundResultMsg{err: client.DeactivateChat(ctx, jid)}
	}
}

// activeChat reports whether the current chat should publish presence and read receipts.
func (m model) activeChat() bool {
	return m.focus == focusInput && (!m.terminalBlurred || m.keepPresenceOnBlur)
}

// pendingPlayedReceipts schedules receipts only while the composer is active.
// The local Played marker survives incognito playback and app restarts.
func (m *model) pendingPlayedReceipts() []tea.Cmd {
	if !m.activeChat() || m.client == nil {
		return nil
	}
	if m.playedReceiptPending == nil {
		m.playedReceiptPending = make(map[string]bool)
	}
	var cmds []tea.Cmd
	for _, msg := range m.messages {
		if msg.ChatJID != m.selectedJID || msg.FromMe || msg.MediaType != "voice note" || !msg.Played || msg.PlayedReceiptSent || msg.ID == "" {
			continue
		}
		key := msg.ChatJID + "\x00" + msg.ID
		if m.playedReceiptPending[key] {
			continue
		}
		m.playedReceiptPending[key] = true
		cmds = append(cmds, markPlayedReceiptCmd(m.ctx, m.client, m.ms, msg))
	}
	return cmds
}

func (m *model) activateChatCmd() tea.Cmd {
	cmds := []tea.Cmd{
		markVisibleMessagesReadCmd(m.ctx, m.cs, m.client, m.selectedJID, m.messages),
		chatActivityCmd(m.ctx, m.client, m.selectedJID, strings.TrimSpace(m.input.Value()) != ""),
	}
	cmds = append(cmds, m.pendingPlayedReceipts()...)
	return tea.Batch(cmds...)
}

// openMediaCmd downloads one image and hands it to the desktop viewer. Inline
// image rendering used chafa for every photo in a chat, which made ordinary
// transcript redraws unnecessarily expensive.
func openMediaCmd(ctx context.Context, client *whatsapp.Client, msg msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		data, mime, err := client.DownloadMedia(ctx, msg)
		if err != nil {
			return mediaOpenedMsg{msgID: msg.ID, err: err}
		}
		ext := extensionFromMime(mime, msg.MediaType)
		dir := os.TempDir()
		tmpPath := filepath.Join(dir, fmt.Sprintf("wa-cli-media-%s%s", msg.ID, ext))
		if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
			return mediaOpenedMsg{msgID: msg.ID, err: err}
		}
		cmd := openFile(tmpPath)
		if err := cmd.Start(); err != nil {
			return mediaOpenedMsg{msgID: msg.ID, err: err}
		}
		return mediaOpenedMsg{msgID: msg.ID, path: tmpPath}
	}
}

func openFile(path string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", path)
	}
	// Prefer a small Wayland-friendly image viewer over launching a full
	// browser for a single photo. Keep xdg-open as a fallback for systems
	// where imv is not installed.
	if runtime.GOOS == "linux" {
		if imv, err := exec.LookPath("imv"); err == nil {
			return exec.Command(imv, path)
		}
	}
	return exec.Command("xdg-open", path)
}

func shouldPreviewInline(mime, mediaType, ext string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	ext = strings.ToLower(strings.TrimSpace(ext))

	if strings.HasPrefix(mime, "image/") {
		return true
	}
	if strings.Contains(mediaType, "image") || strings.Contains(mediaType, "imagen") || strings.Contains(mediaType, "foto") || strings.Contains(mediaType, "sticker") {
		return true
	}
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	default:
		return false
	}
}

var oscRegex = regexp.MustCompile(`(?:\x1b\]|\])\d+;rgb:[^\x1b\\]*(?:\x1b\\|\\|\s*)`)
var apcGraphicsRegex = regexp.MustCompile(`\x1b(?:_|P)[^\x1b]*(?:\x1b\\)`)
var csiRegex = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// leakedMouseSeqRegex matches an SGR mouse report (`ESC[<btn;x;yM` or `...m`)
// that reached the composer as literal text instead of being consumed as a
// tea.MouseMsg. This can happen when the terminal delivers a burst of wheel
// events (rapid scrolling) in reads too small/fragmented for bubbletea's
// parser to reassemble in time; the leading ESC is unprintable, so only the
// "[<btn;x;yM" tail shows up as garbage in whatever field has focus.
var leakedMouseSeqRegex = regexp.MustCompile(`\[?<?\d+;\d+;\d+[Mm]`)

// stripLeakedMouseSequences removes any leaked mouse report text from an
// input field right after it's updated, so mouse tracking can stay on even
// while the composer has focus — see leakedMouseSeqRegex.
func stripLeakedMouseSequences(input *textinput.Model) {
	value := input.Value()
	cleaned := leakedMouseSeqRegex.ReplaceAllString(value, "")
	if cleaned == value {
		return
	}
	input.SetValue(cleaned)
	input.CursorEnd()
}

func cleanChafaOutput(out string) string {
	cleaned := oscRegex.ReplaceAllString(out, "")
	// Never let Kitty/Sixel/APC graphics or cursor-control sequences enter
	// Bubble Tea's text content. SGR color sequences ending in `m` are safe
	// and are preserved; other CSI controls are removed.
	cleaned = apcGraphicsRegex.ReplaceAllString(cleaned, "")
	cleaned = csiRegex.ReplaceAllStringFunc(cleaned, func(sequence string) string {
		if strings.HasSuffix(sequence, "m") {
			return sequence
		}
		return ""
	})
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return ""
	}
	var b strings.Builder
	lines := strings.Split(cleaned, "\n")
	for i, line := range lines {
		trimmedLine := strings.TrimRight(line, " \t\r")
		if trimmedLine != "" {
			b.WriteString(trimmedLine + "\x1b[0m")
		}
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func renderImageInTerminal(path string, viewportWidth int) (string, error) {
	if _, err := exec.LookPath("chafa"); err != nil {
		return "", fmt.Errorf("no se encontró 'chafa' para previsualizar imágenes en terminal")
	}

	// Kitty/iTerm escape sequences render real pixels, but embedding those
	// cursor-control sequences in Bubble Tea's viewport corrupts the TUI
	// layout. Use high-density true-color symbols here instead: they remain
	// sharp enough at a compact size and are safe inside the viewport.
	maxW := 18
	if viewportWidth-4 < maxW {
		maxW = viewportWidth - 4
	}
	if maxW < 12 {
		maxW = 12
	}
	maxH := maxW / 2
	if maxH < 6 {
		maxH = 6
	}
	if maxH > 10 {
		maxH = 10
	}

	cmd := exec.Command("chafa",
		"--format", "symbols",
		"--symbols", "block+vhalf+hhalf+quad+sextant",
		"--colors", "full",
		"--dither", "none",
		"--work", "9",
		"--preprocess", "off",
		"--scale", "max",
		"--animate", "off",
		"--passthrough", "none",
		"--size", fmt.Sprintf("%dx%d", maxW, maxH),
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("falló previsualización en terminal: %w", err)
	}
	preview := cleanChafaOutput(string(out))
	if preview == "" {
		return "", fmt.Errorf("chafa no devolvió salida válida para la imagen")
	}
	return preview, nil
}

func extensionFromMime(mime, mediaType string) string {
	switch {
	case strings.Contains(mime, "image/png"):
		return ".png"
	case strings.Contains(mime, "image/webp"):
		return ".webp"
	case strings.Contains(mime, "image/jpeg") || strings.Contains(mime, "image/jpg"):
		return ".jpg"
	case strings.Contains(mime, "video/mp4"):
		return ".mp4"
	case strings.Contains(mime, "audio/ogg") || strings.Contains(mime, "audio/opus"):
		return ".ogg"
	case strings.Contains(mime, "application/pdf"):
		return ".pdf"
	}
	switch mediaType {
	case "image", "sticker":
		return ".jpg"
	case "video":
		return ".mp4"
	case "audio":
		return ".ogg"
	default:
		return ".bin"
	}
}

// Update satisfies tea.Model: it's the single dispatch point for every
// event the TUI reacts to (key presses, window resizes, and the async
// results of the tea.Cmds started from Init or from a previous Update).
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	prevOffset := m.viewport.YOffset
	updated, cmd := m.update(msg)
	var next model
	switch v := updated.(type) {
	case model:
		next = v
	case *model:
		// Some handlers have pointer receivers (e.g. clipboard image send) and
		// return their receiver as the tea.Model.
		next = *v
	default:
		return updated, cmd
	}
	next.syncFollowBottom(prevOffset)
	if err := syncBotFocus(next.botFocusPath, next.selectedJID, next.focus == focusInput && !next.terminalBlurred); err != nil {
		next.statusLine = "No se pudo actualizar bot_focus: " + err.Error()
	}
	return next, cmd
}

// syncFollowBottom keeps the message history glued to the newest message.
// Many handlers re-render (presence, receipts, relative times, layout
// changes) without restoring the bottom, which used to leave new messages
// hidden below the fold until the user scrolled. If the handler itself moved
// the offset, that was a deliberate scroll: remember whether it ended at the
// bottom. Otherwise, a reader who was following snaps back to the bottom.
func (m *model) syncFollowBottom(prevOffset int) {
	if m.viewport.YOffset != prevOffset {
		m.followBottom = m.viewport.AtBottom()
		return
	}
	if m.followBottom && !m.viewport.AtBottom() {
		m.viewport.GotoBottom()
	}
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case stickerLoadedMsg:
		if msg.key == m.stickerLoading {
			m.stickerLoading = ""
		}
		if msg.err == nil && msg.id != 0 {
			m.stickerImages[msg.key] = msg.id
			wasAtBottom := m.viewport.AtBottom()
			y := m.viewport.YOffset
			m.viewport.SetContent(m.renderMessages())
			if wasAtBottom {
				m.viewport.GotoBottom()
			} else {
				m.viewport.SetYOffset(y)
			}
		} else {
			m.stickerFailed[msg.key] = true
			if msg.err != nil {
				m.statusLine = "No se pudo mostrar un sticker: " + msg.err.Error()
			}
		}
		return m, m.nextPendingStickerCmd()
	case clearJumpHighlightMsg:
		if msg.seq == m.jumpHighlightSeq && m.highlightedMessageID != "" {
			m.highlightedMessageID = ""
			y := m.viewport.YOffset
			m.viewport.SetContent(m.renderMessages())
			m.viewport.SetYOffset(y)
		}
		return m, nil
	case connectionMsg:
		m.setConnectionState(msg.connected, msg.reason)
		return m, nil
	case callTickMsg:
		if m.calls == nil {
			return m, nil
		}
		previous := m.callState
		m.callState = m.calls.Snapshot()
		if m.callNotify && m.callState.Busy && m.callState.Incoming && m.callState.Sequence != previous.Sequence {
			body := "F4: controles · F5: contestar · F6: rechazar"
			title := "WhatsApp · Llamada entrante"
			if m.callState.Video {
				title = "WhatsApp · Videollamada entrante"
				body = "F4: controles · F5: contestar con cámara · F6: rechazar"
			}
			if m.callNotifyPreview {
				body = m.callName(m.callState.Peer) + "\n" + body
			}
			notify.SendAsync(title, body, nil)
		}
		if previous.Busy && !m.callState.Busy {
			m.statusLine = m.callState.Description()
		}
		return m, callTickCmd()
	case callActionMsg:
		m.callActionBusy = false
		if m.calls != nil {
			m.callState = m.calls.Snapshot()
		}
		if msg.err != nil {
			m.statusLine = "Llamada: " + msg.err.Error()
		}
		return m, nil

	case tea.WindowSizeMsg:
		if m.ready && m.width == msg.Width && m.height == msg.Height {
			return m, nil
		}
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.layout()
		return m, tea.ClearScreen

	case tea.BlurMsg:
		m.metrics.Record(metrics.Blur, "", "", 0, 0)
		m.terminalBlurred = true
		if m.keepPresenceOnBlur && m.focus == focusInput {
			// Explicit Ctrl+Space override: keep the active chat pink so the
			// normal presence and read-receipt loop continues while unfocused.
			return m, nil
		}
		// Losing terminal/tab focus ends active participation in the chat. Clear
		// the pink composer focus too, so a later FocusMsg cannot resume presence
		// or read receipts until the user explicitly focuses the composer again.
		m.focus = focusChatList
		m.input.Blur()
		if m.client == nil || m.selectedJID == "" {
			return m, nil
		}
		return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)

	case tea.FocusMsg:
		m.metrics.Record(metrics.Focus, "", "", 0, 0)
		m.terminalBlurred = false
		if m.focus == focusInput {
			return m, m.activateChatCmd()
		}
		return m, nil

	case chatsLoadedMsg:
		wasEmpty := len(m.chats) == 0
		m.chats = msg.chats
		if m.statusLine == "Connecting..." {
			m.statusLine = ""
		}
		if m.selectedJID == "" && len(m.chats) > 0 {
			m.selectedJID = m.chats[0].JID
			m.recordChatOpen()
		}
		if (wasEmpty || len(m.messages) == 0) && m.selectedJID != "" {
			return m, tea.Batch(loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0), loadChatNumbersCmd(m.ctx, m.client, m.chats))
		}
		return m, loadChatNumbersCmd(m.ctx, m.client, m.chats)

	case chatNumbersLoadedMsg:
		m.chatNumbers = msg.numbers
		return m, nil

	case renameResultMsg:
		m.renameSaving = false
		if msg.err != nil {
			m.statusLine = "No se pudo guardar el nombre: " + msg.err.Error()
			return m, nil
		}
		for i := range m.chats {
			if m.chats[i].JID == msg.jid {
				m.chats[i].LocalName = msg.name
				if msg.name == "" {
					if m.chats[i].Name != "" {
						m.senderNames[msg.jid] = m.chats[i].Name
					} else {
						delete(m.senderNames, msg.jid)
					}
				}
				break
			}
		}
		if msg.name != "" {
			m.senderNames[msg.jid] = msg.name
		}
		m.renameJID = ""
		m.renameInput.Blur()
		m.statusLine = "Nombre guardado"
		m.viewport.SetContent(m.renderMessages())
		return m, nil

	case contactsLoadedMsg:
		if msg.names == nil {
			msg.names = make(map[string]string)
		}
		for _, chat := range m.chats {
			if chat.LocalName != "" {
				msg.names[chat.JID] = chat.LocalName
			}
		}
		m.senderNames = msg.names
		if len(m.messages) > 0 {
			m.viewport.SetContent(m.renderMessages())
		}
		return m, nil

	case groupsLoadedMsg:
		m.groupNames = msg.names
		return m, nil

	case presenceMsg:
		if m.presence == nil {
			m.presence = map[string]whatsapp.PresenceState{}
		}
		previous := m.presence[msg.presence.JID]
		if msg.presence.TypingKnown {
			// ChatPresence only describes composing/paused; retain the online
			// state reported by the separate presence subscription.
			msg.presence.IsOnline = previous.IsOnline
			msg.presence.LastSeen = previous.LastSeen
		} else {
			// A normal online/offline update does not end a composing indicator.
			msg.presence.IsTyping = previous.IsTyping
			msg.presence.TypingKnown = previous.TypingKnown
			msg.presence.TypingUntil = previous.TypingUntil
		}
		// Periodic subscriptions may return last=deny after an earlier event
		// supplied a real timestamp. Never erase a verified last-seen.
		if msg.presence.LastSeen.IsZero() && !previous.LastSeen.IsZero() {
			msg.presence.LastSeen = previous.LastSeen
		}
		m.presence[msg.presence.JID] = msg.presence
		if len(m.messages) > 0 {
			m.viewport.SetContent(m.renderMessages())
		}
		return m, nil

	case messagesLoadedMsg:
		if msg.chatJID != m.selectedJID {
			return m, nil // stale response for a chat we've since navigated away from
		}
		m.noteTranscripts(msg.msgs)
		hadMessages := len(m.messages) > 0
		wasAtBottom := m.viewport.AtBottom()
		m.loadingOlder = false
		if msg.prepend {
			// Preserve the reader's position after adding content above it.
			prefix := m
			prefix.messages = msg.msgs
			prefix.replyToID = ""
			prefix.selectedAudioID = ""
			prefix.searchMatchIndex = -1
			prefix.selectedDeleteID = ""
			prefixLines := strings.Count(prefix.renderMessages(), "\n")
			m.messages = append(msg.msgs, m.messages...)
			m.messageOffset += len(msg.msgs)
			m.hasOlder = msg.hasOlder
			m.viewport.SetContent(m.renderMessages())
			y := prefixLines - m.viewport.Height
			if y < 0 {
				y = 0
			}
			m.viewport.SetYOffset(y)
			m.statusLine = fmt.Sprintf("Cargados %d mensajes anteriores", len(msg.msgs))
			return m, nil
		}
		m.messages = msg.msgs
		m.messageOffset = len(msg.msgs)
		m.hasOlder = msg.hasOlder
		if _, ok := m.selectedReplyMessage(); !ok {
			m.replyToID = ""
		}
		if activity := latestIncomingActivity(msg.msgs); !activity.IsZero() {
			m.lastActivity[msg.chatJID] = activity
		}
		m.viewport.SetContent(m.renderMessages())
		// A reload after an incoming message, receipt, or send result must not
		// pull a reader away from older context. New chats still open at the
		// newest message, and readers already at the bottom continue following.
		if !hadMessages || wasAtBottom {
			m.viewport.GotoBottom()
		}
		if m.activeChat() {
			return m, m.activateChatCmd()
		}
		return m, nil

	case searchMessagesLoadedMsg:
		if msg.chatJID != m.selectedJID {
			return m, nil
		}
		if msg.err != nil {
			m.statusLine = "No se pudo buscar mensajes: " + msg.err.Error()
			return m, nil
		}
		m.messages = msg.msgs
		m.messageOffset = len(msg.msgs)
		m.hasOlder = false
		m.loadingOlder = false
		m.searchMatchIndex = -1
		m.viewport.SetContent(m.renderMessages())
		if m.moveSearchMatch(-1) {
			return m, nil
		}
		m.statusLine = "No se encontraron mensajes con \"" + m.searchTerm + "\""
		return m, nil

	case incomingMsg:
		var cmds []tea.Cmd
		if !msg.msg.FromMe && msg.msg.Timestamp > 0 {
			if m.lastActivity == nil {
				m.lastActivity = map[string]time.Time{}
			}
			activity := time.UnixMilli(msg.msg.Timestamp)
			if activity.After(m.lastActivity[msg.msg.ChatJID]) {
				m.lastActivity[msg.msg.ChatJID] = activity
			}
		}
		if m.sameContact(msg.msg.ChatJID, m.selectedJID) {
			wasAtBottom := m.viewport.AtBottom()
			m.messages = append(m.messages, msg.msg)
			m.viewport.SetContent(m.renderMessages())
			if wasAtBottom {
				m.viewport.GotoBottom()
			}
			if m.activeChat() {
				cmds = append(cmds, markReadCmd(m.ctx, m.cs, m.client, msg.msg))
			}
		}
		cmds = append(cmds, loadChatsCmd(m.cs))
		return m, tea.Batch(cmds...)

	case botSentMsg:
		m.statusLine = "bot: mensaje enviado"
		if m.sameContact(msg.chatJID, m.selectedJID) {
			return m, tea.Batch(
				loadChatsCmd(m.cs),
				loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0),
			)
		}
		return m, tea.Batch(loadChatsCmd(m.cs))

	case sendResultMsg:
		if msg.err == nil {
			m.metrics.Record(metrics.ComposeKeys, msg.chatJID, "", msg.keyCount, msg.backspaces)
			m.metrics.Record(metrics.Send, msg.chatJID, msg.messageID, msg.composeMS, msg.charCount)
			if msg.quotedID != "" {
				m.metrics.Record(metrics.ReplyQuote, msg.chatJID, msg.quotedID, 0, 0)
			}
		}
		if msg.err != nil {
			m.statusLine = "send failed: " + msg.err.Error()
		} else {
			// Attach the WhatsApp ID to the optimistic on-screen message as
			// soon as sending succeeds. That makes a recent message eligible
			// for reply or Ctrl+D without reopening the chat.
			if msg.messageID != "" {
				for i := len(m.messages) - 1; i >= 0; i-- {
					if m.messages[i].FromMe && m.messages[i].ID == "" {
						m.messages[i].ID = msg.messageID
						break
					}
				}
			}
			if msg.reply {
				m.statusLine = "Respuesta enviada"
			} else {
				m.statusLine = "Mensaje enviado"
			}
			// Reload the active transcript from msgstore as well as the sidebar.
			// recordOutgoingMessage has already persisted the definitive server
			// ID; replacing the optimistic copy avoids a message that renders
			// correctly but cannot be selected for Ctrl+D.
			return m, tea.Batch(
				loadChatsCmd(m.cs),
				loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0),
			)
		}
		return m, nil

	case deleteResultMsg:
		m.deleteSelectMode = false
		m.selectedDeleteID = ""
		m.deleteConfirm = false
		if msg.err != nil {
			m.statusLine = "No se pudo eliminar para todos: " + msg.err.Error()
			return m, nil
		}
		for i := range m.messages {
			if m.messages[i].ID == msg.message.ID {
				m.messages[i] = msg.message
				break
			}
		}
		m.viewport.SetContent(m.renderMessages())
		m.statusLine = "Mensaje eliminado para todos"
		return m, nil

	case editResultMsg:
		if msg.err != nil {
			m.statusLine = "No se pudo editar: " + msg.err.Error()
			return m, nil
		}
		for i := range m.messages {
			if m.messages[i].ID == msg.message.ID {
				m.messages[i] = msg.message
				break
			}
		}
		m.viewport.SetContent(m.renderMessages())
		m.statusLine = "Mensaje editado"
		return m, nil

	case forwardResultMsg:
		m.selectedForwardID = ""
		m.forwardDestinationJID = ""
		if msg.err != nil {
			m.statusLine = "No se pudo reenviar: " + msg.err.Error()
			return m, nil
		}
		m.statusLine = "Mensaje reenviado"
		return m, tea.Batch(
			loadChatsCmd(m.cs),
			loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0),
		)

	case sendImageMsg:
		if msg.err == nil {
			m.metrics.Record(metrics.MediaSend, mediaChat(msg.chatJID, m.selectedJID), "", 1, 0)
		}
		if msg.err != nil {
			m.statusLine = "send image failed: " + msg.err.Error()
		} else {
			m.statusLine = "Image sent!"
			return m, loadChatsCmd(m.cs)
		}
		return m, nil

	case sendStickerMsg:
		if msg.err == nil {
			m.metrics.Record(metrics.MediaSend, mediaChat(msg.chatJID, m.selectedJID), "", 4, 0)
		}
		if msg.err != nil {
			m.statusLine = "No se pudo enviar el sticker: " + msg.err.Error()
			return m, nil
		}
		m.statusLine = "Sticker enviado"
		return m, tea.Batch(
			loadChatsCmd(m.cs),
			loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0),
		)

	case sendAudioMsg:
		if msg.err == nil {
			m.metrics.Record(metrics.MediaSend, mediaChat(msg.chatJID, m.selectedJID), "", 3, 0)
		}
		if msg.err != nil {
			m.statusLine = "send voice note failed: " + msg.err.Error()
		} else {
			m.statusLine = "Voice note sent!"
			return m, loadChatsCmd(m.cs)
		}
		return m, nil

	case sendDocumentsResultMsg:
		for i := 0; i < msg.sent; i++ {
			m.metrics.Record(metrics.MediaSend, mediaChat(msg.chatJID, m.selectedJID), "", 5, 0)
		}
		if msg.err != nil {
			m.statusLine = fmt.Sprintf("Se enviaron %d documento(s); falló: %v", msg.sent, msg.err)
			return m, nil
		}
		m.statusLine = fmt.Sprintf("%d documento(s) enviado(s)", msg.sent)
		return m, tea.Batch(
			loadChatsCmd(m.cs),
			loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0),
		)

	case sendDroppedFilesResultMsg:
		for _, code := range msg.codes {
			m.metrics.Record(metrics.MediaSend, msg.chatJID, "", code, 0)
		}
		if msg.err != nil {
			m.statusLine = fmt.Sprintf("Se enviaron %d archivo(s); falló: %v", msg.sent, msg.err)
			return m, nil
		}
		m.statusLine = fmt.Sprintf("%d archivo(s) enviado(s)", msg.sent)
		if len(msg.warnings) > 0 {
			m.statusLine += " · " + strings.Join(msg.warnings, "; ")
		}
		return m, tea.Batch(
			loadChatsCmd(m.cs),
			loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0),
		)

	case documentsPickedMsg:
		if msg.err != nil {
			// Keep attachments usable even when the desktop picker is missing
			// or broken.  The terminal picker needs no graphical dependencies.
			next, cmd := m.openDocumentPicker()
			if nextModel, ok := next.(model); ok {
				nextModel.statusLine = "Selector gráfico no disponible; usando selector interno"
				return nextModel, cmd
			}
			return next, cmd
		}
		if len(msg.paths) == 0 {
			m.statusLine = "No seleccionaste documentos"
			return m, nil
		}
		m.statusLine = fmt.Sprintf("Enviando %d documento(s)...", len(msg.paths))
		return m, sendDocumentsCmd(m.ctx, m.client, m.guard, m.selectedJID, msg.paths)

	case stickerPickedMsg:
		if msg.err != nil {
			m.statusLine = "No se pudo cargar el sticker: " + msg.err.Error()
			return m, nil
		}
		if msg.path == "" {
			m.statusLine = "No seleccionaste un sticker"
			return m, nil
		}
		m.statusLine = "Enviando sticker..."
		return m, sendStickerCmd(m.ctx, m.client, m.guard, m.selectedJID, msg.path)

	case audioPlayedMsg:
		if msg.seq != m.audioPlaySeq {
			stopAudioPlayer(msg.player)
			if msg.path != "" {
				_ = os.Remove(msg.path)
			}
			return m, nil
		}
		if msg.err != nil {
			m.statusLine = "No se pudo reproducir el audio: " + msg.err.Error()
			m.playingAudioID = ""
			m.audioPlayer = nil
			m.audioPaused = false
		} else {
			m.statusLine = "Reproduciendo nota de voz"
			m.playingAudioID = msg.msgID
			m.audioChatJID = msg.chatJID
			m.audioStarted = time.Now()
			m.audioPausedAt = time.Time{}
			m.audioPausedTotal = 0
			m.metrics.Record(metrics.AudioPlayStart, msg.chatJID, msg.msgID, int64(audioDurationSeconds(msg.message)), msg.message.Timestamp)
			m.audioPlayer = msg.player
			m.audioPaused = false
			for i := range m.messages {
				if m.messages[i].ID == msg.msgID {
					m.messages[i].Played = true
					break
				}
			}
			m.viewport.SetContent(m.renderMessages())
			localPlayed := markAudioPlayedCmd(m.ms, msg.chatJID, msg.msgID)
			if !msg.message.FromMe && msg.message.MediaType == "voice note" {
				if msg.done == nil {
					return m, tea.Batch(append([]tea.Cmd{localPlayed}, m.pendingPlayedReceipts()...)...)
				}
				return m, tea.Batch(append([]tea.Cmd{localPlayed, waitAudioCmd(msg)}, m.pendingPlayedReceipts()...)...)
			}
			if msg.done == nil {
				return m, localPlayed
			}
			return m, tea.Batch(localPlayed, waitAudioCmd(msg))
		}
		return m, nil

	case audioFinishedMsg:
		if msg.seq == m.audioPlaySeq && msg.msgID == m.playingAudioID {
			m.endAudio(msg.completed)
			m.playingAudioID = ""
			m.audioPlayer = nil
			m.audioPaused = false
		}
		return m, nil
	case playedReceiptResultMsg:
		delete(m.playedReceiptPending, msg.chatJID+"\x00"+msg.msgID)
		if msg.err != nil {
			m.statusLine = "Audio reproducido, pero no se confirmó a la otra persona: " + msg.err.Error()
		} else {
			for i := range m.messages {
				if m.messages[i].ChatJID == msg.chatJID && m.messages[i].ID == msg.msgID {
					m.messages[i].PlayedReceiptSent = true
				}
			}
		}
		return m, nil

	case mediaDownloadedMsg:
		if msg.err != nil {
			m.statusLine = "No se pudo descargar el documento: " + msg.err.Error()
		} else if msg.openErr != nil {
			m.statusLine = "Documento descargado en " + msg.path + "; no se pudo abrir: " + msg.openErr.Error()
		} else {
			m.statusLine = "Documento descargado y abierto: " + msg.path
		}
		return m, nil
	case videoSavedMsg:
		if msg.err != nil {
			m.statusLine = "No se pudo guardar el video: " + msg.err.Error()
		} else {
			m.statusLine = "Video guardado en " + msg.path
		}
		return m, nil

	case documentFolderPickedMsg:
		if msg.err != nil {
			m.statusLine = "No se pudo abrir el selector de carpeta: " + msg.err.Error()
			return m, nil
		}
		if msg.folder == "" {
			m.statusLine = "Descarga cancelada"
			return m, nil
		}
		m.metrics.Record(metrics.DocumentOpen, msg.doc.ChatJID, msg.doc.ID, msg.doc.Timestamp, boolInt(msg.doc.FromMe))
		m.statusLine = "Descargando documento a " + msg.folder + "..."
		return m, downloadDocumentToCmd(m.ctx, m.client, msg.doc, msg.folder)

	case receiptMsg:
		if m.sameContact(msg.chatJID, m.selectedJID) {
			idSet := make(map[string]bool, len(msg.msgIDs))
			for _, id := range msg.msgIDs {
				idSet[id] = true
			}
			updated := false
			for i := range m.messages {
				if idSet[m.messages[i].ID] {
					m.messages[i].Status = msg.status
					if !msg.readAt.IsZero() {
						m.messages[i].ReadAt = msg.readAt.UnixMilli()
					}
					updated = true
				}
			}
			if updated {
				m.viewport.SetContent(m.renderMessages())
			}
		}
		return m, nil

	case presenceTickMsg:
		if !m.terminalBlurred && !m.idle && !m.lastInput.IsZero() && time.Since(m.lastInput) >= 120*time.Second {
			m.idle = true
			m.metrics.RecordAt(m.lastInput, metrics.IdleStart, "", "", 0, 0)
		}
		var cmds []tea.Cmd
		if m.client != nil && m.client.WA != nil {
			isConnected := m.client.WA.IsConnected()
			if isConnected != m.connected {
				m.setConnectionState(isConnected, "")
			}
		}
		if m.selectedJID != "" && m.client != nil && m.activeChat() {
			composing := m.focus == focusInput && strings.TrimSpace(m.input.Value()) != ""
			cmds = append(cmds, chatActivityCmd(m.ctx, m.client, m.selectedJID, composing))
			cmds = append(cmds, markVisibleMessagesReadCmd(m.ctx, m.cs, m.client, m.selectedJID, m.messages))
			cmds = append(cmds, m.pendingPlayedReceipts()...)
		}
		// Re-render so a message that remains at one check can surface the
		// possible-block warning without requiring another user action.
		if len(m.messages) > 0 {
			wasAtBottom := m.viewport.AtBottom()
			m.viewport.SetContent(m.renderMessages())
			if wasAtBottom {
				m.viewport.GotoBottom()
			}
		}
		cmds = append(cmds, tickPresenceCmd(), tea.WindowSize())
		return m, tea.Batch(cmds...)

	case backgroundResultMsg:
		if msg.err != nil {
			m.statusLine = "background sync failed: " + msg.err.Error()
		}
		return m, nil

	case readResultMsg:
		if msg.err != nil {
			m.statusLine = "read receipt pending: " + msg.err.Error()
			return m, nil
		}
		if len(msg.msgIDs) == 0 {
			return m, nil
		}
		idSet := make(map[string]bool, len(msg.msgIDs))
		for _, id := range msg.msgIDs {
			idSet[id] = true
		}
		for i := range m.messages {
			if !m.messages[i].FromMe && idSet[m.messages[i].ID] {
				m.messages[i].Status = "read"
			}
		}
		return m, loadChatsCmd(m.cs)

	case mediaOpenedMsg:
		if msg.msgID != "" {
			if msg.path != "" {
				m.mediaPaths[msg.msgID] = msg.path
			}
			if msg.preview != "" {
				m.mediaPreviews[msg.msgID] = msg.preview
				if m.mediaHidden == nil {
					m.mediaHidden = map[string]bool{}
				}
				m.mediaHidden[msg.msgID] = false
				wasAtBottom := m.viewport.AtBottom()
				m.viewport.SetContent(m.renderMessages())
				if wasAtBottom {
					m.viewport.GotoBottom()
				}
			}
		}
		if msg.err != nil {
			m.statusLine = "failed to open media: " + msg.err.Error()
		} else if msg.preview != "" {
			m.statusLine = "Vista previa mostrada (presiona 'o' para ocultar)"
		} else if msg.fallbackInfo != "" {
			m.statusLine = "Abierto en visor externo (" + msg.fallbackInfo + ")"
		} else {
			m.statusLine = "opened media: " + msg.path
		}
		return m, nil

	case transcriptionResultMsg:
		if msg.err == nil {
			m.noteTranscripts([]msgstore.Message{msg.msg})
		}
		// Result node: close selector state, attach text to the original
		// message, persist it, and redraw below [voice note].
		m.transcribingID = ""
		m.audioSelectMode = false
		m.selectedAudioID = ""
		m.audioPlayMode = false
		if msg.err != nil {
			m.statusLine = "Transcripción falló: " + msg.err.Error()
			return m, nil
		}
		for i := range m.messages {
			if m.messages[i].ID == msg.msg.ID {
				m.messages[i].Transcript = msg.msg.Transcript
				break
			}
		}
		m.viewport.SetContent(m.renderMessages())
		m.statusLine = "Audio transcrito y guardado"
		return m, saveTranscriptCmd(m.ms, msg.msg)

	case botAudioTranscriptMsg:
		if msg.chatJID != normalizedChatJID(m.selectedJID) {
			return m, nil
		}
		for i := range m.messages {
			if m.messages[i].ID == msg.messageID {
				atBottom := m.viewport.AtBottom()
				m.messages[i].Transcript = msg.transcript
				m.viewport.SetContent(m.renderMessages())
				if atBottom {
					m.viewport.GotoBottom()
				}
				return m, nil
			}
		}
		return m, loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0)

	case dictationTickMsg:
		if !m.isDictating || msg.seq != m.dictationSeq {
			return m, nil
		}
		m.refreshDictationPreview()
		if time.Since(m.lastSpaceTime) >= 350*time.Millisecond || time.Since(m.recordingStartTime) >= 2*time.Minute {
			return m.stopDictation()
		}
		return m, dictationTickCmd(m.dictationSeq)

	case dictationResultMsg:
		if msg.seq != m.dictationSeq {
			return m, nil
		}
		if m.dictationCancel != nil {
			m.dictationCancel()
			m.dictationCancel = nil
		}
		m.dictationProcessing = false
		if msg.err != nil {
			if m.dictationPreview != "" && msg.chatJID == m.selectedJID {
				preview := m.dictationPreview
				m.restoreDictationDraft()
				m.insertDictation(preview)
				m.statusLine = "Falló la corrección final; revisa el texto provisional antes de enviar"
				return m, nil
			}
			m.restoreDictationDraft()
			m.statusLine = "Dictado falló: " + msg.err.Error()
			return m, nil
		}
		if msg.chatJID != m.selectedJID {
			m.restoreDictationDraft()
			m.dictationPendingText = msg.text
			m.statusLine = "Dictado listo: vuelve al chat donde grabaste para insertarlo"
			return m, nil
		}
		m.restoreDictationDraft()
		m.insertDictation(msg.text)
		m.recordDictation(msg.text)
		m.statusLine = "Dictado insertado; revísalo y pulsa Enter para enviar"
		return m, chatActivityCmd(m.ctx, m.client, m.selectedJID, true)

	case errMsg:
		m.statusLine = msg.err.Error()
		return m, nil

	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress || msg.Action == tea.MouseActionRelease {
			m.noteInput()
		}
		if m.isDictating || m.dictationProcessing {
			return m, nil
		}
		if m.renameJID != "" {
			return m, nil
		}
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			for _, chat := range m.chats {
				inSidebar := !m.sidebarHidden && zone.Get(chatNameZoneID(chat.JID)).InBounds(msg)
				inHeader := chat.JID == m.selectedJID && zone.Get(chatHeaderZoneID(chat.JID)).InBounds(msg)
				if !inSidebar && !inHeader {
					continue
				}
				wasActive := m.activeChat()
				m.focus = focusChatList
				m.input.Blur()
				m.renameJID = chat.JID
				m.renameInput.SetValue(chat.DisplayName())
				m.renameInput.CursorEnd()
				m.renameInput.Focus()
				m.statusLine = "Cambia el nombre y pulsa Enter para guardar · Esc cancela"
				if wasActive {
					return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)
				}
				return m, nil
			}
		}
		if m.connectionKnown && !m.connected {
			return m, nil
		}
		if m.callPanel {
			return m, nil
		}
		if m.multiSelectMode && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			for _, message := range m.messages {
				if message.ID == "" {
					continue
				}
				if !zone.Get(msgZoneID(message.ID)).InBounds(msg) {
					continue
				}
				if m.multiSelectIDs[message.ID] {
					delete(m.multiSelectIDs, message.ID)
				} else {
					m.multiSelectIDs[message.ID] = true
				}
				m.statusLine = fmt.Sprintf("%d seleccionados · click izq. marca/desmarca · click der. abre acciones · Esc cancela", len(m.multiSelectIDs))
				m.viewport.SetContent(m.renderMessages())
				break
			}
			return m, nil
		}

		// A second right-click while selecting opens the batch action menu
		// for everything marked so far, instead of toggling more messages —
		// mirrors WhatsApp's "finish" gesture and keeps the whole flow
		// mouse-driven instead of needing f/c/Ctrl+D on the keyboard.
		if m.multiSelectMode && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonRight {
			m.contextMenuClickX = msg.X
			m.contextMenuClickY = msg.Y
			if !m.openMultiActionMenu() {
				m.statusLine = "Marca al menos un mensaje primero (click izquierdo)"
			}
			return m, nil
		}

		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonRight {
			matched := false
			for _, message := range m.messages {
				if message.ID == "" {
					continue
				}
				if zone.Get(msgZoneID(message.ID)).InBounds(msg) {
					matched = true
					m.contextMenuClickX = msg.X
					m.contextMenuClickY = msg.Y
					m.openContextMenu(message.ID)
					m.viewport.SetContent(m.renderMessages())
					break
				}
			}
			if !matched {
				m.statusLine = fmt.Sprintf("[debug] sin match click=(%d,%d) yoffset=%d total_msgs=%d", msg.X, msg.Y, m.viewport.YOffset, len(m.messages))
			}
			return m, nil
		}

		if m.contextMenuOpen {
			if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
				for i := range m.contextMenuItems {
					if zone.Get(ctxItemZoneID(i)).InBounds(msg) {
						m.contextMenuCursor = i
						return m.commitContextMenu()
					}
				}
				m.closeContextMenu()
			}
			return m, nil
		}

		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			for _, message := range m.messages {
				_, _, quotedID, hasQuote := m.getQuotedInfo(message)
				if !hasQuote || quotedID == "" {
					continue
				}
				if !zone.Get(quoteZoneID(message.ID)).InBounds(msg) {
					continue
				}
				if m.jumpToMessage(quotedID) {
					m.statusLine = "Saltado al mensaje citado"
					return m, clearJumpHighlightCmd(m.jumpHighlightSeq)
				} else {
					m.statusLine = "El mensaje citado ya no está cargado"
				}
				return m, nil
			}
			// A direct click on a voice note uses the same mpv playback path as
			// the keyboard selector. Quote links above keep their own action.
			for _, message := range m.messages {
				if message.ID == "" || !isVoiceMessage(message) || !zone.Get(msgZoneID(message.ID)).InBounds(msg) {
					continue
				}
				m.audioSelectMode, m.audioPlayMode = false, false
				m.selectedAudioID = message.ID
				updated, cmd := m.handlePlayAudio()
				m = updated.(model)
				m.selectedAudioID = ""
				m.viewport.SetContent(m.renderMessages())
				return m, cmd
			}
		}

		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		if m.viewport.YOffset == 0 && m.hasOlder && !m.loadingOlder {
			m.loadingOlder = true
			return m, tea.Batch(cmd, loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, m.messageOffset))
		}
		return m, cmd

	case tea.KeyMsg:
		m.noteInput()
		return m.handleKey(msg)
	}

	return m, nil
}

func (m *model) setConnectionState(connected bool, reason string) {
	wasConnected := m.connected
	m.connected = connected
	m.connectionKnown = true
	if connected {
		m.connectionNotice = ""
		if !wasConnected {
			m.statusLine = "Conexión restablecida. Ya puedes escribir."
		}
		return
	}
	if reason != "" {
		m.connectionNotice = reason
	} else if m.connectionNotice == "" {
		m.connectionNotice = "Sin conexión con WhatsApp. Esperando reconexión; pulsa q para reiniciar wa-cli."
	}
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		if m.isRecordingAudio {
			m.cancelAudioRecording()
		}
		if m.isDictating {
			m.cancelDictation()
		}
		if m.dictationCancel != nil {
			m.dictationCancel()
			m.dictationCancel = nil
		}
		m.quitting = true
		return m, tea.Quit
	}
	if updated, handled := m.handleBotLevelKey(msg.String()); handled {
		return updated, nil
	}
	if updated, handled := m.handleBotToggleKey(msg.String()); handled {
		m = updated
		return m, nil
	}
	if m.renameJID != "" {
		switch msg.String() {
		case "esc":
			if m.renameSaving {
				return m, nil
			}
			m.renameJID = ""
			m.renameSaving = false
			m.renameInput.Blur()
			m.statusLine = "Cambio de nombre cancelado"
			return m, nil
		case "enter":
			if m.renameSaving {
				return m, nil
			}
			m.renameSaving = true
			m.statusLine = "Guardando nombre..."
			return m, renameChatCmd(m.cs, m.renameJID, strings.TrimSpace(m.renameInput.Value()))
		}
		if m.renameSaving {
			return m, nil
		}
		var cmd tea.Cmd
		m.renameInput, cmd = m.renameInput.Update(msg)
		stripLeakedMouseSequences(&m.renameInput)
		return m, cmd
	}
	if m.dictationPendingText != "" && m.selectedJID == m.dictationChatJID {
		m.insertDictation(m.dictationPendingText)
		m.recordDictation(m.dictationPendingText)
		m.dictationPendingText = ""
		m.statusLine = "Dictado insertado; revísalo y pulsa Enter para enviar"
	}
	if m.isDictating {
		switch msg.String() {
		case " ":
			m.lastSpaceTime = time.Now()
			return m, nil
		case "esc":
			m.cancelDictation()
			return m, nil
		default:
			return m.stopDictation()
		}
	}
	if m.dictationProcessing {
		if msg.String() == "esc" {
			if m.dictationCancel != nil {
				m.dictationCancel()
				m.dictationCancel = nil
			}
			m.dictationSeq++
			m.dictationProcessing = false
			m.restoreDictationDraft()
			m.statusLine = "Dictado cancelado"
		} else {
			m.statusLine = "Terminando transcripción… Esc cancela"
		}
		return m, nil
	}
	if m.connectionKnown && !m.connected {
		if msg.String() == "q" {
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil
	}
	if m.callPanel {
		return m.handleCallKey(msg)
	}
	switch msg.String() {
	case "f4":
		m.callPanel = true
		m.callTarget = m.selectedJID
		m.statusLine = ""
		if m.calls != nil {
			m.callState = m.calls.Snapshot()
		}
		return m, nil
	case "f5":
		return m.callAction("answer")
	case "f6":
		return m.callAction("end")
	}
	if msg.String() == "ctrl+@" || msg.String() == "ctrl+space" {
		m.keepPresenceOnBlur = !m.keepPresenceOnBlur
		if m.keepPresenceOnBlur {
			m.statusLine = "Presencia y vistos fuera de pestaña ACTIVADOS (Ctrl+Space desactiva)"
		} else {
			m.statusLine = "Presencia y vistos fuera de pestaña DESACTIVADOS (Ctrl+Space activa)"
		}
		return m, nil
	}

	if m.isRecordingAudio {
		switch msg.String() {
		case "enter", "ctrl+r":
			return m.stopAndSendAudioRecording()
		case "esc":
			m.cancelAudioRecording()
			return m, nil
		}
	}

	if m.helpVisible {
		if msg.String() == "esc" || msg.String() == "?" || msg.String() == "enter" {
			m.helpVisible = false
		}
		return m, nil
	}

	if m.contextMenuOpen {
		switch msg.String() {
		case "up", "k":
			m.contextMenuCursor--
			if m.contextMenuCursor < 0 {
				m.contextMenuCursor = len(m.contextMenuItems) - 1
			}
		case "down", "j":
			m.contextMenuCursor++
			if m.contextMenuCursor >= len(m.contextMenuItems) {
				m.contextMenuCursor = 0
			}
		case "enter":
			return m.commitContextMenu()
		case "esc":
			m.closeContextMenu()
		}
		return m, nil
	}

	if m.deleteConfirm {
		switch msg.String() {
		case "left", "h", "right", "l", "tab":
			m.deleteConfirmed = !m.deleteConfirmed
			return m, nil
		case "y":
			m.deleteConfirmed = true
		case "n", "esc":
			m.deleteConfirm = false
			m.deleteConfirmed = false
			m.statusLine = "Eliminación cancelada"
			return m, nil
		case "enter":
			if !m.deleteConfirmed {
				m.deleteConfirm = false
				m.statusLine = "Eliminación cancelada"
				return m, nil
			}
			return m.confirmDeleteMessage()
		}
		return m, nil
	}
	if m.forwardConfirm {
		switch msg.String() {
		case "left", "h", "right", "l", "tab":
			m.forwardConfirmed = !m.forwardConfirmed
			return m, nil
		case "y":
			m.forwardConfirmed = true
		case "n", "esc":
			m.forwardConfirm = false
			m.forwardConfirmed = false
			m.statusLine = "Reenvío cancelado"
			return m, nil
		case "enter":
			if !m.forwardConfirmed {
				m.forwardConfirm = false
				m.statusLine = "Reenvío cancelado"
				return m, nil
			}
			return m.confirmForwardMessage()
		}
		return m, nil
	}

	// Guarded by !forwardDestinationMode so that once "f" opens the
	// destination picker, ↑/↓/Enter/Esc fall through to the same
	// forwardDestinationMode handling the single-message flow already uses
	// further below — multi-select just supplies more than one message to it.
	if m.multiSelectMode && !m.forwardDestinationMode {
		switch msg.String() {
		case "esc":
			m.multiSelectMode = false
			m.multiSelectIDs = nil
			m.statusLine = "Selección cancelada"
			m.viewport.SetContent(m.renderMessages())
			return m, nil
		case "f":
			return m.startMultiForward()
		case "c":
			return m.copyMultiSelect()
		case "ctrl+d":
			return m.startMultiDelete()
		}
	}

	if msg.String() == "ctrl+r" {
		m.startAudioRecording()
		return m, nil
	}

	if msg.String() == "ctrl+f" {
		m.searching = true
		m.searchInput.SetValue(m.searchTerm)
		m.searchInput.Focus()
		// Searching is not active participation in the chat. Drop presence
		// before moving keyboard focus away from the composer so WhatsApp does
		// not keep showing this account online while browsing history.
		m.focus = focusChatList
		m.input.Blur()
		m.statusLine = "Escribe una palabra o frase y pulsa Enter"
		return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)
	}

	if m.searching {
		switch msg.String() {
		case "esc":
			m.searching = false
			m.searchInput.Blur()
			m.statusLine = "Búsqueda cancelada"
			return m, nil
		case "enter":
			m.searchTerm = strings.TrimSpace(m.searchInput.Value())
			m.searching = false
			m.searchInput.Blur()
			m.focus = focusChatList
			m.input.Blur()
			m.searchMatchIndex = -1
			if m.searchTerm == "" {
				m.statusLine = "Escribe una palabra o frase para buscar"
				return m, nil
			}
			m.statusLine = "Buscando en el historial guardado..."
			return m, searchMessagesCmd(m.ctx, m.ms, m.client, m.selectedJID, m.searchTerm)
		}
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		stripLeakedMouseSequences(&m.searchInput)
		return m, cmd
	}

	// Kitty and other terminal emulators commonly expose a dropped file as a
	// bracketed paste containing its path. Recognize complete local paths and
	// upload them directly instead of inserting a path string into the chat.
	if msg.Paste && !m.documentPicker && !m.imageSelectMode && !m.videoSelectMode && !m.documentSelectMode {
		if paths, ok := parseDroppedFiles(string(msg.Runes)); ok {
			if m.selectedJID == "" {
				m.statusLine = "Selecciona un chat antes de soltar archivos"
				return m, nil
			}
			m.statusLine = fmt.Sprintf("Enviando %d archivo(s) arrastrado(s)...", len(paths))
			return m, tea.Batch(
				sendDroppedFilesCmd(m.ctx, m.client, m.guard, m.selectedJID, paths),
				chatInactiveCmd(m.ctx, m.client, m.selectedJID),
			)
		}
	}

	// Message history has its own viewport. Keep these bindings global so
	// history can be reviewed whether the chat list or input has focus.
	switch msg.String() {
	case "pgup":
		m.viewport.HalfViewUp()
		if m.viewport.YOffset == 0 && m.hasOlder && !m.loadingOlder {
			m.loadingOlder = true
			m.statusLine = "Cargando mensajes anteriores..."
			return m, loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, m.messageOffset)
		}
		return m, nil
	case "pgdown":
		m.viewport.HalfViewDown()
		return m, nil
	case "ctrl+d":
		m.deleteSelectMode = true
		m.selectedDeleteID = ""
		m.focus = focusChatList
		m.input.Blur()
		if !m.moveDeleteSelection(-1) {
			m.deleteSelectMode = false
			m.statusLine = "No hay mensajes tuyos para eliminar"
		}
		return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)
	case "ctrl+u":
		m.videoSelectMode = true
		m.selectedVideoID = ""
		m.focus = focusChatList
		m.input.Blur()
		if !m.moveVideoSelection(-1) {
			m.videoSelectMode = false
		}
		return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)
	case "ctrl+o":
		m.imageSelectMode = true
		m.selectedImageID = ""
		m.focus = focusChatList
		m.input.Blur()
		if !m.moveImageSelection(-1) {
			m.imageSelectMode = false
		}
		return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)
	case "ctrl+l":
		return m.handleOpenLink()
	case "ctrl+v", "ctrl+shift+v":
		return m.handleSendImageFromClipboardOrPath()
	case "ctrl+e":
		if m.selectedJID == "" {
			m.statusLine = "Selecciona un chat primero"
			return m, nil
		}
		m.statusLine = "Abriendo selector de documentos..."
		return m, pickDocumentsCmd()
	case "ctrl+s":
		if m.selectedJID == "" {
			m.statusLine = "Selecciona un chat primero"
			return m, nil
		}
		m.statusLine = "Abriendo selector de stickers..."
		return m, pickStickerCmd()
	case "ctrl+h":
		return m.handleDownloadDocument()
	case "ctrl+t":
		if m.focus == focusChatList {
			m.audioSelectMode = true
			m.audioPlayMode = false
			m.moveAudioSelection(-1)
			return m, nil
		}
		return m.handleTranscribeAudio()
	case "ctrl+up", "alt+up":
		m.selectReplyMessage(-1)
		return m, nil
	case "ctrl+down", "alt+down":
		m.selectReplyMessage(1)
		return m, nil
	}

	if msg.String() == "tab" {
		if m.focus == focusChatList {
			m.focus = focusInput
			m.input.Focus()
			return m, m.activateChatCmd()
		} else {
			m.focus = focusChatList
			m.input.Blur()
			return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)
		}
	}

	if m.focus == focusChatList {
		switch msg.String() {
		case "?":
			m.helpVisible = true
			return m, nil
		case "s":
			return m, m.revealVisibleStickers()
		case "p":
			m.audioSelectMode = true
			m.audioPlayMode = true
			m.moveAudioSelection(-1)
			return m, nil
		case "b":
			m.sidebarHidden = !m.sidebarHidden
			m.layout()
			if m.sidebarHidden {
				m.statusLine = "Barra de chats oculta (b para mostrar)"
			} else {
				m.statusLine = "Barra de chats visible"
			}
			return m, tea.ClearScreen
		case "up", "k":
			if m.documentSelectMode {
				m.moveDocumentSelection(-1)
				return m, nil
			}
			if m.forwardDestinationMode {
				m.moveForwardDestination(-1)
				return m, nil
			}
			if m.forwardSelectMode {
				m.moveForwardSelection(-1)
				return m, nil
			}
			if m.imageSelectMode {
				m.moveImageSelection(-1)
				return m, nil
			}
			if m.videoSelectMode {
				m.moveVideoSelection(-1)
				return m, nil
			}
			if m.copySelectMode {
				m.moveCopySelection(-1)
				return m, nil
			}
			if m.deleteSelectMode {
				m.moveDeleteSelection(-1)
				return m, nil
			}
			if m.audioSelectMode {
				m.moveAudioSelection(-1)
				return m, nil
			}
			if m.searchTerm != "" {
				m.moveSearchMatch(-1)
				return m, nil
			}
			m.moveSelection(-1)
			return m, loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0)
		case "down", "j":
			if m.documentSelectMode {
				m.moveDocumentSelection(1)
				return m, nil
			}
			if m.forwardDestinationMode {
				m.moveForwardDestination(1)
				return m, nil
			}
			if m.forwardSelectMode {
				m.moveForwardSelection(1)
				return m, nil
			}
			if m.imageSelectMode {
				m.moveImageSelection(1)
				return m, nil
			}
			if m.videoSelectMode {
				m.moveVideoSelection(1)
				return m, nil
			}
			if m.copySelectMode {
				m.moveCopySelection(1)
				return m, nil
			}
			if m.deleteSelectMode {
				m.moveDeleteSelection(1)
				return m, nil
			}
			if m.audioSelectMode {
				m.moveAudioSelection(1)
				return m, nil
			}
			if m.searchTerm != "" {
				m.moveSearchMatch(1)
				return m, nil
			}
			m.moveSelection(1)
			return m, loadMessagesCmd(m.ctx, m.cs, m.ms, m.client, m.selectedJID, 0)
		case "enter":
			if m.documentSelectMode {
				return m.downloadSelectedDocument()
			}
			if m.forwardDestinationMode {
				if m.forwardDestinationJID != "" {
					m.forwardDestinationMode = false
					m.forwardConfirm = true
					m.forwardConfirmed = false
				}
				return m, nil
			}
			if m.forwardSelectMode {
				if _, ok := m.selectedForwardMessage(); ok {
					m.forwardSelectMode = false
					m.forwardDestinationMode = true
					m.forwardDestinationJID = m.initialForwardDestination()
					m.statusLine = "Elige el chat destino · ↑/↓ cambia · Enter continúa · Esc cancela"
				}
				return m, nil
			}
			if m.imageSelectMode {
				return m.openSelectedImage()
			}
			if m.videoSelectMode {
				return m.openSelectedVideo()
			}
			if m.copySelectMode {
				return m.copySelectedMessage()
			}
			if m.deleteSelectMode {
				if m.selectedDeleteID != "" {
					m.deleteConfirm = true
					m.deleteConfirmed = false
				}
				return m, nil
			}
			// Selector terminal node: Enter transcribes the marked audio rather
			// than activating the chat or focusing the composer.
			if m.audioSelectMode {
				if m.audioPlayMode {
					m.audioSelectMode = false
					m.audioPlayMode = false
					return m.handlePlayAudio()
				}
				return m.handleTranscribeAudio()
			}
			if m.selectedJID != "" {
				m.focus = focusInput
				m.input.Focus()
			}
			return m, m.activateChatCmd()
		case "o":
			return m.handleOpenMedia()
		case "d":
			if m.videoSelectMode {
				return m.saveSelectedVideo()
			}
		case "u":
			return m.handleOpenVideo()
		case "l":
			return m.handleOpenLink()
		case "r":
			m.startAudioRecording()
			return m, nil
		case "c":
			m.copySelectMode = true
			m.selectedCopyID = ""
			if !m.moveCopySelection(-1) {
				m.copySelectMode = false
			}
			return m, nil
		case "R":
			if m.selectReplyMessage(-1) {
				m.focus = focusInput
				m.input.Focus()
				return m, m.activateChatCmd()
			}
			return m, nil
		case "f":
			m.forwardSelectMode = true
			m.selectedForwardID = ""
			if !m.moveForwardSelection(-1) {
				m.forwardSelectMode = false
			}
			return m, nil
		case "t":
			return m.handleTranscribeAudio()
		case "T":
			m.audioSelectMode = true
			m.audioPlayMode = false
			m.moveAudioSelection(-1)
			return m, nil
		case "esc":
			if m.documentSelectMode {
				m.documentSelectMode = false
				m.selectedDocumentID = ""
				m.statusLine = "Descarga de documento cancelada"
				m.viewport.SetContent(m.renderMessages())
			} else if m.forwardDestinationMode {
				m.forwardDestinationMode = false
				m.forwardDestinationJID = ""
				if m.multiSelectMode {
					m.statusLine = fmt.Sprintf("%d seleccionados · f reenviar · c copiar · Ctrl+D eliminar · Esc cancelar", len(m.multiSelectIDs))
				} else {
					m.forwardSelectMode = true
					m.statusLine = "Vuelve a elegir el mensaje · ↑/↓ cambia · Enter continúa · Esc cancela"
				}
			} else if m.forwardSelectMode {
				m.forwardSelectMode = false
				m.selectedForwardID = ""
				m.statusLine = "Reenvío cancelado"
				m.viewport.SetContent(m.renderMessages())
			} else if m.imageSelectMode {
				m.imageSelectMode = false
				m.selectedImageID = ""
				m.statusLine = "Selector de fotos cancelado"
				m.viewport.SetContent(m.renderMessages())
			} else if m.videoSelectMode {
				m.videoSelectMode = false
				m.selectedVideoID = ""
				m.statusLine = "Selector de videos cancelado"
				m.viewport.SetContent(m.renderMessages())
			} else if m.copySelectMode {
				m.copySelectMode = false
				m.selectedCopyID = ""
				m.statusLine = "Copia cancelada"
				m.viewport.SetContent(m.renderMessages())
			} else if m.deleteSelectMode {
				m.deleteSelectMode = false
				m.selectedDeleteID = ""
				m.viewport.SetContent(m.renderMessages())
				m.statusLine = "Eliminación cancelada"
			} else if m.audioSelectMode {
				m.audioSelectMode = false
				m.selectedAudioID = ""
				m.audioPlayMode = false
				m.statusLine = "Selector de audio cancelado"
				m.viewport.SetContent(m.renderMessages())
			} else if m.searchTerm != "" {
				m.searchTerm = ""
				m.searchMatchIndex = -1
				m.statusLine = "Búsqueda cerrada"
				m.viewport.SetContent(m.renderMessages())
			}
			return m, nil
		case "q":
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil
	}

	// focusInput
	if msg.String() == " " {
		now := time.Now()
		gap := now.Sub(m.lastSpaceTime)
		if gap < 900*time.Millisecond {
			m.spaceRepeatCount++
			if gap < 160*time.Millisecond {
				m.spaceRapidCount++
			}
		} else {
			m.spaceRepeatCount = 1
			m.spaceRapidCount = 0
			m.spaceRunStart = now
		}
		m.lastSpaceTime = now

		if m.spaceRepeatCount >= 4 && m.spaceRapidCount >= 2 && now.Sub(m.spaceRunStart) >= 250*time.Millisecond {
			if m.startDictation() {
				m.removeDictationWarmupSpaces(m.spaceRepeatCount - 1)
				m.dictationBaseDraft = m.input.Value()
				m.dictationInsertPos = m.input.Position()
				m.spaceRepeatCount = 0
				m.spaceRapidCount = 0
				return m, dictationTickCmd(m.dictationSeq)
			}
		}
	} else {
		m.spaceRepeatCount = 0
		m.spaceRapidCount = 0
	}

	switch msg.String() {
	case "esc":
		if m.editingMessageID != "" {
			m.editingMessageID = ""
			m.resetCompose()
			m.input.SetValue("")
			m.statusLine = "Edición cancelada"
			return m, nil
		}
		if m.replyToID != "" {
			m.replyToID = ""
			m.statusLine = "Respuesta cancelada"
			m.viewport.SetContent(m.renderMessages())
			return m, nil
		}
		m.focus = focusChatList
		m.input.Blur()
		return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" || m.selectedJID == "" {
			return m, nil
		}
		if m.editingMessageID != "" {
			message, ok := m.findMessage(m.editingMessageID)
			m.editingMessageID = ""
			m.resetCompose()
			m.input.SetValue("")
			if !ok {
				m.statusLine = "El mensaje ya no está disponible para editar"
				return m, nil
			}
			m.statusLine = "Guardando edición..."
			return m, editMessageCmd(m.ctx, m.client, m.ms, m.selectedJID, message, text)
		}
		replyTo, isReply := m.selectedReplyMessage()
		if paths, ok := parseDroppedFiles(text); ok {
			m.abandonCompose()
			m.input.SetValue("")
			m.statusLine = fmt.Sprintf("Enviando %d archivo(s)...", len(paths))
			return m, tea.Batch(
				sendDroppedFilesCmd(m.ctx, m.client, m.guard, m.selectedJID, paths),
				chatActivityCmd(m.ctx, m.client, m.selectedJID, false),
			)
		}

		composeMS := m.composeDuration()
		keyCount, backspaces := m.composeKeys, m.composeBackspaces
		m.resetCompose()
		m.input.SetValue("")
		m.replyToID = ""

		// Show it immediately rather than waiting for the send to
		// round-trip — sendResultMsg corrects the status line if it
		// actually failed.
		optimisticID := ""
		if isReply {
			if m.replyQuotes == nil {
				m.replyQuotes = map[string]msgstore.Message{}
			}
			optimisticID = fmt.Sprintf("optimistic-reply-%d", time.Now().UnixNano())
			m.replyQuotes[optimisticID] = replyTo
		}
		m.messages = append(m.messages, msgstore.Message{
			ID:        optimisticID,
			ChatJID:   m.selectedJID,
			Text:      text,
			FromMe:    true,
			Timestamp: time.Now().UnixMilli(),
		})
		m.viewport.SetContent(m.renderMessages())
		m.viewport.GotoBottom()

		sendText := sendCmd(m.ctx, m.client, m.guard, m.selectedJID, text)
		if isReply {
			sendText = sendReplyCmd(m.ctx, m.client, m.guard, m.selectedJID, text, replyTo)
			m.statusLine = "Enviando respuesta..."
		}
		originalSend := sendText
		chatJID := m.selectedJID
		quotedID := ""
		if isReply {
			quotedID = replyTo.ID
		}
		sendText = func() tea.Msg {
			raw := originalSend()
			result, ok := raw.(sendResultMsg)
			if !ok {
				return raw
			}
			result.chatJID = chatJID
			result.composeMS = composeMS
			result.charCount = int64(len([]rune(text)))
			result.keyCount = keyCount
			result.backspaces = backspaces
			result.quotedID = quotedID
			return result
		}
		return m, tea.Batch(
			sendText,
			markVisibleMessagesReadCmd(m.ctx, m.cs, m.client, m.selectedJID, m.messages),
			chatActivityCmd(m.ctx, m.client, m.selectedJID, false),
		)
	}

	wasEmpty := strings.TrimSpace(m.input.Value()) == ""
	oldValue := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	stripLeakedMouseSequences(&m.input)
	isEmpty := strings.TrimSpace(m.input.Value()) == ""
	if !m.composeAt.IsZero() || (wasEmpty && !isEmpty) {
		m.composeKeys++
		if msg.String() == "backspace" || msg.String() == "delete" {
			m.composeBackspaces++
		}
	}
	if wasEmpty && !isEmpty && m.composeAt.IsZero() {
		m.composeAt = time.Now()
		m.metrics.Record(metrics.ComposeStart, m.selectedJID, "", 0, 0)
	}
	if !wasEmpty && isEmpty {
		if !m.composeAt.IsZero() {
			m.metrics.Record(metrics.ComposeAbandon, m.selectedJID, "", m.composeDuration(), int64(len([]rune(oldValue))))
		}
		m.resetCompose()
	}
	if wasEmpty != isEmpty {
		return m, tea.Batch(cmd, chatActivityCmd(m.ctx, m.client, m.selectedJID, !isEmpty))
	}
	return m, cmd
}

func (m *model) moveSelection(delta int) {
	if len(m.chats) == 0 {
		return
	}
	idx := 0
	for i, c := range m.chats {
		if c.JID == m.selectedJID {
			idx = i
			break
		}
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(m.chats) {
		idx = len(m.chats) - 1
	}
	if m.selectedJID != m.chats[idx].JID {
		m.abandonCompose()
		m.selectedJID = m.chats[idx].JID
		m.recordChatOpen()
	}
	m.messages = nil
	m.stickerReveal = map[string]bool{}
	m.stickerPending = nil
	m.replyToID = ""
	m.audioSelectMode = false
	m.selectedAudioID = ""
	m.audioPlayMode = false
	m.viewport.SetContent("")
}

// moveAudioSelection walks only voice notes in their chronological order.
// The selector starts at the newest note, matching the natural reading order
// of the chat, while ↑/↓ can move to any other audio without changing chats.
func (m *model) moveAudioSelection(delta int) {
	indices := make([]int, 0)
	for i, msg := range m.messages {
		if isVoiceMessage(msg) && msg.ID != "" {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		m.selectedAudioID = ""
		m.statusLine = "No hay notas de voz en este chat"
		return
	}

	current := -1
	for pos, idx := range indices {
		if m.messages[idx].ID == m.selectedAudioID {
			current = pos
			break
		}
	}
	if current < 0 {
		if delta < 0 {
			current = len(indices) - 1
		} else {
			current = 0
		}
	} else {
		next := current + delta
		if next < 0 || next >= len(indices) {
			m.statusLine = "No hay más audios en esa dirección"
			return
		}
		current = next
	}

	selected := m.messages[indices[current]]
	m.selectedAudioID = selected.ID
	action := "transcribe"
	if m.audioPlayMode {
		action = "reproducir"
	}
	m.statusLine = "Audio seleccionado · ↑/↓ cambia · Enter " + action + " · Esc cancela"
	m.viewport.SetContent(m.renderMessages())
	m.ensureAudioMessageVisible(delta)
}

// ensureAudioMessageVisible mirrors reply navigation: when moving toward
// older audio, lift the viewport only if the marked note has left through the
// top edge. Moving toward newer audio never drags the reader away from the
// current position.
func (m *model) ensureAudioMessageVisible(delta int) {
	if delta >= 0 || m.selectedAudioID == "" || m.viewport.Height <= 0 {
		return
	}
	rendered := m.renderMessages()
	marker := strings.Index(rendered, "➤ ")
	if marker < 0 {
		return
	}
	start := strings.Count(rendered[:marker], "\n")
	if start < m.viewport.YOffset {
		m.viewport.SetYOffset(start - m.viewport.Height/2)
	}
}

// selectedReplyMessage returns the message currently marked for a quoted
// reply. Messages without an ID cannot be quoted by WhatsApp, so they are
// deliberately never selectable.
func (m model) selectedReplyMessage() (msgstore.Message, bool) {
	if m.replyToID == "" {
		return msgstore.Message{}, false
	}
	for _, msg := range m.messages {
		if msg.ID == m.replyToID {
			return msg, true
		}
	}
	return msgstore.Message{}, false
}

// selectReplyMessage moves through persisted messages and exposes the target
// both in the transcript and status line. When no target exists, start at the
// newest message; this makes `r` in the chat list a one-key reply shortcut.
func (m *model) selectReplyMessage(delta int) bool {
	if len(m.messages) == 0 {
		m.statusLine = "No hay mensajes para responder en este chat"
		return false
	}

	current := -1
	if m.replyToID != "" {
		for i, msg := range m.messages {
			if msg.ID == m.replyToID {
				current = i
				break
			}
		}
	}
	if current == -1 {
		current = len(m.messages)
		if delta > 0 {
			current = -1
		}
	}

	for {
		current += delta
		if current < 0 || current >= len(m.messages) {
			m.statusLine = "No hay más mensajes en esa dirección"
			return false
		}
		if m.messages[current].ID != "" {
			m.replyToID = m.messages[current].ID
			preview := m.messages[current].Text
			if preview == "" && m.messages[current].MediaType != "" {
				preview = "[" + m.messages[current].MediaType + "]"
			}
			if preview == "" {
				preview = "mensaje"
			}
			if len(preview) > 48 {
				preview = preview[:48] + "…"
			}
			m.statusLine = "Respondiendo a: " + preview + "  (Ctrl+↑/↓ cambia; Esc cancela)"
			m.viewport.SetContent(m.renderMessages())
			m.ensureReplyMessageVisible(delta)
			return true
		}
	}
}

// moveCopySelection walks messages which have text that can be put on the
// clipboard. It uses an ID rather than an index so incoming messages do not
// change the selected target.
func (m *model) moveCopySelection(delta int) bool {
	if len(m.messages) == 0 {
		m.statusLine = "No hay mensajes para copiar en este chat"
		return false
	}

	current := -1
	if m.selectedCopyID != "" {
		for i, message := range m.messages {
			if message.ID == m.selectedCopyID {
				current = i
				break
			}
		}
	}
	if current == -1 {
		current = len(m.messages)
		if delta > 0 {
			current = -1
		}
	}

	for {
		current += delta
		if current < 0 || current >= len(m.messages) {
			m.statusLine = "No hay más mensajes con texto en esa dirección"
			return false
		}
		message := m.messages[current]
		if message.ID == "" || strings.TrimSpace(message.Text) == "" {
			continue
		}
		m.selectedCopyID = message.ID
		preview := strings.TrimSpace(message.Text)
		if len(preview) > 48 {
			preview = preview[:48] + "…"
		}
		m.statusLine = "Copiar: " + preview + "  (↑/↓ cambia; Enter copia; Esc cancela)"
		m.viewport.SetContent(m.renderMessages())
		m.ensureSelectedMessageVisible(message.ID)
		return true
	}
}

func (m model) copySelectedMessage() (tea.Model, tea.Cmd) {
	for _, message := range m.messages {
		if message.ID != m.selectedCopyID {
			continue
		}
		text := strings.TrimSpace(message.Text)
		if text == "" {
			break
		}
		if err := copyTextToClipboard(text); err != nil {
			m.statusLine = "No se pudo copiar: " + err.Error()
			return m, nil
		}
		m.copySelectMode = false
		m.selectedCopyID = ""
		m.statusLine = "Mensaje copiado al portapapeles"
		m.viewport.SetContent(m.renderMessages())
		return m, nil
	}
	m.copySelectMode = false
	m.selectedCopyID = ""
	m.statusLine = "El mensaje seleccionado ya no está disponible"
	return m, nil
}

func msgZoneID(id string) string         { return "msg:" + id }
func chatNameZoneID(jid string) string   { return "chat-name:" + jid }
func chatHeaderZoneID(jid string) string { return "chat-header:" + jid }

func (m model) findMessage(id string) (msgstore.Message, bool) {
	for _, message := range m.messages {
		if message.ID == id {
			return message, true
		}
	}
	return msgstore.Message{}, false
}

func ctxItemZoneID(index int) string { return fmt.Sprintf("ctxitem:%d", index) }

func quoteZoneID(replyID string) string { return "quote:" + replyID }

// jumpToMessage scrolls so the given message sits roughly in the middle of
// the viewport, the way clicking a quoted-message header should feel —
// unlike ensureSelectedMessageVisible, which only nudges the view when a
// target has scrolled off an edge.
func (m *model) jumpToMessage(id string) bool {
	if m.viewport.Height <= 0 {
		return false
	}
	start, ok := m.messageStartLine(id)
	if !ok {
		return false
	}
	y := start - m.viewport.Height/2
	if y < 0 {
		y = 0
	}
	m.highlightedMessageID = id
	m.jumpHighlightSeq++
	m.viewport.SetContent(m.renderMessages())
	m.viewport.SetYOffset(y)
	return true
}

type contextMenuAction int

const (
	ctxActionReply contextMenuAction = iota
	ctxActionCopy
	ctxActionForward
	ctxActionDelete
	ctxActionMultiSelect
	ctxActionMultiForward
	ctxActionMultiCopy
	ctxActionMultiDelete
	ctxActionCancelSelection
	ctxActionViewPhoto
	ctxActionDownloadDoc
	ctxActionEdit
)

type contextMenuItem struct {
	label  string
	action contextMenuAction
}

// openContextMenu builds the right-click menu for one message, offering only
// the actions that message actually supports (e.g. no "Eliminar" on a message
// that isn't ours, no "Reenviar" without the retained WhatsApp protobuf).
func (m *model) openContextMenu(msgID string) bool {
	var target msgstore.Message
	found := false
	for _, message := range m.messages {
		if message.ID == msgID {
			target = message
			found = true
			break
		}
	}
	if !found || msgID == "" {
		return false
	}

	items := []contextMenuItem{{"Responder", ctxActionReply}}
	if strings.TrimSpace(target.Text) != "" {
		items = append(items, contextMenuItem{"Copiar", ctxActionCopy})
	}
	if isPhotoMessage(target) {
		items = append(items, contextMenuItem{"Ver foto", ctxActionViewPhoto})
	}
	if isDocumentMessage(target) {
		items = append(items, contextMenuItem{"Descargar documento", ctxActionDownloadDoc})
	}
	if target.RawProto != "" {
		items = append(items, contextMenuItem{"Reenviar", ctxActionForward})
	}
	if target.FromMe && target.MediaType == "" && strings.TrimSpace(target.Text) != "" {
		items = append(items, contextMenuItem{"Editar", ctxActionEdit})
	}
	if target.FromMe {
		items = append(items, contextMenuItem{"Eliminar para todos", ctxActionDelete})
	}
	items = append(items, contextMenuItem{"Seleccionar varios", ctxActionMultiSelect})

	m.contextMenuOpen = true
	m.contextMenuMsgID = msgID
	m.contextMenuItems = items
	m.contextMenuCursor = 0
	return true
}

// openMultiActionMenu is what a second right-click opens while multi-select
// is active: a menu acting on everything currently marked, instead of the
// per-message menu openContextMenu builds. multiSelectMode itself is left
// untouched here, so its checkboxes stay visible under the menu — only
// committing "Cancelar selección" (or completing copy) turns it off.
func (m *model) openMultiActionMenu() bool {
	if len(m.multiSelectIDs) == 0 {
		return false
	}
	hasText, hasForwardable, hasOwn := false, false, false
	for _, message := range m.messages {
		if !m.multiSelectIDs[message.ID] {
			continue
		}
		if strings.TrimSpace(message.Text) != "" {
			hasText = true
		}
		if message.RawProto != "" {
			hasForwardable = true
		}
		if message.FromMe {
			hasOwn = true
		}
	}

	var items []contextMenuItem
	if hasForwardable {
		items = append(items, contextMenuItem{"Reenviar seleccionados", ctxActionMultiForward})
	}
	if hasText {
		items = append(items, contextMenuItem{"Copiar seleccionados", ctxActionMultiCopy})
	}
	if hasOwn {
		items = append(items, contextMenuItem{"Eliminar seleccionados para todos", ctxActionMultiDelete})
	}
	items = append(items, contextMenuItem{"Cancelar selección", ctxActionCancelSelection})

	m.contextMenuOpen = true
	m.contextMenuMsgID = ""
	m.contextMenuItems = items
	m.contextMenuCursor = 0
	return true
}

func (m *model) closeContextMenu() {
	m.contextMenuOpen = false
	m.contextMenuMsgID = ""
	m.contextMenuItems = nil
	m.contextMenuCursor = 0
}

// commitContextMenu runs the currently highlighted action and hands control
// back to the same flows the keyboard shortcuts use (e.g. delete still goes
// through the Sí/No confirm modal), so right-click never skips a safety step.
func (m model) commitContextMenu() (tea.Model, tea.Cmd) {
	if m.contextMenuCursor < 0 || m.contextMenuCursor >= len(m.contextMenuItems) {
		m.closeContextMenu()
		return m, nil
	}
	msgID := m.contextMenuMsgID
	action := m.contextMenuItems[m.contextMenuCursor].action
	m.closeContextMenu()

	switch action {
	case ctxActionReply:
		m.replyToID = msgID
		for _, message := range m.messages {
			if message.ID != msgID {
				continue
			}
			preview := message.Text
			if preview == "" && message.MediaType != "" {
				preview = "[" + message.MediaType + "]"
			}
			if preview == "" {
				preview = "mensaje"
			}
			if len(preview) > 48 {
				preview = preview[:48] + "…"
			}
			m.statusLine = "Respondiendo a: " + preview + "  (Esc cancela)"
			break
		}
		m.viewport.SetContent(m.renderMessages())
		return m, nil
	case ctxActionCopy:
		m.selectedCopyID = msgID
		return m.copySelectedMessage()
	case ctxActionForward:
		m.selectedForwardID = msgID
		if _, ok := m.selectedForwardMessage(); !ok {
			m.statusLine = "El mensaje ya no está disponible para reenviar"
			return m, nil
		}
		m.forwardDestinationMode = true
		m.forwardDestinationJID = m.initialForwardDestination()
		m.statusLine = "Elige el chat destino · ↑/↓ cambia · Enter continúa · Esc cancela"
		return m, nil
	case ctxActionDelete:
		m.selectedDeleteID = msgID
		if _, ok := m.selectedDeleteMessage(); !ok {
			m.statusLine = "Ese mensaje no se puede eliminar para todos"
			return m, nil
		}
		m.deleteConfirm = true
		m.deleteConfirmed = false
		return m, nil
	case ctxActionMultiSelect:
		m.multiSelectMode = true
		m.multiSelectIDs = map[string]bool{msgID: true}
		m.statusLine = "1 seleccionado · click izq. marca más · click der. abre acciones · Esc cancela"
		m.viewport.SetContent(m.renderMessages())
		return m, nil
	case ctxActionMultiForward:
		return m.startMultiForward()
	case ctxActionMultiCopy:
		return m.copyMultiSelect()
	case ctxActionMultiDelete:
		return m.startMultiDelete()
	case ctxActionCancelSelection:
		m.multiSelectMode = false
		m.multiSelectIDs = nil
		m.statusLine = "Selección cancelada"
		m.viewport.SetContent(m.renderMessages())
		return m, nil
	case ctxActionViewPhoto:
		if message, ok := m.findMessage(msgID); ok {
			return m.openImage(message)
		}
		m.statusLine = "La foto ya no está disponible"
		return m, nil
	case ctxActionDownloadDoc:
		if message, ok := m.findMessage(msgID); ok {
			m.statusLine = "Elige la carpeta de descarga..."
			return m, pickDownloadFolderCmd(message)
		}
		m.statusLine = "El documento ya no está disponible"
		return m, nil
	case ctxActionEdit:
		if message, ok := m.findMessage(msgID); ok {
			m.editingMessageID = msgID
			m.replyToID = ""
			m.input.SetValue(message.Text)
			m.input.CursorEnd()
			m.focus = focusInput
			m.input.Focus()
			m.statusLine = "Editando mensaje · Enter guarda · Esc cancela"
			return m, m.activateChatCmd()
		}
		m.statusLine = "El mensaje ya no está disponible para editar"
		return m, nil
	}
	return m, nil
}

// moveForwardSelection walks messages whose original WhatsApp protobuf was
// retained locally. That payload is required to preserve the real content and
// WhatsApp's forwarded marker; an old text-only history record is not enough.
func (m *model) moveForwardSelection(delta int) bool {
	current := -1
	for i, message := range m.messages {
		if message.ID == m.selectedForwardID {
			current = i
			break
		}
	}
	if current == -1 {
		current = len(m.messages)
		if delta > 0 {
			current = -1
		}
	}
	for current += delta; current >= 0 && current < len(m.messages); current += delta {
		message := m.messages[current]
		if message.ID == "" || message.RawProto == "" {
			continue
		}
		m.selectedForwardID = message.ID
		m.statusLine = "Reenviar: mensaje marcado · ↑/↓ cambia · Enter elige destino · Esc cancela"
		m.viewport.SetContent(m.renderMessages())
		m.ensureSelectedMessageVisible(message.ID)
		return true
	}
	m.statusLine = "No hay más mensajes reenviables en esa dirección"
	return false
}

func (m model) selectedForwardMessage() (msgstore.Message, bool) {
	for _, message := range m.messages {
		if message.ID == m.selectedForwardID && message.RawProto != "" {
			return message, true
		}
	}
	return msgstore.Message{}, false
}

func (m model) initialForwardDestination() string {
	for _, chat := range m.chats {
		if chat.JID != m.selectedJID {
			return chat.JID
		}
	}
	return ""
}

func (m *model) moveForwardDestination(delta int) {
	if len(m.chats) == 0 {
		m.forwardDestinationJID = ""
		m.statusLine = "No hay chats disponibles como destino"
		return
	}
	index := -1
	for i, chat := range m.chats {
		if chat.JID == m.forwardDestinationJID {
			index = i
			break
		}
	}
	if index < 0 {
		m.forwardDestinationJID = m.initialForwardDestination()
		return
	}
	for next := index + delta; next >= 0 && next < len(m.chats); next += delta {
		if m.chats[next].JID == m.selectedJID {
			continue
		}
		m.forwardDestinationJID = m.chats[next].JID
		return
	}
	m.statusLine = "No hay más chats en esa dirección"
}

func (m model) forwardDestinationName() string {
	for _, chat := range m.chats {
		if chat.JID != m.forwardDestinationJID {
			continue
		}
		if chat.DisplayName() != "" {
			return chat.DisplayName()
		}
		if chat.IsGroup && m.groupNames[chat.JID] != "" {
			return m.groupNames[chat.JID]
		}
		return chat.JID
	}
	return "chat seleccionado"
}

func (m model) confirmForwardMessage() (tea.Model, tea.Cmd) {
	if m.multiSelectMode {
		return m.confirmMultiForward()
	}
	message, ok := m.selectedForwardMessage()
	if !ok || m.forwardDestinationJID == "" {
		m.forwardConfirm = false
		m.forwardDestinationMode = false
		m.forwardSelectMode = false
		m.statusLine = "El mensaje o destino ya no está disponible"
		return m, nil
	}
	m.forwardConfirm = false
	m.forwardDestinationMode = false
	m.forwardSelectMode = false
	m.statusLine = "Reenviando mensaje..."
	return m, forwardMessageCmd(m.ctx, m.client, m.guard, m.forwardDestinationJID, message)
}

// startMultiForward is the multi-select counterpart of pressing "f": instead
// of walking to one message, it opens the same destination picker for every
// selected message that still has its original WhatsApp protobuf retained.
func (m model) startMultiForward() (tea.Model, tea.Cmd) {
	count := 0
	for _, message := range m.messages {
		if m.multiSelectIDs[message.ID] && message.RawProto != "" {
			count++
		}
	}
	if count == 0 {
		m.statusLine = "Ningún mensaje seleccionado se puede reenviar (falta el original)"
		return m, nil
	}
	m.forwardDestinationMode = true
	m.forwardDestinationJID = m.initialForwardDestination()
	m.statusLine = fmt.Sprintf("Elige el chat destino para %d mensajes · ↑/↓ cambia · Enter continúa · Esc cancela", count)
	return m, nil
}

func (m model) confirmMultiForward() (tea.Model, tea.Cmd) {
	if m.forwardDestinationJID == "" {
		m.forwardConfirm = false
		m.forwardDestinationMode = false
		m.statusLine = "El destino ya no está disponible"
		return m, nil
	}
	var cmds []tea.Cmd
	for _, message := range m.messages {
		if m.multiSelectIDs[message.ID] && message.RawProto != "" {
			cmds = append(cmds, forwardMessageCmd(m.ctx, m.client, m.guard, m.forwardDestinationJID, message))
		}
	}
	m.forwardConfirm = false
	m.forwardDestinationMode = false
	m.multiSelectMode = false
	m.multiSelectIDs = nil
	m.viewport.SetContent(m.renderMessages())
	if len(cmds) == 0 {
		m.statusLine = "Ningún mensaje se pudo reenviar"
		return m, nil
	}
	m.statusLine = fmt.Sprintf("Reenviando %d mensajes...", len(cmds))
	return m, tea.Batch(cmds...)
}

// copyMultiSelect joins every selected message's text (in transcript order)
// and copies it as one clipboard block — the closest useful analog to
// WhatsApp's single-message copy when several are marked.
func (m model) copyMultiSelect() (tea.Model, tea.Cmd) {
	var texts []string
	for _, message := range m.messages {
		if !m.multiSelectIDs[message.ID] {
			continue
		}
		if t := strings.TrimSpace(message.Text); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		m.statusLine = "Ningún mensaje seleccionado tiene texto para copiar"
		return m, nil
	}
	if err := copyTextToClipboard(strings.Join(texts, "\n")); err != nil {
		m.statusLine = "No se pudo copiar: " + err.Error()
		return m, nil
	}
	m.multiSelectMode = false
	m.multiSelectIDs = nil
	m.statusLine = fmt.Sprintf("%d mensajes copiados al portapapeles", len(texts))
	m.viewport.SetContent(m.renderMessages())
	return m, nil
}

// startMultiDelete is the multi-select counterpart of Ctrl+D: it still routes
// through the existing Sí/No deleteConfirm modal, never skipping that safety
// step just because several messages are involved.
func (m model) startMultiDelete() (tea.Model, tea.Cmd) {
	count := 0
	for _, message := range m.messages {
		if m.multiSelectIDs[message.ID] && message.FromMe {
			count++
		}
	}
	if count == 0 {
		m.statusLine = "Ningún mensaje seleccionado es tuyo (no se puede eliminar para todos)"
		return m, nil
	}
	m.deleteConfirm = true
	m.deleteConfirmed = false
	return m, nil
}

func (m model) confirmMultiDelete() (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	for _, message := range m.messages {
		if m.multiSelectIDs[message.ID] && message.FromMe {
			cmds = append(cmds, revokeMessageCmd(m.ctx, m.client, m.ms, m.selectedJID, message))
		}
	}
	m.deleteConfirm = false
	m.multiSelectMode = false
	m.multiSelectIDs = nil
	m.viewport.SetContent(m.renderMessages())
	if len(cmds) == 0 {
		m.statusLine = "Ningún mensaje se pudo eliminar"
		return m, nil
	}
	m.statusLine = fmt.Sprintf("Eliminando %d mensajes...", len(cmds))
	return m, tea.Batch(cmds...)
}

func copyTextToClipboard(text string) error {
	commands := [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}}
	if runtime.GOOS == "darwin" {
		commands = [][]string{{"pbcopy"}}
	}
	var lastErr error
	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		return fmt.Errorf("no hay herramienta de portapapeles disponible")
	}
	return fmt.Errorf("instala wl-clipboard o xclip (%w)", lastErr)
}

// ensureReplyMessageVisible keeps the selected reply target on screen. Use the
// already-rendered transcript to count real visual lines, including wrapped
// messages and quote/media lines; estimating one line per message can move the
// viewport far away from the selected target.
func (m *model) ensureReplyMessageVisible(delta int) {
	// Moving toward newer messages must never pull the transcript down. The
	// automatic page movement is intentionally one-way: only Ctrl+↑ can move
	// the view upward when its target has left through the top edge.
	if delta >= 0 || m.replyToID == "" || m.viewport.Height <= 0 {
		return
	}
	start, ok := m.messageStartLine(m.replyToID)
	if !ok {
		return
	}
	if start < m.viewport.YOffset {
		m.viewport.SetYOffset(start - m.viewport.Height/2)
	}
}

// moveDeleteSelection walks only the account's own persisted messages, which
// are the only messages this client may revoke for everyone.
func (m *model) moveDeleteSelection(delta int) bool {
	current := -1
	for i, message := range m.messages {
		if message.ID == m.selectedDeleteID {
			current = i
			break
		}
	}
	if current == -1 {
		current = len(m.messages)
		if delta > 0 {
			current = -1
		}
	}
	for current += delta; current >= 0 && current < len(m.messages); current += delta {
		message := m.messages[current]
		if message.ID != "" && message.FromMe {
			m.selectedDeleteID = message.ID
			m.viewport.SetContent(m.renderMessages())
			m.ensureSelectedMessageVisible(message.ID)
			m.statusLine = "🗑 Mensaje marcado · ↑/↓ cambiar · Enter eliminar para todos · Esc cancelar"
			return true
		}
	}
	m.statusLine = "No hay más mensajes tuyos en esa dirección"
	return false
}

func (m *model) ensureSelectedMessageVisible(id string) {
	if m.viewport.Height <= 0 {
		return
	}
	start, ok := m.messageStartLine(id)
	if !ok {
		return
	}
	if start < m.viewport.YOffset {
		m.viewport.SetYOffset(start)
	} else if start >= m.viewport.YOffset+m.viewport.Height {
		m.viewport.SetYOffset(start - m.viewport.Height + 1)
	}
}

func (m model) selectedDeleteMessage() (msgstore.Message, bool) {
	for _, message := range m.messages {
		if message.ID == m.selectedDeleteID && message.FromMe {
			return message, true
		}
	}
	return msgstore.Message{}, false
}

func (m model) confirmDeleteMessage() (tea.Model, tea.Cmd) {
	if m.multiSelectMode {
		return m.confirmMultiDelete()
	}
	message, ok := m.selectedDeleteMessage()
	if !ok {
		m.deleteConfirm = false
		m.deleteSelectMode = false
		m.statusLine = "El mensaje ya no está disponible"
		return m, nil
	}
	m.deleteConfirm = false
	m.statusLine = "Eliminando mensaje para todos..."
	return m, revokeMessageCmd(m.ctx, m.client, m.ms, m.selectedJID, message)
}

func (m model) openDocumentPicker() (tea.Model, tea.Cmd) {
	if m.selectedJID == "" {
		m.statusLine = "Selecciona un chat primero"
		return m, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	m.documentPicker = true
	m.documentDir = home
	m.documentCursor = 0
	m.selectedDocuments = map[string]bool{}
	m.loadDocumentEntries()
	return m, nil
}

// pickDocumentsCmd opens a native GTK 3 file chooser. This avoids Zenity 4's
// GTK 4 hang under Hyprland while still following the desktop GTK theme.
func pickDocumentsCmd() tea.Cmd {
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		const script = `
import sys, gi
gi.require_version("Gtk", "3.0")
from gi.repository import Gtk

dialog = Gtk.FileChooserDialog(title="Adjuntar documentos", action=Gtk.FileChooserAction.OPEN)
dialog.add_buttons("Cancelar", Gtk.ResponseType.CANCEL, "Abrir", Gtk.ResponseType.OK)
dialog.set_select_multiple(True)
dialog.set_current_folder(sys.argv[1])
response = dialog.run()
if response == Gtk.ResponseType.OK:
    print("\n".join(dialog.get_filenames()))
dialog.destroy()
`
		output, err := exec.Command("python", "-c", script, home).Output()
		if err != nil {
			return documentsPickedMsg{err: err}
		}
		var paths []string
		for _, path := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if path = strings.TrimSpace(path); path != "" {
				paths = append(paths, path)
			}
		}
		return documentsPickedMsg{paths: paths}
	}
}

// pickStickerCmd accepts only WebP because WhatsApp requires that format for
// stickers. It uses the same graphical toolkit as Ctrl+E, but for one file.
func pickStickerCmd() tea.Cmd {
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		const script = `
import sys, gi
gi.require_version("Gtk", "3.0")
from gi.repository import Gtk

dialog = Gtk.FileChooserDialog(title="Elegir sticker", action=Gtk.FileChooserAction.OPEN)
dialog.add_buttons("Cancelar", Gtk.ResponseType.CANCEL, "Abrir", Gtk.ResponseType.OK)
dialog.set_current_folder(sys.argv[1])
file_filter = Gtk.FileFilter()
file_filter.set_name("Stickers WebP")
file_filter.add_pattern("*.webp")
dialog.add_filter(file_filter)
response = dialog.run()
if response == Gtk.ResponseType.OK:
    print(dialog.get_filename())
dialog.destroy()
`
		output, err := exec.Command("python", "-c", script, home).Output()
		if err != nil {
			return stickerPickedMsg{err: err}
		}
		return stickerPickedMsg{path: strings.TrimSpace(string(output))}
	}
}

func (m *model) loadDocumentEntries() {
	entries, err := os.ReadDir(m.documentDir)
	if err != nil {
		m.statusLine = "No se pudo abrir " + m.documentDir + ": " + err.Error()
		m.documentEntries = nil
		return
	}
	m.documentEntries = entries
	if m.documentCursor >= len(entries) {
		m.documentCursor = len(entries) - 1
	}
	if m.documentCursor < 0 {
		m.documentCursor = 0
	}
}

func (m model) handleDocumentPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.documentPicker = false
		m.statusLine = "Selector de documentos cancelado"
		return m, nil
	case "up", "k":
		if m.documentCursor > 0 {
			m.documentCursor--
		}
	case "down", "j":
		if m.documentCursor+1 < len(m.documentEntries) {
			m.documentCursor++
		}
	case "backspace", "left", "h":
		parent := filepath.Dir(m.documentDir)
		if parent != m.documentDir {
			m.documentDir = parent
			m.documentCursor = 0
			m.loadDocumentEntries()
		}
	case " ":
		if len(m.documentEntries) == 0 {
			return m, nil
		}
		entry := m.documentEntries[m.documentCursor]
		if !entry.IsDir() {
			path := filepath.Join(m.documentDir, entry.Name())
			m.selectedDocuments[path] = !m.selectedDocuments[path]
		}
	case "enter", "right", "l":
		if len(m.documentEntries) == 0 {
			return m, nil
		}
		entry := m.documentEntries[m.documentCursor]
		if entry.IsDir() {
			m.documentDir = filepath.Join(m.documentDir, entry.Name())
			m.documentCursor = 0
			m.loadDocumentEntries()
			return m, nil
		}
		if len(m.selectedDocuments) == 0 {
			m.selectedDocuments[filepath.Join(m.documentDir, entry.Name())] = true
		}
		paths := make([]string, 0, len(m.selectedDocuments))
		for path, selected := range m.selectedDocuments {
			if selected {
				paths = append(paths, path)
			}
		}
		if len(paths) == 0 {
			return m, nil
		}
		m.documentPicker = false
		m.statusLine = fmt.Sprintf("Enviando %d documento(s)...", len(paths))
		return m, sendDocumentsCmd(m.ctx, m.client, m.guard, m.selectedJID, paths)
	}
	return m, nil
}

func sendDocumentsCmd(ctx context.Context, client *whatsapp.Client, guard *safety.Guard, chatJID string, paths []string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return sendDocumentsResultMsg{chatJID: chatJID, err: fmt.Errorf("whatsapp client not connected")}
		}
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			return sendDocumentsResultMsg{chatJID: chatJID, err: err}
		}
		sent := 0
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				return sendDocumentsResultMsg{chatJID: chatJID, sent: sent, err: err}
			}
			mimeType := mime.TypeByExtension(filepath.Ext(path))
			if mimeType == "" {
				mimeType = http.DetectContentType(data)
			}
			if _, err := client.SendDocument(ctx, jid, data, mimeType, filepath.Base(path), ""); err != nil {
				return sendDocumentsResultMsg{chatJID: chatJID, sent: sent, err: err}
			}
			sent++
		}
		if guard != nil {
			_ = guard.MarkKnown(chatJID)
		}
		return sendDocumentsResultMsg{chatJID: chatJID, sent: sent}
	}
}

func parseDroppedFiles(payload string) ([]string, bool) {
	payload = strings.TrimSpace(strings.Trim(payload, "\x00"))
	if payload == "" {
		return nil, false
	}
	if path, ok := normalizeDroppedPath(payload); ok {
		return []string{path}, true
	}

	lines := strings.Split(payload, "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		path, ok := normalizeDroppedPath(line)
		if !ok {
			return nil, false
		}
		paths = append(paths, path)
	}
	return paths, len(paths) > 0
}

func normalizeDroppedPath(candidate string) (string, bool) {
	candidate = strings.TrimSpace(candidate)
	candidate = strings.Trim(candidate, `"'`)
	candidate = strings.ReplaceAll(candidate, `\ `, " ")
	if strings.HasPrefix(candidate, "file://") {
		parsed, err := url.Parse(candidate)
		if err != nil || (parsed.Host != "" && parsed.Host != "localhost") {
			return "", false
		}
		candidate = parsed.Path
	}
	if strings.HasPrefix(candidate, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		candidate = filepath.Join(home, candidate[2:])
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	return absolute, true
}

func sendDroppedFilesCmd(ctx context.Context, client *whatsapp.Client, guard *safety.Guard, chatJID string, paths []string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return sendDroppedFilesResultMsg{err: fmt.Errorf("whatsapp client not connected")}
		}
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			return sendDroppedFilesResultMsg{err: err}
		}
		sent := 0
		codes := make([]int64, 0, len(paths))
		warnings := []string{}
		for _, path := range paths {
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				if err == nil {
					err = fmt.Errorf("not a regular file")
				}
				return sendDroppedFilesResultMsg{sent: sent, codes: codes, chatJID: chatJID, err: fmt.Errorf("%s: %w", filepath.Base(path), err)}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return sendDroppedFilesResultMsg{sent: sent, codes: codes, chatJID: chatJID, err: fmt.Errorf("%s: %w", filepath.Base(path), err)}
			}
			mimeType := mime.TypeByExtension(filepath.Ext(path))
			if mimeType == "" || mimeType == "application/octet-stream" {
				mimeType = http.DetectContentType(data)
			}
			var sendErr error
			code := droppedMediaKind(path, mimeType)
			switch code {
			case 1:
				if !strings.HasPrefix(mimeType, "image/") {
					mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
				}
				_, sendErr = client.SendImage(ctx, jid, data, mimeType, "")
			case 2:
				videoData := data
				if mimeType != "video/mp4" && !strings.EqualFold(filepath.Ext(path), ".mp4") {
					converted, convertErr := convertDroppedVideo(ctx, path)
					if convertErr != nil {
						code = 5
						warnings = append(warnings, fmt.Sprintf("%s: no se pudo convertir a MP4 (%v); enviado como documento", filepath.Base(path), convertErr))
					} else {
						videoData = converted
					}
				}
				if code == 2 {
					_, sendErr = client.SendVideo(ctx, jid, videoData, "video/mp4", "")
				} else {
					_, sendErr = client.SendDocument(ctx, jid, data, mimeType, filepath.Base(path), "")
				}
			default:
				_, sendErr = client.SendDocument(ctx, jid, data, mimeType, filepath.Base(path), "")
			}
			if sendErr != nil {
				return sendDroppedFilesResultMsg{sent: sent, codes: codes, chatJID: chatJID, err: fmt.Errorf("%s: %w", filepath.Base(path), sendErr)}
			}
			sent++
			codes = append(codes, code)
		}
		if guard != nil {
			_ = guard.MarkKnown(chatJID)
		}
		return sendDroppedFilesResultMsg{sent: sent, codes: codes, chatJID: chatJID, warnings: warnings}
	}
}

func droppedMediaKind(path, mimeType string) int64 {
	ext := strings.ToLower(filepath.Ext(path))
	baseMime, _, _ := mime.ParseMediaType(mimeType)
	if baseMime == "" {
		baseMime = mimeType
	}
	if baseMime == "video/mp4" || strings.HasPrefix(baseMime, "video/") {
		return 2
	}
	switch ext {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".3gp", ".3g2", ".mpeg", ".mpg":
		return 2
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return 1
	}
	switch baseMime {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return 1
	}
	return 5
}

func convertDroppedVideo(ctx context.Context, path string) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg no está disponible")
	}
	tmp, err := os.CreateTemp("", "wa-cli-video-*.mp4")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", path, "-c:v", "libx264", "-c:a", "aac", "-movflags", "+faststart", tmpPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return os.ReadFile(tmpPath)
}

// editMessageCmd sends the edit to WhatsApp first — only on success does it
// touch msgstore/the in-memory message, so a rejected edit (outside
// WhatsApp's edit window, etc.) never desyncs the local copy from what the
// other side actually sees.
func editMessageCmd(ctx context.Context, client *whatsapp.Client, store *msgstore.Store, chatJID string, message msgstore.Message, newText string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return editResultMsg{err: fmt.Errorf("whatsapp client not connected")}
		}
		if err := client.EditMessage(ctx, chatJID, message.ID, newText); err != nil {
			return editResultMsg{err: err}
		}
		message.Text = newText
		if store != nil {
			if err := store.Append(message); err != nil {
				return editResultMsg{err: fmt.Errorf("mensaje editado, pero no se pudo actualizar el historial: %w", err)}
			}
		}
		return editResultMsg{message: message}
	}
}

func revokeMessageCmd(ctx context.Context, client *whatsapp.Client, store *msgstore.Store, chatJID string, message msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return deleteResultMsg{err: fmt.Errorf("whatsapp client not connected")}
		}
		if err := client.RevokeMessage(ctx, chatJID, message.ID); err != nil {
			return deleteResultMsg{err: err}
		}
		message.Text = "🚫 Eliminaste este mensaje"
		message.MediaType = ""
		message.Transcript = ""
		message.RawProto = ""
		if store != nil {
			if err := store.Append(message); err != nil {
				return deleteResultMsg{err: fmt.Errorf("mensaje eliminado, pero no se pudo actualizar el historial: %w", err)}
			}
		}
		return deleteResultMsg{message: message}
	}
}

// moveSearchMatch finds the next matching message in the requested direction.
// Searches start at the newest message, so ↑ naturally walks backwards through
// the conversation after Enter confirms the query.
func (m *model) moveSearchMatch(delta int) bool {
	query := strings.ToLower(strings.TrimSpace(m.searchTerm))
	if query == "" || len(m.messages) == 0 {
		return false
	}

	index := m.searchMatchIndex
	if index < 0 || index >= len(m.messages) {
		if delta < 0 {
			index = len(m.messages)
		} else {
			index = -1
		}
	}
	for index += delta; index >= 0 && index < len(m.messages); index += delta {
		message := m.messages[index]
		if strings.Contains(strings.ToLower(message.Text+"\n"+message.Transcript), query) {
			m.searchMatchIndex = index
			m.viewport.SetContent(m.renderMessages())
			m.ensureSearchMessageVisible(index)
			m.statusLine = "Resultado para \"" + m.searchTerm + "\" (↑ anterior, ↓ siguiente)"
			return true
		}
	}
	m.statusLine = "No hay más resultados para \"" + m.searchTerm + "\""
	return false
}

// ensureSearchMessageVisible scrolls precisely to the chosen result. Unlike
// reply selection, both directions should follow a search result on screen.
func (m *model) ensureSearchMessageVisible(index int) {
	if index < 0 || index >= len(m.messages) || m.viewport.Height <= 0 {
		return
	}
	start, ok := m.messageStartLine(m.messages[index].ID)
	if !ok {
		return
	}
	end := start + 1
	if start < m.viewport.YOffset {
		m.viewport.SetYOffset(start)
	} else if end > m.viewport.YOffset+m.viewport.Height {
		m.viewport.SetYOffset(end - m.viewport.Height)
	}
}

// messageStartLine returns the physical line where an ID begins in the
// rendered transcript. It deliberately renders the preceding messages rather
// than looking for decoration glyphs: transcripts and reply selection both use
// ↳, so glyph matching can scroll to an unrelated earlier voice note.
func (m model) messageStartLine(id string) (int, bool) {
	for i, msg := range m.messages {
		if msg.ID != id {
			continue
		}
		if i == 0 {
			return 0, true
		}
		prefix := m
		prefix.messages = m.messages[:i]
		prefix.replyToID = ""
		prefix.selectedAudioID = ""
		prefix.searchMatchIndex = -1
		prefix.selectedDeleteID = ""
		prefix.selectedCopyID = ""
		prefix.selectedImageID = ""
		prefix.selectedDocumentID = ""
		return strings.Count(prefix.renderMessages(), "\n"), true
	}
	return 0, false
}

func (m *model) layout() {
	bodyHeight := m.height - 4
	if bodyHeight < 5 {
		bodyHeight = 5
	}

	mainWidth := m.width - 2
	if !m.sidebarHidden {
		mainWidth -= sidebarWidth + 4
	}
	if mainWidth < 10 {
		mainWidth = 10
	}

	headerHeight := 2
	inputHeight := 3
	if m.replyToID != "" || m.deleteSelectMode || m.forwardSelectMode || m.forwardDestinationMode {
		inputHeight = 4 // action banner plus the input box
	}
	vpHeight := bodyHeight - headerHeight - inputHeight
	if vpHeight < 3 {
		vpHeight = 3
	}

	m.viewport.Width = mainWidth
	m.viewport.Height = vpHeight
	m.input.Width = mainWidth - 4
	m.searchInput.Width = mainWidth - 4
	if len(m.messages) > 0 {
		m.viewport.SetContent(m.renderMessages())
	}
}

func padLines(s string, targetLines int, maxWidth int) string {
	if targetLines <= 0 {
		return ""
	}
	s = strings.TrimSuffix(s, "\n")
	rawLines := strings.Split(s, "\n")
	if len(rawLines) == 1 && rawLines[0] == "" {
		rawLines = []string{}
	}
	var physicalLines []string
	for _, l := range rawLines {
		if maxWidth > 0 && lipgloss.Width(l) > maxWidth {
			l = lipgloss.NewStyle().MaxWidth(maxWidth).Render(l)
		}
		physicalLines = append(physicalLines, l)
	}
	for len(physicalLines) < targetLines {
		physicalLines = append(physicalLines, "")
	}
	if len(physicalLines) > targetLines {
		physicalLines = physicalLines[:targetLines]
	}
	return strings.Join(physicalLines, "\n")
}

// View satisfies tea.Model: it renders the current frame (sidebar,
// message viewport, and input box) from m's state — pure and
// side-effect-free, as Bubble Tea requires.
func (m model) View() string {
	if m.quitting {
		return ""
	}
	if !m.ready {
		return "Loading wa-cli...\n"
	}

	vpView := padLines(m.viewport.View(), m.viewport.Height, m.viewport.Width)

	chatView := lipgloss.JoinVertical(lipgloss.Left,
		m.renderChatHeader(),
		vpView,
		m.renderTypingIndicator(),
		m.renderInputBar(),
	)
	body := chatView
	if !m.sidebarHidden {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.renderChatList(), chatView)
	}
	if m.deleteConfirm {
		body = lipgloss.Place(m.width, lipgloss.Height(body), lipgloss.Center, lipgloss.Center, m.renderDeleteModal())
	}
	if m.forwardConfirm {
		body = lipgloss.Place(m.width, lipgloss.Height(body), lipgloss.Center, lipgloss.Center, m.renderForwardModal())
	}
	if m.documentPicker {
		body = lipgloss.Place(m.width, lipgloss.Height(body), lipgloss.Center, lipgloss.Center, m.renderDocumentPicker())
	}
	if m.renameJID != "" {
		body = lipgloss.Place(m.width, lipgloss.Height(body), lipgloss.Center, lipgloss.Center, m.renderRenameModal())
	}
	if m.helpVisible {
		body = lipgloss.Place(m.width, lipgloss.Height(body), lipgloss.Center, lipgloss.Center, m.renderHelpModal())
	}
	if m.contextMenuOpen {
		// Unlike the modals above, the context menu must float over the
		// still-visible chat instead of blanking it out — that's what
		// overlay.Composite gives us (a real splice onto the background)
		// where lipgloss.Place would just paint an empty canvas around it.
		body = overlay.Composite(m.renderContextMenu(), body, overlay.Left, overlay.Top, m.contextMenuClickX, m.contextMenuClickY)
	}
	if m.callPanel {
		body = lipgloss.Place(m.width, lipgloss.Height(body), lipgloss.Center, lipgloss.Center, m.renderCallPanel())
	}

	var footer []string
	st := m.statusLine
	if m.connectionKnown && !m.connected {
		st = m.connectionNotice
		if st == "" {
			st = "Conectando con WhatsApp. Escritura bloqueada hasta recuperar la conexión."
		}
	} else if callStatus := m.callFooter(); callStatus != "" {
		st = callStatus
	}
	if st == "" {
		st = " "
	}
	footer = append(footer, ansi.Truncate(statusStyle.Render(st), m.width, ""))
	footer = append(footer, ansi.Truncate(helpStyle.Render(
		"? atajos · s mostrar stickers · F4 llamadas · arrastra archivo para enviar · Ctrl+O fotos · Ctrl+U videos · Ctrl+F buscar · Ctrl+S enviar sticker · Ctrl+D eliminar · Ctrl+E adjuntar · Ctrl+H descargar · b barra · q salir"), m.width, ""))

	parts := []string{body}
	parts = append(parts, footer...)
	return zone.Scan(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

func (m model) renderChatHeader() string {
	chatName := m.selectedJID
	if chat, ok := m.currentChat(); ok {
		chatName = m.chatDisplayName(chat)
	}
	if chatName == "" {
		chatName = "wa-cli"
	}
	width := m.viewport.Width
	if width <= 0 {
		width = 40
	}

	right := ""
	if p, ok := m.presenceForJID(m.selectedJID); ok {
		if p.IsOnline {
			right = onlineStyle.Render("● En línea")
		} else if !p.LastSeen.IsZero() {
			right = lastSeenStyle.Render("Últ. vez " + p.LastSeen.Local().Format("15:04"))
		}
	}
	if right == "" {
		if activity := m.activityForJID(m.selectedJID); !activity.IsZero() {
			right = lastSeenStyle.Render("Últ. actividad " + activity.Local().Format("15:04"))
		}
	}
	if right == "" {
		right = lastSeenStyle.Render("Últ. vez no disponible")
	}
	botLabel := ""
	if content, err := os.ReadFile(m.botPath); err == nil && string(content) == normalizedChatJID(m.selectedJID) && m.selectedJID != "" {
		botLabel = botOnStyle.Render("BOT ON")
		right = botLabel + " · " + right
	}
	numberPart := ""
	if number := m.chatNumbers[m.selectedJID]; number != "" && number != chatName {
		numberPart = " · " + number
	}
	available := width - lipgloss.Width(right) - 1
	if available < lipgloss.Width(numberPart)+4 {
		right = botLabel
		available = width - lipgloss.Width(right) - 1
		if available < 1 {
			right = ""
			available = width
		}
	}
	if available < lipgloss.Width(numberPart)+1 {
		numberPart = ansi.Truncate(numberPart, available-1, "…")
	}
	nameWidth := available - lipgloss.Width(numberPart)
	if nameWidth < 1 {
		nameWidth = 1
	}
	left := chatListHeaderStyle.Render(ansi.Truncate(chatName, nameWidth, "…") + numberPart)
	if m.selectedJID != "" {
		left = zone.Mark(chatHeaderZoneID(m.selectedJID), left)
	}

	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	headerText := left + strings.Repeat(" ", gap) + right
	return lipgloss.NewStyle().Width(width).Border(lipgloss.NormalBorder(), false, false, true, false).BorderForeground(lipgloss.Color("240")).Render(headerText)
}

func (m model) chatDisplayName(chat chatstore.Chat) string {
	if name := chat.DisplayName(); name != "" {
		return name
	}
	if chat.IsGroup && m.groupNames[chat.JID] != "" {
		return m.groupNames[chat.JID]
	}
	if number := m.chatNumbers[chat.JID]; number != "" {
		return number
	}
	return chat.JID
}

func (m model) renderRenameModal() string {
	caption := "Nombre del chat"
	if m.renameSaving {
		caption = "Guardando nombre..."
	}
	return deleteModalStyle.Render(caption + "\n\n" + m.renameInput.View() + "\n\nEnter: guardar · Esc: cancelar · vacío: restaurar nombre original")
}

func (m model) handleOpenMedia() (tea.Model, tea.Cmd) {
	// Open only the most recent image in a normal desktop viewer. Loading all
	// image previews into the terminal is costly and makes scrolling sluggish.
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if !isPhotoMessage(msg) {
			continue
		}
		return m.openImage(msg)
	}

	if len(m.messages) == 0 {
		m.statusLine = "No hay fotos ni stickers en este chat"
	} else {
		m.statusLine = "No hay fotos ni stickers en los mensajes cargados"
	}
	return m, nil
}

func isPhotoMessage(msg msgstore.Message) bool {
	mediaType := strings.ToLower(msg.MediaType)
	return strings.Contains(mediaType, "image") || strings.Contains(mediaType, "imagen") || strings.Contains(mediaType, "foto")
}

func isVideoMessage(msg msgstore.Message) bool {
	mediaType := strings.ToLower(msg.MediaType)
	text := strings.ToLower(msg.Text)
	return strings.Contains(mediaType, "video") || strings.Contains(text, ".mp4") || strings.Contains(text, ".mkv")
}

func (m *model) moveImageSelection(delta int) bool {
	current := -1
	if m.selectedImageID != "" {
		for i, message := range m.messages {
			if message.ID == m.selectedImageID {
				current = i
				break
			}
		}
	}
	if current == -1 {
		current = len(m.messages)
		if delta > 0 {
			current = -1
		}
	}
	for {
		current += delta
		if current < 0 || current >= len(m.messages) {
			m.statusLine = "No hay más fotos en esa dirección"
			return false
		}
		message := m.messages[current]
		if message.ID == "" || !isPhotoMessage(message) {
			continue
		}
		m.selectedImageID = message.ID
		m.statusLine = "Foto seleccionada · ↑/↓ cambia · Enter abre · Esc cancela"
		m.viewport.SetContent(m.renderMessages())
		m.ensureSelectedMessageVisible(message.ID)
		return true
	}
}

func (m model) openSelectedImage() (tea.Model, tea.Cmd) {
	for _, message := range m.messages {
		if message.ID == m.selectedImageID {
			m.imageSelectMode = false
			m.selectedImageID = ""
			return m.openImage(message)
		}
	}
	m.imageSelectMode = false
	m.selectedImageID = ""
	m.statusLine = "La foto seleccionada ya no está disponible"
	return m, nil
}

func (m *model) moveVideoSelection(delta int) bool {
	current := -1
	if m.selectedVideoID != "" {
		for i, message := range m.messages {
			if message.ID == m.selectedVideoID {
				current = i
				break
			}
		}
	}
	if current == -1 {
		current = len(m.messages)
		if delta > 0 {
			current = -1
		}
	}
	for {
		current += delta
		if current < 0 || current >= len(m.messages) {
			m.statusLine = "No hay más videos en esa dirección"
			return false
		}
		message := m.messages[current]
		if message.ID == "" || !isVideoMessage(message) {
			continue
		}
		m.selectedVideoID = message.ID
		m.statusLine = "Video seleccionado · ↑/↓ cambia · Enter reproduce · d descarga · Esc cancela"
		m.viewport.SetContent(m.renderMessages())
		m.ensureSelectedMessageVisible(message.ID)
		return true
	}
}

func (m model) openSelectedVideo() (tea.Model, tea.Cmd) {
	for _, message := range m.messages {
		if message.ID == m.selectedVideoID {
			m.videoSelectMode = false
			m.selectedVideoID = ""
			return m.openVideo(message)
		}
	}
	m.videoSelectMode = false
	m.selectedVideoID = ""
	m.statusLine = "El video seleccionado ya no está disponible"
	return m, nil
}

func (m model) saveSelectedVideo() (tea.Model, tea.Cmd) {
	for _, message := range m.messages {
		if message.ID == m.selectedVideoID && isVideoMessage(message) {
			m.videoSelectMode = false
			m.selectedVideoID = ""
			m.viewport.SetContent(m.renderMessages())
			if strings.Contains(strings.ToLower(message.MediaType), "de una sola vez") {
				m.statusLine = "⚠️ Video de una sola vez: por favor ábrelo desde tu celular"
				return m, nil
			}
			m.statusLine = "Guardando video..."
			return m, saveVideoCmd(m.ctx, m.client, message, m.mediaPaths[message.ID])
		}
	}
	m.videoSelectMode = false
	m.selectedVideoID = ""
	m.statusLine = "El video seleccionado ya no está disponible"
	return m, nil
}

func (m model) openVideo(message msgstore.Message) (tea.Model, tea.Cmd) {
	if strings.Contains(strings.ToLower(message.MediaType), "de una sola vez") {
		m.statusLine = "⚠️ Video de una sola vez: por favor ábrelo desde tu celular"
		return m, nil
	}
	m.metrics.Record(metrics.VideoOpen, message.ChatJID, message.ID, message.Timestamp, boolInt(message.FromMe))
	if path := m.mediaPaths[message.ID]; path != "" {
		if _, err := os.Stat(path); err == nil {
			player := "xdg-open"
			args := []string{path}
			if _, err := exec.LookPath("mpv"); err == nil {
				player = "mpv"
				args = []string{"--autofit=50%", path}
			}
			if err := exec.Command(player, args...).Start(); err == nil {
				m.statusLine = "Reproduciendo video con " + player + ": " + path
				return m, nil
			}
		}
	}
	m.statusLine = "Descargando y reproduciendo video..."
	return m, openVideoCmd(m.ctx, m.client, message)
}

func (m model) openImage(msg msgstore.Message) (tea.Model, tea.Cmd) {
	if strings.Contains(strings.ToLower(msg.MediaType), "de una sola vez") {
		m.statusLine = "⚠️ Foto de una sola vez: ábrela desde tu celular"
		return m, nil
	}
	m.metrics.Record(metrics.ImageOpen, msg.ChatJID, msg.ID, msg.Timestamp, boolInt(msg.FromMe))
	if path := m.mediaPaths[msg.ID]; path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := openFile(path).Start(); err == nil {
				m.statusLine = "Abierta en el visor externo"
				return m, nil
			}
		}
	}
	m.statusLine = "Descargando y abriendo la foto seleccionada..."
	return m, openMediaCmd(m.ctx, m.client, msg)
}

// handleDownloadDocument opens a selector for documents in the active chat.
// Ctrl+H is global, so it works from the chat list or composer.
func (m model) handleDownloadDocument() (tea.Model, tea.Cmd) {
	m.documentSelectMode = true
	m.selectedDocumentID = ""
	m.focus = focusChatList
	m.input.Blur()
	if !m.moveDocumentSelection(-1) {
		m.documentSelectMode = false
		m.statusLine = "No hay documentos para descargar en este chat"
		return m, nil
	}
	return m, chatInactiveCmd(m.ctx, m.client, m.selectedJID)
}

func isDocumentMessage(msg msgstore.Message) bool {
	return msg.ID != "" && strings.Contains(strings.ToLower(msg.MediaType), "document")
}

func (m *model) moveDocumentSelection(delta int) bool {
	current := -1
	for i, message := range m.messages {
		if message.ID == m.selectedDocumentID {
			current = i
			break
		}
	}
	if current == -1 {
		current = len(m.messages)
		if delta > 0 {
			current = -1
		}
	}
	for current += delta; current >= 0 && current < len(m.messages); current += delta {
		message := m.messages[current]
		if !isDocumentMessage(message) {
			continue
		}
		m.selectedDocumentID = message.ID
		m.statusLine = "Documento seleccionado · ↑/↓ cambia · Enter descarga · Esc cancela"
		m.viewport.SetContent(m.renderMessages())
		m.ensureSelectedMessageVisible(message.ID)
		return true
	}
	m.statusLine = "No hay más documentos en esa dirección"
	return false
}

func (m model) downloadSelectedDocument() (tea.Model, tea.Cmd) {
	for _, message := range m.messages {
		if message.ID != m.selectedDocumentID || !isDocumentMessage(message) {
			continue
		}
		m.documentSelectMode = false
		m.selectedDocumentID = ""
		m.statusLine = "Elige la carpeta de descarga..."
		m.viewport.SetContent(m.renderMessages())
		return m, pickDownloadFolderCmd(message)
	}
	m.documentSelectMode = false
	m.selectedDocumentID = ""
	m.statusLine = "El documento seleccionado ya no está disponible"
	return m, nil
}

// pickDownloadFolderCmd opens a GTK folder chooser (defaulting to
// ~/Downloads) so the user picks where a document lands instead of it always
// going straight to ~/Downloads. Cancelling the dialog yields an empty
// folder, which the documentFolderPickedMsg handler treats as "do nothing".
func pickDownloadFolderCmd(doc msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		downloadsDir := filepath.Join(home, "Downloads")
		const script = `
import sys, gi
gi.require_version("Gtk", "3.0")
from gi.repository import Gtk

dialog = Gtk.FileChooserDialog(title="Elegir carpeta de descarga", action=Gtk.FileChooserAction.SELECT_FOLDER)
dialog.add_buttons("Cancelar", Gtk.ResponseType.CANCEL, "Elegir", Gtk.ResponseType.OK)
dialog.set_current_folder(sys.argv[1])
response = dialog.run()
if response == Gtk.ResponseType.OK:
    print(dialog.get_filename())
dialog.destroy()
`
		output, err := exec.Command("python", "-c", script, downloadsDir).Output()
		if err != nil {
			return documentFolderPickedMsg{err: err, doc: doc}
		}
		return documentFolderPickedMsg{folder: strings.TrimSpace(string(output)), doc: doc}
	}
}

func downloadDocumentToCmd(ctx context.Context, client *whatsapp.Client, msg msgstore.Message, destDir string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return mediaDownloadedMsg{err: fmt.Errorf("whatsapp client not connected")}
		}
		data, mimeType, err := client.DownloadMedia(ctx, msg)
		if err != nil {
			return mediaDownloadedMsg{err: err}
		}
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return mediaDownloadedMsg{err: fmt.Errorf("creating destination directory: %w", err)}
		}
		name := whatsapp.DocumentFileName(msg)
		if name == "" || name == "." {
			name = msg.ID
			if name == "" {
				name = fmt.Sprintf("documento-%d", msg.Timestamp)
			}
			if exts, extErr := mime.ExtensionsByType(mimeType); extErr == nil && len(exts) > 0 {
				name += exts[0]
			}
		}
		path, file, err := createUniqueDownload(destDir, name)
		if err != nil {
			return mediaDownloadedMsg{err: err}
		}
		if _, err := file.Write(data); err != nil {
			file.Close()
			os.Remove(path)
			return mediaDownloadedMsg{err: err}
		}
		if err := file.Close(); err != nil {
			return mediaDownloadedMsg{err: err}
		}
		openCmd := exec.Command("xdg-open", path)
		if err := openCmd.Start(); err != nil {
			return mediaDownloadedMsg{path: path, openErr: err}
		}
		return mediaDownloadedMsg{path: path}
	}
}

func createUniqueDownload(dir, name string) (string, *os.File, error) {
	name = filepath.Base(name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		path := filepath.Join(dir, candidate)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		return path, file, err
	}
}

func saveVideoCmd(ctx context.Context, client *whatsapp.Client, msg msgstore.Message, cachedPath string) tea.Cmd {
	return func() tea.Msg {
		var reader io.Reader
		if cachedPath != "" {
			if file, err := os.Open(cachedPath); err == nil {
				defer file.Close()
				reader = file
			}
		}
		if reader == nil {
			if client == nil {
				return videoSavedMsg{err: fmt.Errorf("whatsapp client not connected")}
			}
			data, _, err := client.DownloadMedia(ctx, msg)
			if err != nil {
				return videoSavedMsg{err: err}
			}
			reader = bytes.NewReader(data)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return videoSavedMsg{err: err}
		}
		dir := filepath.Join(home, "Downloads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return videoSavedMsg{err: err}
		}
		name := whatsapp.DocumentFileName(msg)
		if name == "" {
			at := time.UnixMilli(msg.Timestamp)
			if msg.Timestamp == 0 {
				at = time.Now()
			}
			name = "video-" + at.Local().Format("20060102-150405") + ".mp4"
		}
		path, file, err := createUniqueDownload(dir, name)
		if err != nil {
			return videoSavedMsg{err: err}
		}
		if _, err := io.Copy(file, reader); err != nil {
			file.Close()
			os.Remove(path)
			return videoSavedMsg{err: err}
		}
		if err := file.Close(); err != nil {
			return videoSavedMsg{err: err}
		}
		return videoSavedMsg{path: path}
	}
}

func (m model) handleTranscribeAudio() (tea.Model, tea.Cmd) {
	if m.transcribingID != "" {
		m.statusLine = "Ya hay una transcripción en curso; espera a que termine"
		return m, nil
	}
	var target msgstore.Message
	if m.selectedAudioID != "" {
		for _, msg := range m.messages {
			if msg.ID == m.selectedAudioID {
				target = msg
				break
			}
		}
	} else if selected, ok := m.selectedReplyMessage(); ok {
		target = selected
	} else {
		for i := len(m.messages) - 1; i >= 0; i-- {
			if isVoiceMessage(m.messages[i]) {
				target = m.messages[i]
				break
			}
		}
	}
	if target.ID == "" || !isVoiceMessage(target) {
		m.statusLine = "Selecciona una nota de voz primero (R y Ctrl+↑/↓)"
		return m, nil
	}
	if strings.Contains(strings.ToLower(target.MediaType), "de una sola vez") {
		m.statusLine = "Audio de una sola vez: ábrelo desde tu celular"
		return m, nil
	}
	if target.Transcript != "" {
		m.statusLine = "Esta nota ya tiene transcripción"
		return m, nil
	}
	m.statusLine = "Transcribiendo nota de voz con Deepgram..."
	m.transcribingID = target.ID
	return m, transcribeAudioCmd(m.ctx, m.client, target)
}

// handlePlayAudio uses the audio marked by T/Ctrl+T, falling back to the
// newest note in the conversation. mpv runs outside the TUI, so playback
// does not block keyboard handling or WhatsApp events.
func (m model) handlePlayAudio() (tea.Model, tea.Cmd) {
	var target msgstore.Message
	if m.selectedAudioID != "" {
		for _, message := range m.messages {
			if message.ID == m.selectedAudioID {
				target = message
				break
			}
		}
	}
	if target.ID == "" {
		for i := len(m.messages) - 1; i >= 0; i-- {
			if isVoiceMessage(m.messages[i]) && m.messages[i].ID != "" {
				target = m.messages[i]
				break
			}
		}
	}
	if target.ID == "" || !isVoiceMessage(target) {
		m.statusLine = "No hay una nota de voz para reproducir"
		return m, nil
	}
	if strings.Contains(strings.ToLower(target.MediaType), "de una sola vez") {
		m.statusLine = "Audio de una sola vez: ábrelo desde tu celular"
		return m, nil
	}
	if target.ID == m.playingAudioID && m.audioPlayer != nil && m.audioPlayer.Process != nil && m.audioPlayer.ProcessState == nil {
		signal := syscall.SIGSTOP
		status := "Audio pausado"
		if m.audioPaused {
			signal = syscall.SIGCONT
			status = "Reproduciendo nota de voz"
		}
		if err := m.audioPlayer.Process.Signal(signal); err != nil {
			m.statusLine = "No se pudo cambiar la reproducción: " + err.Error()
			return m, nil
		}
		m.audioPaused = !m.audioPaused
		if m.audioPaused {
			m.audioPausedAt = time.Now()
		} else if !m.audioPausedAt.IsZero() {
			m.audioPausedTotal += time.Since(m.audioPausedAt)
			m.audioPausedAt = time.Time{}
		}
		m.statusLine = status
		return m, nil
	}
	m.endAudio(false)
	stopAudioPlayer(m.audioPlayer)
	m.audioPlayer = nil
	m.playingAudioID = ""
	m.audioPaused = false
	m.audioPlaySeq++
	m.statusLine = "Preparando audio..."
	return m, playAudioCmd(m.ctx, m.client, target, m.audioPlaySeq)
}

func playAudioCmd(ctx context.Context, client *whatsapp.Client, msg msgstore.Message, seq uint64) tea.Cmd {
	return func() tea.Msg {
		result := audioPlayedMsg{msgID: msg.ID, chatJID: msg.ChatJID, message: msg, seq: seq}
		if client == nil {
			result.err = fmt.Errorf("whatsapp client not connected")
			return result
		}
		data, mime, err := client.DownloadMedia(ctx, msg)
		if err != nil {
			result.err = err
			return result
		}
		file, err := os.CreateTemp("/tmp", "wa-cli-play-*."+strings.TrimPrefix(extensionFromMime(mime, msg.MediaType), "."))
		if err != nil {
			result.err = err
			return result
		}
		path := file.Name()
		if _, err = file.Write(data); err == nil {
			err = file.Close()
		} else {
			_ = file.Close()
		}
		if err != nil {
			_ = os.Remove(path)
			result.err = err
			return result
		}
		// Explicitly disable user-configured loops and bind the player to this
		// TUI process. Without both, an mpv profile may repeat a voice note and
		// keep playing after wa-cli exits.
		player := exec.Command("mpv", "--no-video", "--force-window=no", "--loop-file=no", "--loop-playlist=no", path)
		configurePlayerProcess(player)
		if err = player.Start(); err != nil {
			_ = os.Remove(path)
			result.err = err
			return result
		}
		done := make(chan bool, 1)
		go func() { err := player.Wait(); _ = os.Remove(path); done <- err == nil }()
		result.done = done
		result.player, result.path = player, path
		return result
	}
}

func waitAudioCmd(msg audioPlayedMsg) tea.Cmd {
	return func() tea.Msg {
		completed := <-msg.done
		return audioFinishedMsg{msgID: msg.msgID, chatJID: msg.chatJID, seq: msg.seq, completed: completed}
	}
}

func stopAudioPlayer(player *exec.Cmd) {
	if player != nil && player.Process != nil {
		_ = player.Process.Kill()
	}
}

func markAudioPlayedCmd(ms *msgstore.Store, chatJID, msgID string) tea.Cmd {
	return func() tea.Msg {
		if ms != nil {
			_ = ms.MarkPlayed(chatJID, msgID)
		}
		return nil
	}
}

func markPlayedReceiptCmd(ctx context.Context, client *whatsapp.Client, ms *msgstore.Store, msg msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		result := playedReceiptResultMsg{chatJID: msg.ChatJID, msgID: msg.ID}
		if client == nil {
			result.err = fmt.Errorf("whatsapp client not connected")
			return result
		}
		chatJID, err := types.ParseJID(msg.ChatJID)
		if err != nil {
			result.err = err
			return result
		}
		senderJID, err := types.ParseJID(msg.SenderJID)
		if err != nil {
			result.err = err
			return result
		}
		result.err = client.MarkPlayed(ctx, chatJID, msg.ID, senderJID)
		if result.err == nil && ms != nil {
			result.err = ms.MarkPlayedReceiptSent(msg.ChatJID, msg.ID)
		}
		return result
	}
}

func isVoiceMessage(msg msgstore.Message) bool {
	media := strings.ToLower(msg.MediaType)
	return strings.Contains(media, "audio") || strings.Contains(media, "voice") || strings.Contains(media, "voz")
}

func whisperPaths() (string, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	waDataDir := filepath.Join(home, ".local", "share", "wa-cli")
	whisperCandidates := []string{
		filepath.Join(waDataDir, "whisper.cpp", "build", "bin", "whisper-cli"),
		filepath.Join(waDataDir, "whisper.cpp", "build", "bin", "main"),
	}
	modelPatterns := []string{
		filepath.Join(waDataDir, "models", "ggml-large-v3-turbo*.bin"),
		filepath.Join(waDataDir, "models", "ggml-large-v3*.bin"),
	}
	whisper := ""
	for _, candidate := range whisperCandidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			whisper = candidate
			break
		}
	}
	modelPath := ""
	for _, pattern := range modelPatterns {
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			modelPath = matches[0]
			break
		}
	}
	return whisper, modelPath
}

// transcribeAudioCmd sends the voice note straight to Deepgram (same key as
// Jarvis/bot) instead of running Whisper locally.
func transcribeAudioCmd(ctx context.Context, client *whatsapp.Client, msg msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return transcriptionResultMsg{err: fmt.Errorf("whatsapp client not connected")}
		}
		transcribeCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		data, contentType, err := client.DownloadMedia(transcribeCtx, msg)
		if err != nil {
			return transcriptionResultMsg{err: err}
		}
		if len(data) == 0 {
			return transcriptionResultMsg{err: fmt.Errorf("audio vacío")}
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return transcriptionResultMsg{err: fmt.Errorf("localizar carpeta del usuario: %w", err)}
		}
		key, err := os.ReadFile(filepath.Join(home, ".config", "jarvis", "deepgram.key"))
		if err != nil {
			return transcriptionResultMsg{err: fmt.Errorf("leer clave de Deepgram: %w", err)}
		}
		transcript, err := transcribeDeepgram(transcribeCtx, data, contentType, strings.TrimSpace(string(key)), deepgramListenURL, http.DefaultClient)
		if err != nil {
			return transcriptionResultMsg{err: err}
		}
		msg.Transcript = transcript
		return transcriptionResultMsg{msg: msg}
	}
}

func saveTranscriptCmd(ms *msgstore.Store, msg msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		if ms == nil {
			return backgroundResultMsg{}
		}
		return backgroundResultMsg{err: ms.Append(msg)}
	}
}

func openVideoCmd(ctx context.Context, client *whatsapp.Client, msg msgstore.Message) tea.Cmd {
	return func() tea.Msg {
		data, mime, err := client.DownloadMedia(ctx, msg)
		if err != nil {
			return mediaOpenedMsg{msgID: msg.ID, err: err}
		}
		ext := extensionFromMime(mime, msg.MediaType)
		if ext == ".bin" {
			ext = ".mp4"
		}
		dir := os.TempDir()
		tmpPath := filepath.Join(dir, fmt.Sprintf("wa-cli-video-%s%s", msg.ID, ext))
		if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
			return mediaOpenedMsg{msgID: msg.ID, err: err}
		}

		var cmd *exec.Cmd
		if _, err := exec.LookPath("mpv"); err == nil {
			cmd = exec.Command("mpv", "--autofit=50%", tmpPath)
		} else {
			cmd = exec.Command("xdg-open", tmpPath)
		}
		if err := cmd.Start(); err != nil {
			return mediaOpenedMsg{msgID: msg.ID, err: err}
		}
		return mediaOpenedMsg{msgID: msg.ID, path: tmpPath, fallbackInfo: "reproduciendo video con mpv"}
	}
}

func (m model) handleOpenVideo() (tea.Model, tea.Cmd) {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if isVideoMessage(msg) {
			return m.openVideo(msg)
		}
	}
	m.statusLine = "No hay mensajes de video en este chat"
	return m, nil
}

var urlRegex = regexp.MustCompile(`https?://[^\s<>"'\)]+`)

func (m model) handleOpenLink() (tea.Model, tea.Cmd) {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		matches := urlRegex.FindAllString(msg.Text, -1)
		if len(matches) > 0 {
			link := matches[len(matches)-1]
			cmd := exec.Command("xdg-open", link)
			if err := cmd.Start(); err != nil {
				m.statusLine = "Error al abrir enlace: " + err.Error()
				return m, nil
			}
			m.statusLine = "Abriendo enlace en navegador: " + link
			return m, nil
		}
	}
	m.statusLine = "No hay enlaces (http/https) en este chat"
	return m, nil
}

func padOrTrimLines(s string, targetLines int, width int) string {
	if targetLines <= 0 {
		return ""
	}
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	var physical []string
	for _, l := range lines {
		if width > 0 && lipgloss.Width(l) > width {
			l = lipgloss.NewStyle().MaxWidth(width).Render(l)
		}
		physical = append(physical, l)
	}
	if len(physical) > targetLines {
		physical = physical[:targetLines]
	}
	for len(physical) < targetLines {
		physical = append(physical, "")
	}
	return strings.Join(physical, "\n")
}

func (m model) renderChatList() string {
	var b strings.Builder
	b.WriteString(chatListHeaderStyle.Render("Chats") + "\n\n")

	if len(m.chats) == 0 {
		b.WriteString(chatItemStyle.Render("(none yet)"))
	}

	bodyHeight := m.height - 4
	if bodyHeight < 5 {
		bodyHeight = 5
	}
	visibleRows := bodyHeight - 4 // the header and borders
	if visibleRows < 1 {
		visibleRows = 1
	}
	selectedJID := m.selectedJID
	if m.forwardDestinationMode && m.forwardDestinationJID != "" {
		selectedJID = m.forwardDestinationJID
	}
	selected := 0
	for i, c := range m.chats {
		if c.JID == selectedJID {
			selected = i
			break
		}
	}
	start := selected - visibleRows/2
	if start < 0 {
		start = 0
	}
	end := start + visibleRows
	if end > len(m.chats) {
		end = len(m.chats)
		start = end - visibleRows
		if start < 0 {
			start = 0
		}
	}

	maxLabelLen := sidebarWidth - 6
	for _, c := range m.chats[start:end] {
		name := m.chatDisplayName(c)
		unread := ""
		if c.UnreadCount > 0 {
			unread = fmt.Sprintf(" (%d)", c.UnreadCount)
		}
		nameWidth := maxLabelLen - lipgloss.Width(unread)
		if nameWidth < 1 {
			nameWidth = 1
		}
		label := ansi.Truncate(name, nameWidth, "…") + unread
		if lipgloss.Width(label) > maxLabelLen {
			label = ansi.Truncate(label, maxLabelLen, "…")
		}
		var line string
		switch {
		case m.forwardDestinationMode && c.JID == m.forwardDestinationJID:
			line = selectedChatStyle.Render("⇢ " + label)
		case c.JID == m.selectedJID:
			line = selectedChatStyle.Render("› " + label)
		case c.UnreadCount > 0:
			line = "  " + unreadStyle.Render(label)
		default:
			line = chatItemStyle.Render("  " + label)
		}
		b.WriteString(zone.Mark(chatNameZoneID(c.JID), line) + "\n")
	}

	inner := padOrTrimLines(b.String(), bodyHeight-2, sidebarWidth-4)
	return sidebarStyle.Width(sidebarWidth).Height(bodyHeight).Render(inner)
}

func (m model) sameContact(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	jA, errA := types.ParseJID(a)
	jB, errB := types.ParseJID(b)
	if errA != nil || errB != nil {
		return a == b
	}
	jA = jA.ToNonAD()
	jB = jB.ToNonAD()
	if jA == jB {
		return true
	}
	if m.client != nil {
		return m.client.SameContact(m.ctx, jA, jB)
	}
	return false
}

func (m model) presenceForJID(chatJID string) (whatsapp.PresenceState, bool) {
	var targetPN string
	if m.client != nil {
		if j, err := types.ParseJID(chatJID); err == nil {
			pn := m.client.ResolvePNJID(m.ctx, j)
			if !pn.IsEmpty() {
				targetPN = pn.String()
			}
		}
	}

	if m.presence != nil {
		if st, ok := m.presence[chatJID]; ok {
			return st, true
		}
		if targetPN != "" {
			if st, ok := m.presence[targetPN]; ok {
				return st, true
			}
		}
		for jid, st := range m.presence {
			if jid == chatJID || (targetPN != "" && (jid == targetPN || strings.Split(jid, "@")[0] == strings.Split(targetPN, "@")[0])) || m.sameContact(jid, chatJID) {
				return st, true
			}
		}
	}

	return whatsapp.PresenceState{}, false
}

func latestIncomingActivity(messages []msgstore.Message) time.Time {
	var latest time.Time
	for _, message := range messages {
		if message.FromMe || message.Timestamp <= 0 {
			continue
		}
		activity := time.UnixMilli(message.Timestamp)
		if activity.After(latest) {
			latest = activity
		}
	}
	return latest
}

func (m model) activityForJID(chatJID string) time.Time {
	if activity := m.lastActivity[chatJID]; !activity.IsZero() {
		return activity
	}
	for jid, activity := range m.lastActivity {
		if m.sameContact(jid, chatJID) {
			return activity
		}
	}
	return time.Time{}
}

// getQuotedInfo also returns quotedID, the local ID of the original message
// being quoted when it's resolvable — used to jump to and center it on
// click. It's empty when only the quoted text/sender is known (e.g. an
// optimistic reply whose target has since scrolled out of m.messages, or a
// quote whose stanzaID isn't in the currently loaded history).
func (m model) getQuotedInfo(msg msgstore.Message) (quotedText string, senderName string, quotedID string, ok bool) {
	// A just-sent reply is rendered before the server assigns its real ID and
	// before the local store is reloaded. Keep its selected target in memory so
	// the quote is visible immediately instead of only after reopening the chat.
	if quoted, exists := m.replyQuotes[msg.ID]; exists {
		senderName = "me"
		if !quoted.FromMe {
			senderName = m.senderDisplayName(quoted)
		}
		quotedText = quotedPreview(quoted)
		return quotedText, senderName, quoted.ID, true
	}

	qText, participant, stanzaID, audioSeconds, hasQuote := whatsapp.ExtractQuotedContext(msg.RawProto)
	if !hasQuote {
		return "", "", "", false
	}

	senderName = m.formatParticipantName(participant)
	if audioSeconds > 0 {
		qText = "Audio · " + formatAudioDuration(audioSeconds)
	}

	if stanzaID != "" {
		for _, prev := range m.messages {
			if prev.ID == stanzaID {
				if prev.FromMe {
					senderName = "me"
				} else {
					senderName = m.senderDisplayName(prev)
				}
				if seconds := audioDurationSeconds(prev); seconds > 0 {
					qText = "Audio · " + formatAudioDuration(seconds)
				} else if qText == "" {
					qText = quotedPreview(prev)
				}
				return qText, senderName, stanzaID, true
			}
		}
	}

	if qText != "" {
		return qText, senderName, stanzaID, true
	}
	return "", "", "", false
}

func quotedPreview(msg msgstore.Message) string {
	if seconds := audioDurationSeconds(msg); seconds > 0 {
		return "Audio · " + formatAudioDuration(seconds)
	}
	if msg.Text != "" {
		return msg.Text
	}
	if msg.MediaType != "" {
		return "[" + msg.MediaType + "]"
	}
	return ""
}

func (m model) formatParticipantName(participant string) string {
	if participant == "" {
		return "them"
	}
	if name, ok := m.senderNames[participant]; ok && name != "" {
		return name
	}
	if chat, ok := m.currentChat(); ok && !chat.IsGroup && chat.DisplayName() != "" {
		return chat.DisplayName()
	}
	if at := strings.Index(participant, "@"); at > 0 {
		return participant[:at]
	}
	return participant
}

func (m model) renderMessages() string {
	if m.selectedJID == "" {
		return "No chats yet. They'll appear here once synced (try 'wa chat list' in another terminal) or as messages arrive."
	}
	if len(m.messages) == 0 {
		return "No local history for this chat yet."
	}

	width := m.viewport.Width
	if width <= 0 {
		width = 40
	}

	var b strings.Builder
	var lastDate time.Time
	for index, msg := range m.messages {
		if msg.Timestamp > 0 {
			msgDate := time.UnixMilli(msg.Timestamp).Local()
			if lastDate.IsZero() || !sameDay(lastDate, msgDate) {
				sep := dateSeparatorStyle.Render(formatDateSeparator(msgDate))
				b.WriteString(lipgloss.NewStyle().Width(width).Align(lipgloss.Center).Render(sep) + "\n\n")
			}
			lastDate = msgDate
		}

		var msgBlock strings.Builder

		hasInlinePreview := false
		if previewStr, ok := m.mediaPreviews[msg.ID]; ok && previewStr != "" && !m.mediaHidden[msg.ID] {
			hasInlinePreview = true
		}

		text := msg.Text
		isDocument := strings.Contains(strings.ToLower(msg.MediaType), "document")
		documentName := ""
		if isDocument {
			documentName = documentDisplayName(msg)
		}
		if documentName != "" {
			text = "📄 " + documentName
			if msg.Text != "" {
				text += "\n" + msg.Text
			}
		} else if !hasInlinePreview {
			switch {
			case msg.MediaType != "" && text != "":
				text = fmt.Sprintf("[%s] %s", msg.MediaType, text)
			case msg.MediaType != "":
				text = fmt.Sprintf("[%s]", msg.MediaType)
			}
		}
		if seconds := audioDurationSeconds(msg); seconds > 0 {
			text += " · " + formatAudioDuration(seconds)
		}

		qText, qSender, quotedID, hasQuote := m.getQuotedInfo(msg)
		if hasQuote && len(qText) > 40 {
			qText = qText[:40] + "…"
		}

		ts := timestampStyle.Render(time.UnixMilli(msg.Timestamp).Local().Format("15:04"))
		playedMark := ""
		if isVoiceMessage(msg) && msg.Played {
			playedMark = playedAudioStyle.Render("●") + " "
		}

		var line string
		var align lipgloss.Position
		if msg.FromMe {
			if hasQuote {
				qHeader := quoteStyle.Render(fmt.Sprintf("│ %s: %s", qSender, qText))
				rendered := lipgloss.NewStyle().Width(width).Align(lipgloss.Right).Render(qHeader)
				if quotedID != "" {
					rendered = zone.Mark(quoteZoneID(msg.ID), rendered)
				}
				msgBlock.WriteString(rendered + "\n")
			}
			var checkMark string
			switch msg.Status {
			case "read":
				checkMark = readReceiptStyle.Render(formatReadReceipt(msg.ReadAt, time.Now()))
			case "delivered":
				checkMark = timestampStyle.Render("✓✓")
			default:
				checkMark = timestampStyle.Render("✓")
			}
			line = fmt.Sprintf("%s%s %s %s", playedMark, myMsgStyle.Render(text), ts, checkMark)
			align = lipgloss.Right
		} else {
			name := senderNameStyle.Render(m.senderDisplayName(msg))
			if hasQuote {
				qHeader := quoteStyle.Render(fmt.Sprintf("│ %s: %s", qSender, qText))
				rendered := lipgloss.NewStyle().Width(width).Align(lipgloss.Left).Render(qHeader)
				if quotedID != "" {
					rendered = zone.Mark(quoteZoneID(msg.ID), rendered)
				}
				msgBlock.WriteString(rendered + "\n")
			}
			line = fmt.Sprintf("%s%s %s: %s", playedMark, ts, name, theirMsgStyle.Render(text))
			align = lipgloss.Left
		}
		if isDocument {
			// Width wraps the caption inside the card; MaxWidth would cut the
			// text and the right border. Lipgloss Width counts padding (2)
			// but not the border (2), and markers like ↳ need ~4 columns.
			bubbleWidth := min(lipgloss.Width(line)+2, max(10, width-8))
			line = documentBubbleStyle.Width(bubbleWidth).Render(line)
		}
		if msg.ID != "" && msg.ID == m.replyToID {
			line = replySelectedStyle.Render("↳ ") + line
		}
		// Render node: audio selection is independent from replyToID, so the
		// marker never changes which message a text reply will quote.
		if m.audioSelectMode && msg.ID != "" && msg.ID == m.selectedAudioID {
			line = replySelectedStyle.Render("➤ ") + line
		}
		if m.searchMatchIndex == index {
			line = replySelectedStyle.Render("🔍 ") + line
		}
		if m.deleteSelectMode && msg.ID != "" && msg.ID == m.selectedDeleteID {
			line = replySelectedStyle.Render("↳ ") + line
		}
		if msg.ID != "" && msg.ID == m.highlightedMessageID {
			line = jumpHighlightStyle.Render("┃ ") + line
		}
		if m.copySelectMode && msg.ID != "" && msg.ID == m.selectedCopyID {
			line = replySelectedStyle.Render("⧉ ") + line
		}
		if m.forwardSelectMode && msg.ID != "" && msg.ID == m.selectedForwardID {
			line = replySelectedStyle.Render("⇢ ") + line
		}
		if m.imageSelectMode && msg.ID != "" && msg.ID == m.selectedImageID {
			line = replySelectedStyle.Render("▣ ") + line
		}
		if m.videoSelectMode && msg.ID != "" && msg.ID == m.selectedVideoID {
			line = replySelectedStyle.Render("▶ ") + line
		}
		if m.documentSelectMode && msg.ID != "" && msg.ID == m.selectedDocumentID {
			line = replySelectedStyle.Render("▤ ") + line
		}
		if m.multiSelectMode && msg.ID != "" {
			if m.multiSelectIDs[msg.ID] {
				line = replySelectedStyle.Render("☑ ") + line
			} else {
				line = timestampStyle.Render("☐ ") + line
			}
		}

		if previewStr, ok := m.mediaPreviews[msg.ID]; ok && previewStr != "" && !m.mediaHidden[msg.ID] {
			previewLines := strings.Split(previewStr, "\n")
			for _, pLine := range previewLines {
				pWidth := lipgloss.Width(pLine)
				if align == lipgloss.Right && width > pWidth {
					msgBlock.WriteString(strings.Repeat(" ", width-pWidth) + pLine + "\n")
				} else {
					msgBlock.WriteString(pLine + "\n")
				}
			}
		}
		if isStickerMessage(msg) {
			if key := stickerKey(msg); m.stickerReveal[key] {
				if imageID := m.stickerImages[key]; imageID != 0 {
					msgBlock.WriteString(stickerPreviewLine(imageID, width, align == lipgloss.Right) + "\n")
				}
			}
		}

		msgBlock.WriteString(lipgloss.NewStyle().Width(width).Align(align).Render(line) + "\n")
		if msg.Transcript != "" {
			label := transcriptLabelStyle.Render("Transcripción")
			msgBlock.WriteString(lipgloss.NewStyle().Width(width).Align(align).Render(label) + "\n")
			for _, segment := range strings.Split(strings.TrimSpace(msg.Transcript), "\n") {
				segment = strings.TrimSpace(segment)
				if segment == "" {
					continue
				}
				transcriptLine := quoteStyle.Render("↳ " + segment)
				msgBlock.WriteString(lipgloss.NewStyle().Width(width).Align(align).Render(transcriptLine) + "\n")
			}
		}

		// The closing zone marker must land inside this message's own last
		// line, not at column 0 of the next line — otherwise EndX ends up
		// smaller than StartX (an inverted box) and InBounds never matches.
		blockContent := strings.TrimSuffix(msgBlock.String(), "\n")
		if msg.ID != "" {
			b.WriteString(zone.Mark(msgZoneID(msg.ID), blockContent))
		} else {
			b.WriteString(blockContent)
		}
		b.WriteString("\n")
	}

	if !strings.HasSuffix(m.selectedJID, "@g.us") && possibleBlockWarning(m.messages, time.Now()) {
		warning := "⚠ Posible bloqueo: tu último mensaje lleva más de 2 min con un solo check. También puede estar sin conexión."
		b.WriteString("\n" + possibleBlockStyle.Width(width).Render(warning) + "\n")
	}
	return b.String()
}

func audioDurationSeconds(msg msgstore.Message) uint32 {
	if !isVoiceMessage(msg) || msg.RawProto == "" {
		return 0
	}
	data, err := base64.StdEncoding.DecodeString(msg.RawProto)
	if err != nil {
		return 0
	}
	var stored waProto.Message
	if err := proto.Unmarshal(data, &stored); err != nil || stored.GetAudioMessage() == nil {
		return 0
	}
	return stored.GetAudioMessage().GetSeconds()
}

func documentDisplayName(msg msgstore.Message) string {
	name := whatsapp.DocumentFileName(msg)
	if name == "" || filepath.Ext(name) != "" || msg.RawProto == "" {
		return name
	}
	raw, err := base64.StdEncoding.DecodeString(msg.RawProto)
	if err != nil {
		return name
	}
	var stored waProto.Message
	if err := proto.Unmarshal(raw, &stored); err != nil || stored.GetDocumentMessage() == nil {
		return name
	}
	if exts, err := mime.ExtensionsByType(stored.GetDocumentMessage().GetMimetype()); err == nil && len(exts) > 0 {
		return name + exts[0]
	}
	return name
}

func formatAudioDuration(seconds uint32) string {
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

// possibleBlockWarning detects the only useful signal WhatsApp exposes to
// senders: a server-accepted message that remains undelivered. This is a
// heuristic, not proof of a block, because an offline phone looks identical.
func possibleBlockWarning(messages []msgstore.Message, now time.Time) bool {
	if len(messages) == 0 {
		return false
	}

	last := messages[len(messages)-1]
	if !last.FromMe || last.ID == "" || last.Status == "delivered" || last.Status == "read" || last.Timestamp <= 0 {
		return false
	}

	return now.Sub(time.UnixMilli(last.Timestamp)) >= possibleBlockDelay
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func formatDateSeparator(d time.Time) string {
	now := time.Now().Local()
	switch {
	case sameDay(d, now):
		return "── Hoy ──"
	case sameDay(d, now.AddDate(0, 0, -1)):
		return "── Ayer ──"
	default:
		return "── " + d.Format("02/01/2006") + " ──"
	}
}

func formatReadReceipt(readAt int64, now time.Time) string {
	if readAt <= 0 {
		return "[api] view"
	}
	readTime := time.UnixMilli(readAt)
	elapsed := now.Sub(readTime)
	if elapsed < time.Minute {
		return "[api] view now"
	}
	if elapsed < time.Hour {
		return fmt.Sprintf("[api] view %dm", int(elapsed.Minutes()))
	}
	return "[api] view " + readTime.Local().Format("15:04")
}

// senderDisplayName resolves a message's sender to a real name instead
// of a generic "them" label. For a 1:1 chat, the chat's own name IS the
// sender's name. For a group (multiple possible senders), it falls back
// to the local contacts lookup, then the sender JID's phone number.
func (m model) senderDisplayName(msg msgstore.Message) string {
	if chat, ok := m.currentChat(); ok {
		if !chat.IsGroup && chat.DisplayName() != "" {
			return chat.DisplayName()
		}
	}
	if name, ok := m.senderNames[msg.SenderJID]; ok && name != "" {
		return name
	}
	if at := strings.Index(msg.SenderJID, "@"); at > 0 {
		return msg.SenderJID[:at]
	}
	return "Unknown"
}

func (m model) currentChat() (chatstore.Chat, bool) {
	for _, c := range m.chats {
		if c.JID == m.selectedJID {
			return c, true
		}
	}
	return chatstore.Chat{}, false
}

func (m model) renderInputBar() string {
	if m.connectionKnown && !m.connected {
		return inputBoxStyle.Width(m.viewport.Width).Render("Sin conexión · escritura bloqueada · q para salir")
	}
	if m.searching {
		return inputBoxFocusedStyle.Width(m.viewport.Width).Render(m.searchInput.View())
	}
	style := inputBoxStyle
	if m.focus == focusInput {
		style = inputBoxFocusedStyle
	}
	input := style.Width(m.viewport.Width).Render(m.input.View())
	if banner := m.renderDeleteBanner(); banner != "" {
		return lipgloss.JoinVertical(lipgloss.Left, banner, input)
	}
	if banner := m.renderForwardBanner(); banner != "" {
		return lipgloss.JoinVertical(lipgloss.Left, banner, input)
	}
	if banner := m.renderReplyBanner(); banner != "" {
		return lipgloss.JoinVertical(lipgloss.Left, banner, input)
	}
	return input
}

// renderTypingIndicator lives next to the composer rather than replacing the
// header's presence status. A composing notification is intentionally brief:
// WhatsApp may omit the matching paused event.
func (m model) renderTypingIndicator() string {
	p, ok := m.presenceForJID(m.selectedJID)
	if !ok || !p.IsTyping || !time.Now().Before(p.TypingUntil) {
		return ""
	}
	return onlineStyle.Render("… escribiendo")
}

func (m model) renderDeleteBanner() string {
	message, ok := m.selectedDeleteMessage()
	if !m.deleteSelectMode || !ok {
		return ""
	}
	preview := message.Text
	if preview == "" && message.MediaType != "" {
		preview = "[" + message.MediaType + "]"
	}
	if preview == "" {
		preview = "mensaje"
	}
	maxWidth := m.viewport.Width - 6
	if maxWidth > 0 && lipgloss.Width(preview) > maxWidth {
		preview = preview[:maxWidth-1] + "…"
	}
	return replyBannerStyle.Width(m.viewport.Width).Render("🗑 Eliminar para todos: " + preview)
}

func (m model) renderReplyBanner() string {
	quoted, ok := m.selectedReplyMessage()
	if !ok {
		return ""
	}
	name := "tú"
	if !quoted.FromMe {
		name = m.senderDisplayName(quoted)
	}
	preview := quotedPreview(quoted)
	if preview == "" {
		preview = "mensaje"
	}
	maxWidth := m.viewport.Width - 6
	if maxWidth > 0 && lipgloss.Width(preview) > maxWidth {
		preview = preview[:maxWidth-1] + "…"
	}
	return replyBannerStyle.Width(m.viewport.Width).Render("↪ Respondiendo a " + name + ": " + preview)
}

func (m model) renderForwardBanner() string {
	if m.forwardDestinationMode {
		return replyBannerStyle.Width(m.viewport.Width).Render("⇢ Reenviar a: " + m.forwardDestinationName())
	}
	message, ok := m.selectedForwardMessage()
	if !m.forwardSelectMode || !ok {
		return ""
	}
	preview := message.Text
	if preview == "" && message.MediaType != "" {
		preview = "[" + message.MediaType + "]"
	}
	if preview == "" {
		preview = "mensaje"
	}
	maxWidth := m.viewport.Width - 13
	if maxWidth > 0 && lipgloss.Width(preview) > maxWidth {
		preview = preview[:maxWidth-1] + "…"
	}
	return replyBannerStyle.Width(m.viewport.Width).Render("⇢ Reenviar: " + preview)
}

func (m model) renderForwardModal() string {
	choice := "[No]   Sí"
	if m.forwardConfirmed {
		choice = "No   [Sí]"
	}
	return deleteModalStyle.Render("⇢ ¿Reenviar este mensaje a " + m.forwardDestinationName() + "?\n\n" + choice + "\n\n←/→ elegir · Enter confirmar · Esc cancelar")
}

func (m model) renderDeleteModal() string {
	choice := "[No]   Sí"
	if m.deleteConfirmed {
		choice = "No   [Sí]"
	}
	return deleteModalStyle.Render("🗑 ¿Eliminar este mensaje para todos?\n\n" + choice + "\n\n←/→ elegir · Enter confirmar · Esc cancelar")
}

func (m model) renderContextMenu() string {
	var lines []string
	for i, item := range m.contextMenuItems {
		style := contextMenuItemStyle
		prefix := "  "
		if i == m.contextMenuCursor {
			style = contextMenuItemSelectedStyle
			prefix = "➤ "
		}
		lines = append(lines, zone.Mark(ctxItemZoneID(i), style.Render(prefix+item.label)))
	}
	lines = append(lines, "", helpStyle.Render("↑/↓ · Enter · Esc"))
	return contextMenuStyle.Render(strings.Join(lines, "\n"))
}

func (m model) renderHelpModal() string {
	rows := [][2]string{
		{"?", "Mostrar u ocultar esta ayuda"},
		{"F4 / F5 / F6", "Panel / contestar / colgar; m silencia y v activa video en el panel"},
		{"↑/↓ · j/k", "Navegar chats; en selectores, mover marca"},
		{"Enter", "Activar chat, enviar o confirmar acción"},
		{"Tab / Esc", "Caja de texto / volver o cancelar"},
		{"Ctrl+Space", "Mantener o pausar presencia y vistos al salir de la pestaña"},
		{"R", "Responder al último mensaje"},
		{"f", "Elegir un mensaje y reenviarlo a otro chat"},
		{"Ctrl+↑ / Ctrl+↓", "Cambiar el mensaje que responderás"},
		{"c", "Elegir un mensaje de texto y copiarlo al portapapeles"},
		{"Ctrl+O", "Elegir una foto y abrirla en el visor externo"},
		{"Ctrl+U", "Elegir video; Enter reproduce, d descarga a ~/Downloads"},
		{"Ctrl+D", "Elegir uno de tus mensajes y eliminar para todos"},
		{"Ctrl+F", "Buscar en todo el historial guardado; ↑/↓ recorre coincidencias"},
		{"Ctrl+S", "Cargar y enviar un sticker WebP"},
		{"s", "Mostrar los stickers visibles en pantalla (desde la lista de chats)"},
		{"Ctrl+E", "Adjuntar uno o más documentos (selector gráfico)"},
		{"Ctrl+H", "Elegir y descargar un documento"},
		{"Ctrl+V", "Enviar imagen del portapapeles"},
		{"Arrastrar archivo", "Soltar imagen, video o documento para enviarlo al chat activo"},
		{"T / Ctrl+T", "Elegir nota de voz para transcribir"},
		{"t", "Transcribir la nota de voz más reciente"},
		{"p", "Elegir nota de voz para escuchar; Enter la reproduce"},
		{"r / Ctrl+R", "Grabar y enviar nota de voz"},
		{"Mantener Espacio", "Dictar con vista previa; al soltar se corrige el borrador, Esc cancela"},
		{"o / u / l", "Abrir última foto / video / enlace"},
		{"PgUp / PgDn", "Recorrer historial; PgUp carga 100 mensajes anteriores"},
		{"b", "Ocultar o mostrar la barra de chats"},
		{"q / Ctrl+C", "Salir"},
	}
	const keyWidth = 18
	var lines []string
	for _, row := range rows {
		lines = append(lines, fmt.Sprintf("%-*s %s", keyWidth, row[0], row[1]))
	}
	title := lipgloss.NewStyle().Width(76).Align(lipgloss.Center).Bold(true).Render("⌨ Atajos de wa-cli")
	footer := lipgloss.NewStyle().Width(76).Align(lipgloss.Center).Render("Esc, Enter o ? para cerrar")
	return deleteModalStyle.Copy().Width(76).Align(lipgloss.Left).Render(title + "\n\n" + strings.Join(lines, "\n") + "\n\n" + footer)
}

func (m model) renderDocumentPicker() string {
	const maxEntries = 13
	start := m.documentCursor - maxEntries/2
	if start < 0 {
		start = 0
	}
	end := start + maxEntries
	if end > len(m.documentEntries) {
		end = len(m.documentEntries)
		start = end - maxEntries
		if start < 0 {
			start = 0
		}
	}
	var lines []string
	for i := start; i < end; i++ {
		entry := m.documentEntries[i]
		path := filepath.Join(m.documentDir, entry.Name())
		prefix := "[ ]"
		if entry.IsDir() {
			prefix = "📁"
		} else if m.selectedDocuments[path] {
			prefix = "[✓]"
		}
		line := prefix + " " + entry.Name()
		if i == m.documentCursor {
			line = replySelectedStyle.Render("› " + line)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, "(carpeta vacía)")
	}
	dir := m.documentDir
	if lipgloss.Width(dir) > 48 {
		dir = "…" + dir[len(dir)-47:]
	}
	header := fmt.Sprintf("📎 Adjuntar documentos (%d seleccionados)\n%s", lenSelectedDocuments(m.selectedDocuments), dir)
	footer := "↑/↓ mover · Espacio marcar · Enter abrir/enviar · ← subir · Esc cancelar"
	return deleteModalStyle.Width(62).Render(header + "\n\n" + strings.Join(lines, "\n") + "\n\n" + footer)
}

func lenSelectedDocuments(selected map[string]bool) int {
	count := 0
	for _, marked := range selected {
		if marked {
			count++
		}
	}
	return count
}

type sendImageMsg struct {
	chatJID string
	err     error
}

type sendStickerMsg struct {
	chatJID string
	err     error
}

func sendStickerCmd(ctx context.Context, client *whatsapp.Client, guard *safety.Guard, chatJID, path string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return sendStickerMsg{chatJID: chatJID, err: fmt.Errorf("whatsapp client not connected")}
		}
		if strings.ToLower(filepath.Ext(path)) != ".webp" {
			return sendStickerMsg{chatJID: chatJID, err: fmt.Errorf("el sticker debe ser un archivo WebP")}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return sendStickerMsg{chatJID: chatJID, err: err}
		}
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			return sendStickerMsg{chatJID: chatJID, err: err}
		}
		if _, err = client.SendSticker(ctx, jid, data, "image/webp"); err != nil {
			return sendStickerMsg{chatJID: chatJID, err: err}
		}
		if guard != nil {
			_ = guard.MarkKnown(chatJID)
		}
		return sendStickerMsg{chatJID: chatJID}
	}
}

func sendImageCmd(ctx context.Context, client *whatsapp.Client, guard *safety.Guard, chatJID string, data []byte, mime, caption string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return sendImageMsg{chatJID: chatJID, err: fmt.Errorf("whatsapp client not connected")}
		}
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			return sendImageMsg{chatJID: chatJID, err: err}
		}
		_, err = client.SendImage(ctx, jid, data, mime, caption)
		if err != nil {
			return sendImageMsg{chatJID: chatJID, err: err}
		}
		if guard != nil {
			_ = guard.MarkKnown(chatJID)
		}
		return sendImageMsg{chatJID: chatJID, err: nil}
	}
}

func (m *model) handleSendImageFromClipboardOrPath() (tea.Model, tea.Cmd) {
	if m.selectedJID == "" {
		m.statusLine = "Select a chat first"
		return m, nil
	}
	inputText := strings.TrimSpace(m.input.Value())
	data, mime, err := readImageFromPath(inputText)
	caption := ""
	if err != nil {
		data, mime, err = readClipboardImage()
		if err != nil {
			m.statusLine = "No image in clipboard or invalid file path"
			return m, nil
		}
		caption = inputText
	}

	m.abandonCompose()
	m.input.SetValue("")
	m.statusLine = "Sending image..."

	m.messages = append(m.messages, msgstore.Message{
		ChatJID:   m.selectedJID,
		Text:      caption,
		MediaType: "image",
		FromMe:    true,
		Timestamp: time.Now().UnixMilli(),
	})
	m.viewport.SetContent(m.renderMessages())
	m.viewport.GotoBottom()

	return m, tea.Batch(
		sendImageCmd(m.ctx, m.client, m.guard, m.selectedJID, data, mime, caption),
		markVisibleMessagesReadCmd(m.ctx, m.cs, m.client, m.selectedJID, m.messages),
		chatActivityCmd(m.ctx, m.client, m.selectedJID, false),
	)
}

func readClipboardImage() ([]byte, string, error) {
	cmd := exec.Command("wl-paste", "--type", "image/png")
	out, err := cmd.Output()
	if err == nil && len(out) > 0 {
		return out, "image/png", nil
	}

	cmd = exec.Command("wl-paste", "--type", "image/jpeg")
	out, err = cmd.Output()
	if err == nil && len(out) > 0 {
		return out, "image/jpeg", nil
	}

	cmd = exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	out, err = cmd.Output()
	if err == nil && len(out) > 0 {
		return out, "image/png", nil
	}

	cmd = exec.Command("xclip", "-selection", "clipboard", "-t", "image/jpeg", "-o")
	out, err = cmd.Output()
	if err == nil && len(out) > 0 {
		return out, "image/jpeg", nil
	}

	return nil, "", fmt.Errorf("no image found in clipboard")
}

func readImageFromPath(pathStr string) ([]byte, string, error) {
	cleanPath := strings.TrimSpace(pathStr)
	cleanPath = strings.Trim(cleanPath, `"'`)
	cleanPath = strings.TrimPrefix(cleanPath, "file://")
	if cleanPath == "" {
		return nil, "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(cleanPath, "~/") {
		home, _ := os.UserHomeDir()
		cleanPath = filepath.Join(home, cleanPath[2:])
	}
	info, err := os.Stat(cleanPath)
	if err != nil || info.IsDir() {
		return nil, "", fmt.Errorf("not a file")
	}
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, "", err
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		ext := strings.ToLower(filepath.Ext(cleanPath))
		switch ext {
		case ".png":
			mime = "image/png"
		case ".jpg", ".jpeg":
			mime = "image/jpeg"
		case ".webp":
			mime = "image/webp"
		case ".gif":
			mime = "image/gif"
		default:
			return nil, "", fmt.Errorf("file is not an image (%s)", mime)
		}
	}
	return data, mime, nil
}

type sendAudioMsg struct {
	chatJID string
	err     error
}

func sendAudioCmd(ctx context.Context, client *whatsapp.Client, guard *safety.Guard, chatJID string, data []byte, mime string, voice bool, seconds uint32) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return sendAudioMsg{chatJID: chatJID, err: fmt.Errorf("whatsapp client not connected")}
		}
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			return sendAudioMsg{chatJID: chatJID, err: err}
		}
		_, err = client.SendAudio(ctx, jid, data, mime, voice, seconds)
		if err != nil {
			return sendAudioMsg{chatJID: chatJID, err: err}
		}
		if guard != nil {
			_ = guard.MarkKnown(chatJID)
		}
		return sendAudioMsg{chatJID: chatJID, err: nil}
	}
}

func (m *model) startAudioRecording() {
	if m.calls != nil && m.calls.Snapshot().Busy {
		m.statusLine = "Termina la llamada antes de grabar una nota de voz"
		return
	}
	if m.selectedJID == "" {
		m.statusLine = "Select a chat first"
		return
	}
	audioPath := filepath.Join(os.TempDir(), fmt.Sprintf("wa-cli-voice-%d.ogg", time.Now().UnixNano()))
	recorderArgs := []string{"-y", "-f", "pulse", "-i", "default"}
	if runtime.GOOS == "darwin" {
		// macOS supplies audio input through AVFoundation. The first input
		// device is used; grant Terminal microphone permission when prompted.
		recorderArgs = []string{"-y", "-f", "avfoundation", "-i", ":0"}
	}
	recorderArgs = append(recorderArgs, "-ac", "1", "-ar", "48000", "-c:a", "libopus", audioPath)
	cmd := exec.Command("ffmpeg", recorderArgs...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		m.statusLine = "Error starting audio recording: " + err.Error()
		return
	}
	m.isRecordingAudio = true
	m.recordingCmd = cmd
	m.recordingStderr = stderr
	m.audioFilePath = audioPath
	m.recordingStartTime = time.Now()
	m.statusLine = "🔴 GRABANDO NOTA DE VOZ... (presiona Enter o Ctrl+R para enviar, Esc para cancelar)"
}

func (m *model) stopAndSendAudioRecording() (tea.Model, tea.Cmd) {
	if !m.isRecordingAudio {
		return m, nil
	}
	m.isRecordingAudio = false
	recDuration := time.Since(m.recordingStartTime)
	seconds := uint32(recDuration.Seconds())
	if seconds == 0 {
		seconds = 1
	}

	processErr := finishAudioRecording(m.recordingCmd, 3*time.Second)

	path := m.audioFilePath
	data, err := os.ReadFile(path)
	_ = os.Remove(path)

	if err != nil || len(data) == 0 {
		detail := lastProcessError(m.recordingStderr)
		if detail == "" && processErr != nil {
			detail = processErr.Error()
		}
		if detail == "" {
			detail = "the recorder produced an empty file"
		}
		m.statusLine = "Audio recording failed: " + detail
		return m, nil
	}

	// Keep a copy of what was sent so a failing voice note can be inspected.
	_ = os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("wa-cli-sent-voice-%d.ogg", time.Now().Unix())), data, 0o600)

	m.statusLine = fmt.Sprintf("Sending voice note (%ds)...", seconds)
	m.messages = append(m.messages, msgstore.Message{
		ChatJID:   m.selectedJID,
		Text:      "",
		MediaType: "voice note",
		FromMe:    true,
		Timestamp: time.Now().UnixMilli(),
	})
	m.viewport.SetContent(m.renderMessages())
	m.viewport.GotoBottom()

	return m, tea.Batch(
		sendAudioCmd(m.ctx, m.client, m.guard, m.selectedJID, data, "audio/ogg; codecs=opus", true, seconds),
		markVisibleMessagesReadCmd(m.ctx, m.cs, m.client, m.selectedJID, m.messages),
		chatActivityCmd(m.ctx, m.client, m.selectedJID, false),
	)
}

func finishAudioRecording(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("recorder process was not started")
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return <-done
	}
}

func lastProcessError(stderr *bytes.Buffer) string {
	if stderr == nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		if line == "" || strings.HasPrefix(line, "size=") || strings.HasPrefix(line, "Exiting normally") {
			continue
		}
		if len(line) > 160 {
			line = line[:160] + "…"
		}
		return line
	}
	return ""
}

func (m *model) cancelAudioRecording() {
	if !m.isRecordingAudio {
		return
	}
	m.isRecordingAudio = false
	if m.recordingCmd != nil && m.recordingCmd.Process != nil {
		_ = m.recordingCmd.Process.Kill()
		_ = m.recordingCmd.Wait()
	}
	if m.audioFilePath != "" {
		_ = os.Remove(m.audioFilePath)
	}
	m.statusLine = "Audio recording cancelled"
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
func (m *model) noteInput() {
	if m.idle {
		m.metrics.Record(metrics.IdleEnd, "", "", 0, 0)
		m.idle = false
	}
	m.lastInput = time.Now()
}
func (m *model) recordChatOpen() {
	for _, c := range m.chats {
		if c.JID == m.selectedJID {
			m.metrics.Record(metrics.ChatOpen, c.JID, "", int64(c.UnreadCount), 0)
			return
		}
	}
}
func (m *model) composeDuration() int64 {
	if m.composeAt.IsZero() {
		return 0
	}
	return time.Since(m.composeAt).Milliseconds()
}
func (m *model) resetCompose() { m.composeAt = time.Time{}; m.composeKeys = 0; m.composeBackspaces = 0 }
func (m *model) abandonCompose() {
	if !m.composeAt.IsZero() && strings.TrimSpace(m.input.Value()) != "" {
		m.metrics.Record(metrics.ComposeAbandon, m.selectedJID, "", m.composeDuration(), int64(len([]rune(m.input.Value()))))
	}
	m.resetCompose()
}
func (m *model) recordDictation(value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	if m.composeAt.IsZero() {
		m.composeAt = time.Now()
		m.metrics.Record(metrics.ComposeStart, m.selectedJID, "", 0, 0)
	}
	m.metrics.Record(metrics.DictationUsed, m.selectedJID, "", 0, int64(len([]rune(value))))
}
func (m *model) endAudio(completed bool) {
	if m.playingAudioID == "" || m.audioStarted.IsZero() {
		return
	}
	paused := m.audioPausedTotal
	if !m.audioPausedAt.IsZero() {
		paused += time.Since(m.audioPausedAt)
	}
	listened := time.Since(m.audioStarted) - paused
	if listened < 0 {
		listened = 0
	}
	m.metrics.Record(metrics.AudioPlayEnd, m.audioChatJID, m.playingAudioID, listened.Milliseconds(), boolInt(completed))
	m.audioStarted = time.Time{}
	m.audioPausedAt = time.Time{}
	m.audioPausedTotal = 0
}

func (m *model) noteTranscripts(messages []msgstore.Message) {
	if m.transcriptSeen == nil {
		m.transcriptSeen = map[string]bool{}
	}
	for _, msg := range messages {
		if msg.Transcript == "" || msg.ID == "" {
			continue
		}
		key := msg.ChatJID + "\x00" + msg.ID
		if m.transcriptSeen[key] {
			continue
		}
		m.transcriptSeen[key] = true
		m.metrics.Record(metrics.TranscriptShown, msg.ChatJID, msg.ID, 0, 0)
	}
}

func mediaChat(result, selected string) string {
	if result != "" {
		return result
	}
	return selected
}
