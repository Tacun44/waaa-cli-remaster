// Package whatsapp wraps github.com/polymorfa/hypermeow with a smaller,
// wa-cli-shaped API (Client.ListChats, SendImage, GroupInfo, and so on)
// rather than leaking whatsmeow's types everywhere. This is the one
// package that talks to WhatsApp's servers; everything under cmd/ goes
// through it rather than importing whatsmeow directly.
//
// Client wires together three separate local stores (see
// internal/store, internal/chatstore, internal/msgstore) plus optional
// desktop notifications (internal/notify) — see ARCHITECTURE.md for how
// they fit together and why they're kept separate.
package whatsapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waProto "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/store/sqlstore"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	waLog "github.com/polymorfa/hypermeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/notify"
	"github.com/codebyoketch/wa-cli/internal/qr"
	"github.com/codebyoketch/wa-cli/internal/safety"
)

// Client is wa-cli's WhatsApp client: a thin, higher-level wrapper
// around a *whatsmeow.Client (exported as WA) plus the local
// chatstore/msgstore state and notification settings needed to serve
// wa-cli's commands and TUI. Construct one with New.
type Client struct {
	WA                 *whatsmeow.Client
	log                waLog.Logger
	chats              *chatstore.Store
	msgs               *msgstore.Store
	onIncoming         func(msgstore.Message)
	onPresence         func(PresenceState)
	onReceipt          func(chatJID string, msgIDs []string, status string, readAt time.Time)
	onConnectionChange func(connected bool, reason string)

	pendingPresence   map[string]types.JID
	pendingPresenceMu sync.Mutex

	// Desktop notifications are opt-in (default off) — only wa watch
	// and the TUI call SetNotifications to enable them. Without this,
	// any brief background sync that also passes chats/msgs to New()
	// (e.g. `wa chat list`, `wa chat send`) would start popping
	// notifications too, which isn't what those commands are for.
	notifyEnabled     bool
	notifyGroups      bool
	notifyShowPreview bool
}

// PresenceState represents a contact's online / last seen status.
type PresenceState struct {
	JID         string
	IsOnline    bool
	IsTyping    bool
	TypingKnown bool
	TypingUntil time.Time
	LastSeen    time.Time
}

// OnPresence registers fn to be called when presence updates arrive.
func (c *Client) OnPresence(fn func(PresenceState)) {
	c.onPresence = fn
}

// OnReceipt registers fn to be called when read/delivered receipts arrive.
func (c *Client) OnReceipt(fn func(chatJID string, msgIDs []string, status string, readAt time.Time)) {
	c.onReceipt = fn
}

// MarkRead sends read receipts to WhatsApp servers for the given message IDs in a chat.
func (c *Client) MarkRead(ctx context.Context, chatJID types.JID, messageIDs []string, senderJID types.JID) error {
	if len(messageIDs) == 0 {
		return nil
	}
	if c.WA == nil || !c.WA.IsConnected() {
		return fmt.Errorf("cannot mark messages read while WhatsApp is disconnected")
	}
	ids := make([]types.MessageID, len(messageIDs))
	for i, id := range messageIDs {
		ids[i] = types.MessageID(id)
	}
	if senderJID.IsEmpty() {
		senderJID = chatJID
	}
	return c.WA.MarkRead(ctx, ids, time.Now(), chatJID.ToNonAD(), senderJID.ToNonAD())
}

// MarkPlayed tells the sender that an incoming voice note started playing.
func (c *Client) MarkPlayed(ctx context.Context, chatJID types.JID, messageID string, senderJID types.JID) error {
	if messageID == "" {
		return nil
	}
	if c.WA == nil || !c.WA.IsConnected() {
		return fmt.Errorf("cannot mark voice note played while WhatsApp is disconnected")
	}
	if senderJID.IsEmpty() {
		senderJID = chatJID
	}
	return c.WA.MarkRead(ctx, []types.MessageID{types.MessageID(messageID)}, time.Now(), chatJID.ToNonAD(), senderJID.ToNonAD(), types.ReceiptTypePlayed)
}

// AnnounceAvailable tells WhatsApp this linked client is actively in use.
func (c *Client) AnnounceAvailable(ctx context.Context) error {
	if c.WA == nil || !c.WA.IsConnected() {
		return nil
	}
	return c.WA.SendPresence(ctx, types.PresenceAvailable)
}

// AnnounceUnavailable tells WhatsApp this linked client is no longer being
// actively used, without disconnecting the websocket.
func (c *Client) AnnounceUnavailable(ctx context.Context) error {
	if c.WA == nil || !c.WA.IsConnected() {
		return nil
	}
	return c.WA.SendPresence(ctx, types.PresenceUnavailable)
}

// SetChatPresence publishes typing/paused activity for one conversation.
func (c *Client) SetChatPresence(ctx context.Context, chatJID types.JID, composing bool) error {
	if c.WA == nil || !c.WA.IsConnected() {
		return nil
	}
	state := types.ChatPresencePaused
	if composing {
		state = types.ChatPresenceComposing
	}
	return c.WA.SendChatPresence(ctx, chatJID.ToNonAD(), state, types.ChatPresenceMediaText)
}

// DeactivateChat stops restoring this chat's presence subscription after a
// reconnect, clears typing state and announces the client as unavailable.
func (c *Client) DeactivateChat(ctx context.Context, chatJID types.JID) error {
	target := chatJID.ToNonAD()
	if pnJID := c.ResolvePNJID(ctx, chatJID); !pnJID.IsEmpty() && pnJID.Server == types.DefaultUserServer {
		target = pnJID.ToNonAD()
	}
	c.pendingPresenceMu.Lock()
	delete(c.pendingPresence, target.String())
	c.pendingPresenceMu.Unlock()
	_ = c.SetChatPresence(ctx, chatJID, false)
	return c.AnnounceUnavailable(ctx)
}

// ResolvePNJID returns the phone number (@s.whatsapp.net) JID corresponding to jid if available.
// VerifiedPhoneNumber only uses a phone JID or WhatsApp's stored alias. It
// avoids name matching, since different contacts can share a name.
func (c *Client) VerifiedPhoneNumber(ctx context.Context, jid types.JID) string {
	if jid.Server == types.DefaultUserServer {
		return "+" + jid.User
	}
	if c == nil || c.WA == nil || c.WA.Store == nil {
		return ""
	}
	alt, err := c.WA.Store.GetAltJID(ctx, jid)
	if err == nil && !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
		return "+" + alt.User
	}
	return ""
}

// ResolvePNJID returns the phone number (@s.whatsapp.net) JID corresponding to jid if available.
func (c *Client) ResolvePNJID(ctx context.Context, jid types.JID) types.JID {
	if jid.Server == "s.whatsapp.net" {
		return jid
	}
	if c.WA == nil || c.WA.Store == nil {
		return jid
	}
	if alt, err := c.WA.Store.GetAltJID(ctx, jid); err == nil && !alt.IsEmpty() && alt.Server == "s.whatsapp.net" {
		return alt
	}
	contacts, err := c.WA.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return jid
	}
	targetName := ""
	if info, ok := contacts[jid]; ok {
		targetName = bestContactName(info)
	}
	if targetName == "" && c.chats != nil {
		if chat, found, _ := c.chats.Get(jid.String()); found {
			targetName = chat.Name
		}
	}
	if targetName != "" {
		tLower := strings.ToLower(targetName)
		// 1. Try exact match
		for cJID, info := range contacts {
			if cJID.Server == "s.whatsapp.net" {
				cName := strings.ToLower(bestContactName(info))
				if cName != "" && cName == tLower {
					return cJID
				}
			}
		}
		// 2. Try prefix/contains match (e.g. Korben vs Korbencillo)
		for cJID, info := range contacts {
			if cJID.Server == "s.whatsapp.net" {
				cName := strings.ToLower(bestContactName(info))
				if cName != "" && (strings.HasPrefix(tLower, cName) || strings.HasPrefix(cName, tLower) || strings.Contains(tLower, cName) || strings.Contains(cName, tLower)) {
					return cJID
				}
			}
		}
	}
	return jid
}

// SameContact reports whether two JIDs are proven aliases for the same
// contact. Unlike ResolvePNJID, it deliberately avoids display-name
// heuristics: names are not unique and must never route messages between
// conversations.
func (c *Client) SameContact(ctx context.Context, a, b types.JID) bool {
	a = a.ToNonAD()
	b = b.ToNonAD()
	if a == b {
		return true
	}
	if c == nil || c.WA == nil || c.WA.Store == nil {
		return false
	}

	canonical := func(jid types.JID) types.JID {
		if jid.Server == types.DefaultUserServer {
			return jid
		}
		alt, err := c.WA.Store.GetAltJID(ctx, jid)
		if err == nil && !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
			return alt.ToNonAD()
		}
		return jid
	}
	return canonical(a) == canonical(b)
}

// SubscribePresence requests presence updates for jid. WhatsApp's presence
// endpoint expects the canonical phone-number JID, so LIDs are resolved before
// subscribing. Subscribing to both forms can produce conflicting updates for
// the same person.
func (c *Client) SubscribePresence(ctx context.Context, jid types.JID) error {
	if c.WA == nil {
		return nil
	}
	target := jid.ToNonAD()
	if pnJID := c.ResolvePNJID(ctx, jid); !pnJID.IsEmpty() && pnJID.Server == types.DefaultUserServer {
		target = pnJID.ToNonAD()
	}

	c.pendingPresenceMu.Lock()
	if c.pendingPresence == nil {
		c.pendingPresence = make(map[string]types.JID)
	}
	c.pendingPresence[target.String()] = target
	c.pendingPresenceMu.Unlock()

	if c.WA.IsConnected() {
		if err := c.WA.SendPresence(ctx, types.PresenceAvailable); err != nil {
			return fmt.Errorf("announcing online presence: %w", err)
		}
		if err := c.WA.SubscribePresence(ctx, target); err != nil {
			return fmt.Errorf("subscribing to presence: %w", err)
		}
	}
	return nil
}

func (c *Client) resubscribePendingPresence() {
	if c.WA == nil || !c.WA.IsConnected() {
		return
	}
	ctx := context.Background()
	if err := c.WA.SendPresence(ctx, types.PresenceAvailable); err != nil {
		c.log.Warnf("failed to announce presence after reconnect: %v", err)
		return
	}
	c.pendingPresenceMu.Lock()
	defer c.pendingPresenceMu.Unlock()
	for _, jid := range c.pendingPresence {
		if err := c.WA.SubscribePresence(ctx, jid); err != nil {
			c.log.Warnf("failed to restore presence subscription: %v", err)
		}
	}
}

const fallbackPushName = "wa-cli"

// New builds a Client using the first (or a fresh, unpaired) device from
// container. chats/msgs may be nil for commands that don't need chat or
// message history (e.g. login/logout/status).
func New(ctx context.Context, container *sqlstore.Container, log waLog.Logger, chats *chatstore.Store, msgs *msgstore.Store) (*Client, error) {
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, waerrors.Wrap(err, "loading device store")
	}
	// SendPresence refuses to announce availability when PushName is empty.
	// Some linked-device sessions (especially ones affected by an incomplete
	// app-state sync) are persisted without it, which silently prevents all
	// subscribed contact presence from arriving. Give those sessions a stable
	// local fallback and persist it when the device is already paired.
	if strings.TrimSpace(device.PushName) == "" {
		device.PushName = fallbackPushName
		if device.ID != nil {
			if err := device.Save(ctx); err != nil {
				return nil, waerrors.Wrap(err, "saving fallback push name")
			}
		}
	}
	clientLog := newQuietLogger(waLog.Stdout("Client", "WARN", true))
	waClient := whatsmeow.NewClient(device, clientLog)
	c := &Client{WA: waClient, log: log, chats: chats, msgs: msgs}

	if chats != nil || msgs != nil {
		waClient.AddEventHandler(c.handleEvent)
	}

	return c, nil
}

// OnIncomingMessage registers fn to be called for every incoming message,
// after it's been ingested into chatstore/msgstore. Lets callers with
// their own event loop (like the TUI) bridge live updates in without
// duplicating ingestion logic. Only one callback is supported; a second
// call replaces the first.
func (c *Client) OnIncomingMessage(fn func(msgstore.Message)) {
	c.onIncoming = fn
}

// OnConnectionChange reports live connection loss and restoration to the TUI.
func (c *Client) OnConnectionChange(fn func(connected bool, reason string)) {
	c.onConnectionChange = fn
}

// SetNotifications turns on desktop notifications for incoming messages.
// Off by default — see the Client struct's notify* field comments for
// why this needs to be explicit rather than inferred from chats/msgs
// being present.
func (c *Client) SetNotifications(enabled, groups, showPreview bool) {
	c.notifyEnabled = enabled
	c.notifyGroups = groups
	c.notifyShowPreview = showPreview
}

func (c *Client) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		c.resubscribePendingPresence()
		if c.onConnectionChange != nil {
			c.onConnectionChange(true, "")
		}
	case *events.Disconnected:
		if c.onConnectionChange != nil {
			c.onConnectionChange(false, "Sin conexión con WhatsApp. Esperando reconexión; pulsa q para reiniciar wa-cli.")
		}
	case events.PermanentDisconnect:
		if c.onConnectionChange != nil {
			c.onConnectionChange(false, "Conexión cerrada por WhatsApp ("+v.PermanentDisconnectDescription()+"). Pulsa q y reinicia wa-cli.")
		}
	case *events.HistorySync:
		if c.chats != nil {
			c.ingestHistorySync(v)
		}
	case *events.Message:
		c.ingestMessage(v)
	case *events.Presence:
		c.ingestPresence(v)
	case *events.ChatPresence:
		c.ingestChatPresence(v)
	case *events.Receipt:
		c.ingestReceipt(v)
	}
}

func (c *Client) ingestChatPresence(evt *events.ChatPresence) {
	if c.onPresence == nil || evt == nil || evt.Chat.IsEmpty() {
		return
	}
	// A paused event is the explicit end of typing. WhatsApp emits composing
	// while the other participant is entering a text message.
	typing := evt.State == types.ChatPresenceComposing
	st := PresenceState{
		JID:         evt.Chat.ToNonAD().String(),
		IsTyping:    typing,
		TypingKnown: true,
	}
	if typing {
		// WhatsApp does not reliably send the paused event, so callers must
		// treat a composing notification as short-lived.
		st.TypingUntil = time.Now().Add(7 * time.Second)
	}
	c.onPresence(st)
}

func (c *Client) ingestReceipt(evt *events.Receipt) {
	if evt == nil || len(evt.MessageIDs) == 0 {
		return
	}
	status := "delivered"
	switch evt.Type {
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf, types.ReceiptTypePlayed, types.ReceiptTypePlayedSelf:
		status = "read"
	case types.ReceiptTypeDelivered, types.ReceiptTypeSender:
		status = "delivered"
	default:
		return
	}
	chatJID := evt.Chat.String()
	readAt := evt.Timestamp
	if readAt.IsZero() {
		readAt = time.Now()
	}

	ids := make([]string, len(evt.MessageIDs))
	for i, id := range evt.MessageIDs {
		ids[i] = string(id)
	}

	if c.msgs != nil {
		pnJID := c.ResolvePNJID(context.Background(), evt.Chat)
		_ = c.msgs.UpdateStatus([]string{chatJID, pnJID.String()}, ids, status, readAt.UnixMilli())
	}

	if c.onReceipt != nil {
		c.onReceipt(chatJID, ids, status, readAt)
	}
}

func (c *Client) ingestPresence(evt *events.Presence) {
	if c.onPresence == nil {
		return
	}
	jidStr := evt.From.ToNonAD().String()
	lastSeen := evt.LastSeen
	st := PresenceState{
		JID:      jidStr,
		IsOnline: !evt.Unavailable,
		LastSeen: lastSeen,
	}
	c.onPresence(st)

	if c.WA != nil && c.WA.Store != nil {
		ctx := context.Background()
		if alt, err := c.WA.Store.GetAltJID(ctx, evt.From); err == nil && !alt.IsEmpty() {
			st.JID = alt.ToNonAD().String()
			c.onPresence(st)
		}
		if contacts, err := c.WA.Store.Contacts.GetAllContacts(ctx); err == nil {
			if info, ok := contacts[evt.From.ToNonAD()]; ok {
				name := bestContactName(info)
				if name != "" {
					for cJID, cInfo := range contacts {
						if cJID.Server == "lid" && strings.EqualFold(bestContactName(cInfo), name) {
							st.JID = cJID.String()
							c.onPresence(st)
						}
					}
				}
			}
		}
	}
}

// ingestHistorySync populates chatstore from WhatsApp's initial (or
// on-demand) history sync payload, sent shortly after a fresh pairing or
// reconnect.
func (c *Client) ingestHistorySync(evt *events.HistorySync) {
	if evt.Data == nil {
		return
	}
	for _, conv := range evt.Data.GetConversations() {
		jid := conv.GetID()
		if jid == "" || isPseudoChat(jid) {
			continue
		}

		name := conv.GetName()
		msgs := conv.GetMessages()

		// Skip pure noise: no name and no message history at all. These
		// show up as bare @lid conversation stubs (e.g. group members
		// using WhatsApp's privacy-ID feature) that aren't real chats
		// worth surfacing in `wa chat list`.
		if name == "" && len(msgs) == 0 {
			continue
		}

		var lastAt int64
		var preview string
		if len(msgs) > 0 {
			last := msgs[len(msgs)-1].GetMessage()
			if ts := last.GetMessageTimestamp(); ts > 0 {
				lastAt = int64(ts) * 1000
			}
			preview = extractPreview(last.GetMessage())
		}

		err := c.chats.Upsert(chatstore.Chat{
			JID:                jid,
			Name:               name,
			IsGroup:            strings.HasSuffix(jid, "@g.us"),
			LastMessageAt:      lastAt,
			LastMessagePreview: preview,
			UnreadCount:        int(conv.GetUnreadCount()),
		})
		if err != nil {
			c.log.Warnf("chatstore upsert failed for %s: %v", jid, err)
		}
	}
}

// statusBroadcastJID is WhatsApp's pseudo-chat for Status/story updates.
const statusBroadcastJID = "status@broadcast"

// newsletterSuffix marks WhatsApp Channel subscriptions (e.g.
// "120363...@newsletter"), unlike status@broadcast these don't have one
// fixed JID, so they're matched by suffix instead of exact equality.
const newsletterSuffix = "@newsletter"

// isPseudoChat reports whether jid is a non-conversation pseudo-chat
// (Status broadcasts, Channel/newsletter subscriptions) that shouldn't be
// printed or tracked as a real chat.
func isPseudoChat(jid string) bool {
	return jid == statusBroadcastJID || strings.HasSuffix(jid, newsletterSuffix)
}

// ingestMessage keeps chatstore and msgstore current as new messages
// arrive/are sent after the initial sync (this is what wa watch leans on
// most heavily; wired in here too so a chat you're mid-conversation with
// during `wa chat list`/`wa chat open` still looks current).
func (c *Client) ingestMessage(evt *events.Message) {
	jid := evt.Info.Chat.String()
	if isPseudoChat(jid) {
		return
	}

	// Anti-Delete / Anti-Revoke: If the sender revokes (deletes for everyone) a message,
	// do NOT delete the message from msgstore! Retain the original message and tag it.
	if !evt.Info.IsFromMe && evt.Message != nil {
		if pm := evt.Message.GetProtocolMessage(); pm != nil && pm.GetType() == waProto.ProtocolMessage_REVOKE {
			targetID := string(pm.GetKey().GetID())
			if targetID != "" && c.msgs != nil {
				existing, ok, _ := c.msgs.Get(jid, targetID)
				if ok {
					if !strings.Contains(existing.Text, "🚫 [eliminado por el remitente]") {
						if existing.Text != "" {
							existing.Text = existing.Text + " 🚫 [eliminado por el remitente]"
						} else {
							existing.Text = "🚫 [eliminado por el remitente]"
						}
					}
					_ = c.msgs.Append(existing)
					if c.onIncoming != nil {
						c.onIncoming(existing)
					}
					c.log.Infof("Anti-delete captured revoked message %s from %s", targetID, jid)
					return
				}
			}
		}
	}

	mediaType, text := classifyMessage(evt.Message)
	preview := text
	switch {
	case mediaType != "" && text != "":
		preview = fmt.Sprintf("[%s] %s", mediaType, text)
	case mediaType != "":
		preview = fmt.Sprintf("[%s]", mediaType)
	}

	if c.chats != nil {
		update := chatstore.Chat{
			JID:                jid,
			IsGroup:            evt.Info.IsGroup,
			LastMessageAt:      evt.Info.Timestamp.UnixMilli(),
			LastMessagePreview: preview,
		}
		// PushName is whoever actually sent this specific message —
		// correct as the chat's name for a 1:1 chat, but only when it's
		// the *other* person's message. Your own outgoing messages also
		// fire this event with PushName set to your own profile name,
		// which would otherwise flip the stored chat name back and
		// forth between yours and theirs depending on who sent last.
		// Also wrong for groups (would overwrite the group's real name
		// with whichever member happened to send most recently).
		if !evt.Info.IsGroup && !evt.Info.IsFromMe {
			update.Name = evt.Info.PushName
		}
		err := c.chats.Upsert(update)
		if err != nil {
			c.log.Warnf("chatstore upsert failed for %s: %v", jid, err)
		}

		if !evt.Info.IsFromMe {
			if err := c.chats.IncrementUnread(jid); err != nil {
				c.log.Warnf("chatstore unread increment failed for %s: %v", jid, err)
			}
		}
	}

	if c.msgs != nil {
		sender := evt.Info.Sender.String()
		if evt.Info.IsFromMe {
			sender = "me"
		}

		var raw string
		if evt.Message != nil {
			if b, err := proto.Marshal(evt.Message); err == nil {
				raw = base64.StdEncoding.EncodeToString(b)
			}
		}

		stored := msgstore.Message{
			ID:        evt.Info.ID,
			ChatJID:   jid,
			SenderJID: sender,
			Timestamp: evt.Info.Timestamp.UnixMilli(),
			Text:      text,
			FromMe:    evt.Info.IsFromMe,
			MediaType: mediaType,
			RawProto:  raw,
		}

		if err := c.msgs.Append(stored); err != nil {
			c.log.Warnf("msgstore append failed for %s: %v", jid, err)
		} else if c.onIncoming != nil {
			c.onIncoming(stored)
		}
	}

	if !evt.Info.IsFromMe && c.notifyEnabled {
		shouldNotify := c.notifyGroups || !evt.Info.IsGroup
		if shouldNotify && c.chats != nil {
			if chat, ok, err := c.chats.Get(jid); err == nil && ok && chat.Muted {
				shouldNotify = false
			}
		}
		if shouldNotify {
			title := evt.Info.PushName
			if title == "" {
				title = jid
			}
			body := "New message"
			if c.notifyShowPreview {
				switch {
				case mediaType != "" && text != "":
					body = fmt.Sprintf("[%s] %s", mediaType, text)
				case mediaType != "":
					body = fmt.Sprintf("[%s]", mediaType)
				case text != "":
					body = text
				}
			}
			notify.SendAsync(title, body, func(err error) {
				c.log.Warnf("desktop notification failed: %v", err)
			})
		}
	}
}

// extractPreview trims plain conversational text for display. Kept for
// HistorySync ingestion, which only has a bare GetConversation()-shaped
// value at the point it's called (see classifyMessage for the fuller,
// type-aware version used everywhere else).
func extractPreview(msg interface{ GetConversation() string }) string {
	if msg == nil {
		return ""
	}
	text := msg.GetConversation()
	if len(text) > 80 {
		text = text[:80] + "…"
	}
	return text
}

// classifyMessage identifies a message's type (empty string for plain
// text) and extracts its text content — the conversation/extended-text
// body for text messages, or the caption (if any) for media messages.
func classifyMessage(msg *waProto.Message) (mediaType, text string) {
	if msg == nil {
		return "", ""
	}

	if vo := msg.GetViewOnceMessage(); vo != nil && vo.GetMessage() != nil {
		return classifyViewOnce(vo.GetMessage())
	}
	if vo := msg.GetViewOnceMessageV2(); vo != nil && vo.GetMessage() != nil {
		return classifyViewOnce(vo.GetMessage())
	}
	if vo := msg.GetViewOnceMessageV2Extension(); vo != nil && vo.GetMessage() != nil {
		return classifyViewOnce(vo.GetMessage())
	}

	switch {
	case msg.GetConversation() != "":
		return "", msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		return "", msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage() != nil:
		if msg.GetImageMessage().GetViewOnce() {
			return "foto de una sola vez (abrir en celular)", msg.GetImageMessage().GetCaption()
		}
		return "image", msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		if msg.GetVideoMessage().GetViewOnce() {
			return "video de una sola vez (abrir en celular)", msg.GetVideoMessage().GetCaption()
		}
		return "video", msg.GetVideoMessage().GetCaption()
	case msg.GetAudioMessage() != nil:
		if msg.GetAudioMessage().GetViewOnce() {
			return "audio de una sola vez (abrir en celular)", ""
		}
		if msg.GetAudioMessage().GetPTT() {
			return "voice note", ""
		}
		return "audio", ""
	case msg.GetDocumentMessage() != nil:
		return "document", msg.GetDocumentMessage().GetCaption()
	case msg.GetStickerMessage() != nil:
		return "sticker", ""
	default:
		return "", ""
	}
}

func classifyViewOnce(inner *waProto.Message) (mediaType, text string) {
	kind, caption := classifyMessage(inner)
	if kind == "" {
		kind = "foto"
	}
	return fmt.Sprintf("%s de una sola vez (abrir en celular)", kind), caption
}

// SyncChats connects, waits up to timeout for chat data to arrive via
// history sync / messages, then disconnects. Intended for one-shot
// commands like `wa chat list` that need reasonably current data without
// a long-running connection (that's what `wa watch`, Phase 5, is for).
func (c *Client) SyncChats(ctx context.Context, timeout time.Duration) error {
	if !c.IsLoggedIn() {
		return waerrors.ErrNotLoggedIn
	}
	if c.chats == nil {
		return waerrors.New("SyncChats called without a chatstore")
	}

	if err := c.WA.Connect(); err != nil {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}
	defer c.WA.Disconnect()

	select {
	case <-time.After(timeout):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// IsLoggedIn reports whether the underlying device store already has a
// persisted session, without opening a network connection.
func (c *Client) IsLoggedIn() bool {
	return c.WA.Store.ID != nil
}

// Contact is a simplified view of a WhatsApp contact.
type Contact struct {
	JID          string `json:"jid"`
	Name         string `json:"name,omitempty"` // best available: FullName > FirstName > PushName > BusinessName
	PushName     string `json:"pushName,omitempty"`
	BusinessName string `json:"businessName,omitempty"`
}

// ListContacts returns all locally known contacts, sorted by name. This
// reads directly from the local device store — no network connection
// needed, since WhatsApp syncs your contact list to the device during
// login and keeps it updated from there.
func (c *Client) ListContacts(ctx context.Context) ([]Contact, error) {
	contacts, err := c.WA.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, waerrors.Wrap(err, "loading contacts")
	}

	out := make([]Contact, 0, len(contacts))
	for jid, info := range contacts {
		out = append(out, Contact{
			JID:          jid.String(),
			Name:         bestContactName(info),
			PushName:     info.PushName,
			BusinessName: info.BusinessName,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func bestContactName(info types.ContactInfo) string {
	switch {
	case info.FullName != "":
		return info.FullName
	case info.FirstName != "":
		return info.FirstName
	case info.PushName != "":
		return info.PushName
	case info.BusinessName != "":
		return info.BusinessName
	default:
		return ""
	}
}

// Group is a simplified view of a WhatsApp group.
type Group struct {
	JID              string `json:"jid"`
	Name             string `json:"name"`
	Topic            string `json:"topic,omitempty"`
	ParticipantCount int    `json:"participantCount"`
}

// Participant is one member of a group.
type Participant struct {
	JID          string `json:"jid"`
	IsAdmin      bool   `json:"isAdmin,omitempty"`
	IsSuperAdmin bool   `json:"isSuperAdmin,omitempty"`
}

// ListGroups returns all groups the account is currently a member of.
func (c *Client) ListGroups(ctx context.Context) ([]Group, error) {
	groups, err := c.WA.GetJoinedGroups(ctx)
	if err != nil {
		return nil, waerrors.Wrap(err, "loading groups")
	}

	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, Group{
			JID:              g.JID.String(),
			Name:             g.GroupName.Name,
			Topic:            g.Topic,
			ParticipantCount: len(g.Participants),
		})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// GroupInfo returns details and the participant list for one group.
func (c *Client) GroupInfo(ctx context.Context, jid types.JID) (Group, []Participant, error) {
	info, err := c.WA.GetGroupInfo(ctx, jid)
	if err != nil {
		return Group{}, nil, waerrors.Wrap(err, "loading group info")
	}

	group := Group{
		JID:              info.JID.String(),
		Name:             info.GroupName.Name,
		Topic:            info.Topic,
		ParticipantCount: len(info.Participants),
	}

	participants := make([]Participant, 0, len(info.Participants))
	for _, p := range info.Participants {
		participants = append(participants, Participant{
			JID:          p.JID.String(),
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}
	return group, participants, nil
}

// CreateGroup creates a new group with the given name and participants.
func (c *Client) CreateGroup(ctx context.Context, name string, participants []types.JID) (Group, error) {
	info, err := c.WA.CreateGroup(ctx, whatsmeow.ReqCreateGroup{
		Name:         name,
		Participants: participants,
	})
	if err != nil {
		return Group{}, waerrors.Wrap(err, "creating group")
	}
	return Group{
		JID:              info.JID.String(),
		Name:             info.GroupName.Name,
		Topic:            info.Topic,
		ParticipantCount: len(info.Participants),
	}, nil
}

// AddParticipants adds participants to an existing group.
func (c *Client) AddParticipants(ctx context.Context, groupJID types.JID, participants []types.JID) error {
	_, err := c.WA.UpdateGroupParticipants(ctx, groupJID, participants, whatsmeow.ParticipantChangeAdd)
	if err != nil {
		return waerrors.Wrap(err, "adding participants")
	}
	return nil
}

// RemoveParticipants removes participants from a group.
func (c *Client) RemoveParticipants(ctx context.Context, groupJID types.JID, participants []types.JID) error {
	_, err := c.WA.UpdateGroupParticipants(ctx, groupJID, participants, whatsmeow.ParticipantChangeRemove)
	if err != nil {
		return waerrors.Wrap(err, "removing participants")
	}
	return nil
}

// Connect establishes the WhatsApp connection. whatsmeow's WA.Connect()
// only *initiates* the handshake and returns immediately — a request
// sent right after can race a socket that isn't ready yet. Rather than a
// blind fixed sleep here (which was slow even when unnecessary), callers
// that immediately send a request should wrap that request in withRetry
// instead — genuinely instant when the connection happens to be ready,
// short backoff only when it actually isn't.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.WA.Connect(); err != nil {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}
	return nil
}

// withRetry attempts fn up to 4 times with short, increasing backoff.
// Used for requests sent right after Connect(), where the first attempt
// may race a socket that isn't fully ready yet.
func withRetry(fn func() error) error {
	backoffs := []time.Duration{0, 300 * time.Millisecond, 800 * time.Millisecond, 2 * time.Second}
	var lastErr error
	for _, wait := range backoffs {
		if wait > 0 {
			time.Sleep(wait)
		}
		if err := fn(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// Login connects and, if unpaired, shows a QR code to scan. It stays
// connected for a grace period after pairing so WhatsApp has time to
// commit the linked device and, if a chatstore is attached, deliver the
// one-time HistorySync payload — disconnecting immediately after the QR
// "success" event is too early and the device never shows up in
// WhatsApp > Linked Devices.
func (c *Client) Login(ctx context.Context) error {
	if c.IsLoggedIn() {
		return c.Connect(ctx)
	}

	qrChan, err := c.WA.GetQRChannel(ctx)
	if err != nil {
		return waerrors.Wrap(err, "getting QR channel")
	}

	if err := c.WA.Connect(); err != nil {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}

	paired := false
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			fmt.Println("Scan this QR code in WhatsApp: Settings > Linked Devices > Link a Device")
			qr.Print(evt.Code)
		case "success":
			paired = true
			fmt.Println("QR scanned — finishing setup, keep this open...")
		case "timeout":
			return waerrors.New("QR code expired, run 'wa login' again")
		default:
			c.log.Infof("login event: %s", evt.Event)
		}
	}

	if !paired {
		return waerrors.New("login did not complete")
	}

	// Give WhatsApp time to finish the post-pairing handshake and, if a
	// chatstore is attached, deliver the one-time HistorySync payload.
	// This is a fixed wait rather than waiting on *events.Connected —
	// see Connect()'s comment for why.
	wait := 15 * time.Second
	if c.chats != nil {
		wait = 25 * time.Second
		fmt.Println("Syncing chat history — this can take a bit on first login...")
	} else {
		fmt.Println("Finishing setup...")
	}

	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return ctx.Err()
	}

	fmt.Println("Login successful.")
	return nil
}

// Watch keeps a long-running connection open, printing incoming messages
// and reconnecting on drops, until ctx is cancelled (e.g. Ctrl+C).
//
// Given the connection instability observed during Phase 2/3 testing
// (frequent "Error sending close to websocket" resets, most likely
// CGNAT-related on some networks), this is deliberately more defensive
// than SyncChats: it doesn't just trust whatsmeow's built-in reconnect
// (which may or may not be enabled depending on your pinned version —
// check `go doc github.com/polymorfa/hypermeow.Client` for an EnableAutoReconnect
// field or similar if reconnects don't happen automatically) — it also
// watches for *events.Disconnected itself and retries with backoff.
func (c *Client) Watch(ctx context.Context, guard *safety.Guard) error {
	if !c.IsLoggedIn() {
		return waerrors.ErrNotLoggedIn
	}

	c.WA.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			c.printIncoming(v, guard)
		case *events.Connected:
			fmt.Println("[connected]")
		case *events.Disconnected:
			fmt.Println("[disconnected — attempting to reconnect...]")
			go c.reconnectWithBackoff(ctx)
		case *events.LoggedOut:
			fmt.Println("[logged out remotely — run 'wa login' again]")
		}
	})

	if err := c.WA.Connect(); err != nil {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}
	defer c.WA.Disconnect()

	fmt.Println("Watching for new messages. Press Ctrl+C to stop.")

	<-ctx.Done()
	fmt.Println("\nStopping...")
	return nil
}

// reconnectWithBackoff retries WA.Connect() with exponential backoff
// (capped at 30s) until it succeeds or ctx is cancelled. This is a
// belt-and-suspenders fallback alongside whatsmeow's own reconnect
// handling — worth keeping given how unreliable the connection proved
// to be in earlier testing on some networks.
func (c *Client) reconnectWithBackoff(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if err := c.WA.Connect(); err != nil {
			c.log.Warnf("reconnect attempt failed: %v", err)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		fmt.Println("[reconnected]")
		return
	}
}

// printIncoming prints one incoming message and marks its sender known
// (so a later `wa chat send` reply doesn't trigger the new-recipient
// confirmation prompt).
func (c *Client) printIncoming(evt *events.Message, guard *safety.Guard) {
	if evt.Info.IsFromMe || isPseudoChat(evt.Info.Chat.String()) {
		return
	}

	sender := evt.Info.PushName
	if sender == "" {
		sender = evt.Info.Sender.User
	}

	mediaType, text := classifyMessage(evt.Message)
	switch {
	case mediaType != "" && text != "":
		text = fmt.Sprintf("[%s] %s", mediaType, text)
	case mediaType != "":
		text = fmt.Sprintf("[%s]", mediaType)
	case text == "":
		text = "[unsupported message type]"
	}

	ts := evt.Info.Timestamp.Local().Format("15:04:05")
	fmt.Printf("[%s] %s (%s): %s\n", ts, sender, evt.Info.Chat.String(), text)

	if guard != nil {
		if err := guard.MarkKnown(evt.Info.Sender.String()); err != nil {
			c.log.Warnf("failed to mark sender known: %v", err)
		}
	}
}

// Logout terminates the linked-device session on WhatsApp's servers and
// clears the local session, so a subsequent command needs `wa login`
// again. Returns waerrors.ErrNotLoggedIn if no session is active.
func (c *Client) Logout(ctx context.Context) error {
	if !c.IsLoggedIn() {
		return waerrors.ErrNotLoggedIn
	}
	err := withRetry(func() error {
		return c.WA.Logout(ctx)
	})
	if err != nil {
		return waerrors.Wrap(err, "logging out")
	}
	return nil
}

// Disconnect closes the current WhatsApp connection without invalidating
// the underlying session, so a later command can reconnect and resume
// using the same persisted device store.
func (c *Client) Disconnect() {
	c.WA.Disconnect()
}

// SendText sends a plain text message to jid and returns the sent
// message's ID (for later reply/forward reference). Callers should call
// Connect() first.
func (c *Client) SendText(ctx context.Context, jid types.JID, text string) (string, error) {
	msg := &waProto.Message{
		Conversation: proto.String(text),
	}
	var resp whatsmeow.SendResponse
	err := withRetry(func() error {
		var sendErr error
		resp, sendErr = c.WA.SendMessage(ctx, jid, msg)
		return sendErr
	})
	if err != nil {
		return "", waerrors.Wrap(err, "sending message")
	}
	c.recordOutgoingMessage(jid, resp.ID, text, "", msg)
	return resp.ID, nil
}

// EditMessage replaces the text of one of the account's own already-sent
// messages, using whatsmeow's BuildEdit — WhatsApp shows the recipient an
// "edited" label rather than a new message. Like revoke, WhatsApp enforces
// its own rules server-side (e.g. an edit window), which surface as a
// SendMessage error here rather than anything wa-cli can detect up front.
func (c *Client) EditMessage(ctx context.Context, chatJID string, messageID string, newText string) error {
	if c.WA == nil || !c.WA.IsConnected() {
		return fmt.Errorf("cannot edit while WhatsApp is disconnected")
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	if messageID == "" {
		return fmt.Errorf("cannot edit a message without an ID")
	}
	edit := c.WA.BuildEdit(chat, types.MessageID(messageID), &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String(newText),
		},
	})
	return withRetry(func() error {
		_, sendErr := c.WA.SendMessage(ctx, chat, edit)
		return sendErr
	})
}

// RevokeMessage deletes one of the account's own messages for everyone in
// the chat. WhatsApp rejects a revoke when its server-side rules do not allow
// it (for example, an expired deletion window).
func (c *Client) RevokeMessage(ctx context.Context, chatJID string, messageID string) error {
	if c.WA == nil || !c.WA.IsConnected() {
		return fmt.Errorf("cannot delete while WhatsApp is disconnected")
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	if messageID == "" {
		return fmt.Errorf("cannot delete a message without an ID")
	}
	return withRetry(func() error {
		_, sendErr := c.WA.RevokeMessage(ctx, chat, types.MessageID(messageID))
		return sendErr
	})
}

// SendReply sends text to jid as a quoted reply to quoted.
func (c *Client) SendReply(ctx context.Context, jid types.JID, text string, quoted msgstore.Message) (string, error) {
	if c.WA == nil || c.WA.Store == nil {
		return "", fmt.Errorf("cannot reply while WhatsApp is disconnected")
	}
	self := c.WA.Store.GetLID()
	if self.IsEmpty() {
		self = c.WA.Store.GetJID()
	}
	ctxInfo, err := replyContextInfo(quoted, self)
	if err != nil {
		return "", err
	}

	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: ctxInfo,
		},
	}
	var resp whatsmeow.SendResponse
	err = withRetry(func() error {
		var sendErr error
		resp, sendErr = c.WA.SendMessage(ctx, jid, msg)
		return sendErr
	})
	if err != nil {
		return "", waerrors.Wrap(err, "sending reply")
	}
	c.recordOutgoingMessage(jid, resp.ID, text, "", msg)
	return resp.ID, nil
}

func replyContextInfo(quoted msgstore.Message, self types.JID) (*waProto.ContextInfo, error) {
	if quoted.ID == "" {
		return nil, fmt.Errorf("cannot reply to a message without an ID")
	}

	participant := quoted.SenderJID
	if quoted.FromMe || participant == "me" {
		if self.IsEmpty() {
			return nil, fmt.Errorf("cannot identify the sender of your message")
		}
		participant = self.ToNonAD().String()
	} else {
		sender, err := types.ParseJID(participant)
		if err != nil || sender.IsEmpty() {
			return nil, fmt.Errorf("cannot identify the sender of the quoted message")
		}
		participant = sender.ToNonAD().String()
	}

	quotedMessage := decodeRawProto(quoted.RawProto)
	if quotedMessage == nil && quoted.Text != "" {
		quotedMessage = &waProto.Message{Conversation: proto.String(quoted.Text)}
	}
	if quotedMessage == nil {
		return nil, fmt.Errorf("cannot quote this message because its original content is unavailable")
	}

	return &waProto.ContextInfo{
		StanzaID:      proto.String(quoted.ID),
		Participant:   proto.String(participant),
		QuotedMessage: quotedMessage,
	}, nil
}

// SendImage uploads data and sends it as an image message with an
// optional caption.
func (c *Client) SendImage(ctx context.Context, jid types.JID, data []byte, mimetype, caption string) (string, error) {
	upload, err := c.WA.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return "", waerrors.Wrap(err, "uploading image")
	}
	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           proto.String(upload.URL),
			DirectPath:    proto.String(upload.DirectPath),
			MediaKey:      upload.MediaKey,
			Mimetype:      proto.String(mimetype),
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
		},
	}
	if caption != "" {
		msg.ImageMessage.Caption = proto.String(caption)
	}
	return c.sendMedia(ctx, jid, msg, "image", caption)
}

// SendVideo uploads data and sends it as a video message with an
// optional caption.
func (c *Client) SendVideo(ctx context.Context, jid types.JID, data []byte, mimetype, caption string) (string, error) {
	upload, err := c.WA.Upload(ctx, data, whatsmeow.MediaVideo)
	if err != nil {
		return "", waerrors.Wrap(err, "uploading video")
	}
	msg := &waProto.Message{
		VideoMessage: &waProto.VideoMessage{
			URL:           proto.String(upload.URL),
			DirectPath:    proto.String(upload.DirectPath),
			MediaKey:      upload.MediaKey,
			Mimetype:      proto.String(mimetype),
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
		},
	}
	if caption != "" {
		msg.VideoMessage.Caption = proto.String(caption)
	}
	return c.sendMedia(ctx, jid, msg, "video", caption)
}

// SendAudio uploads data and sends it as an audio message. Set voice=true
// to send as a voice note (PTT) rather than a regular audio file.
func (c *Client) SendAudio(ctx context.Context, jid types.JID, data []byte, mimetype string, voice bool, seconds uint32) (string, error) {
	upload, err := c.WA.Upload(ctx, data, whatsmeow.MediaAudio)
	if err != nil {
		return "", waerrors.Wrap(err, "uploading audio")
	}
	if mimetype == "" || mimetype == "audio/ogg" {
		mimetype = "audio/ogg; codecs=opus"
	}
	if seconds == 0 {
		seconds = 1
	}
	msg := &waProto.Message{
		AudioMessage: &waProto.AudioMessage{
			URL:           proto.String(upload.URL),
			DirectPath:    proto.String(upload.DirectPath),
			MediaKey:      upload.MediaKey,
			Mimetype:      proto.String(mimetype),
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			PTT:           proto.Bool(voice),
			Seconds:       proto.Uint32(seconds),
		},
	}
	if voice {
		msg.AudioMessage.Waveform = voiceWaveform(data)
	}
	kind := "audio"
	if voice {
		kind = "voice note"
	}
	return c.sendMedia(ctx, jid, msg, kind, "")
}

// voiceWaveform returns the 64-sample (0-100) waveform WhatsApp shows in a
// voice note bubble, or nil when ffmpeg cannot decode the data.
func voiceWaveform(data []byte) []byte {
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", "pipe:0", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	const bars = 64
	n := len(out) / 2
	if err != nil || n < bars {
		return nil
	}
	peaks := make([]int, bars)
	maxPeak := 0
	for i := 0; i < n; i++ {
		v := int(int16(uint16(out[2*i]) | uint16(out[2*i+1])<<8))
		if v < 0 {
			v = -v
		}
		if b := i * bars / n; v > peaks[b] {
			peaks[b] = v
		}
	}
	for _, p := range peaks {
		if p > maxPeak {
			maxPeak = p
		}
	}
	if maxPeak == 0 {
		return nil
	}
	wave := make([]byte, bars)
	for i, p := range peaks {
		wave[i] = byte(p * 100 / maxPeak)
	}
	return wave
}

// SendDocument uploads data and sends it as a document message.
func (c *Client) SendDocument(ctx context.Context, jid types.JID, data []byte, mimetype, filename, caption string) (string, error) {
	upload, err := c.WA.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return "", waerrors.Wrap(err, "uploading document")
	}
	msg := &waProto.Message{
		DocumentMessage: &waProto.DocumentMessage{
			URL:           proto.String(upload.URL),
			DirectPath:    proto.String(upload.DirectPath),
			MediaKey:      upload.MediaKey,
			Mimetype:      proto.String(mimetype),
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			FileName:      proto.String(filename),
		},
	}
	if caption != "" {
		msg.DocumentMessage.Caption = proto.String(caption)
	}
	return c.sendMedia(ctx, jid, msg, "document", caption)
}

// SendSticker uploads data and sends it as a sticker message. data
// should already be in WebP format — WhatsApp doesn't accept other
// formats for stickers, and this doesn't convert for you.
func (c *Client) SendSticker(ctx context.Context, jid types.JID, data []byte, mimetype string) (string, error) {
	upload, err := c.WA.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return "", waerrors.Wrap(err, "uploading sticker")
	}
	msg := &waProto.Message{
		StickerMessage: &waProto.StickerMessage{
			URL:           proto.String(upload.URL),
			DirectPath:    proto.String(upload.DirectPath),
			MediaKey:      upload.MediaKey,
			Mimetype:      proto.String(mimetype),
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
		},
	}
	return c.sendMedia(ctx, jid, msg, "sticker", "")
}

func (c *Client) sendMedia(ctx context.Context, jid types.JID, msg *waProto.Message, kind, caption string) (string, error) {
	var resp whatsmeow.SendResponse
	err := withRetry(func() error {
		var sendErr error
		resp, sendErr = c.WA.SendMessage(ctx, jid, msg)
		return sendErr
	})
	if err != nil {
		return "", waerrors.Wrap(err, "sending "+kind)
	}

	c.recordOutgoingMessage(jid, resp.ID, caption, kind, msg)
	return resp.ID, nil
}

// recordOutgoingMessage saves outgoing messages to msgstore and updates chatstore.
func (c *Client) recordOutgoingMessage(jid types.JID, msgID, text, mediaType string, msg *waProto.Message) {
	now := time.Now().UnixMilli()

	var raw string
	if msg != nil {
		if b, err := proto.Marshal(msg); err == nil {
			raw = base64.StdEncoding.EncodeToString(b)
		}
	}

	if c.msgs != nil {
		err := c.msgs.Append(msgstore.Message{
			ID:        msgID,
			ChatJID:   jid.String(),
			SenderJID: "me",
			Timestamp: now,
			Text:      text,
			FromMe:    true,
			MediaType: mediaType,
			RawProto:  raw,
		})
		if err != nil {
			c.log.Warnf("msgstore append failed for sent message %s: %v", msgID, err)
		}
	}

	if c.chats != nil {
		preview := text
		switch {
		case mediaType != "" && text != "":
			preview = fmt.Sprintf("[%s] %s", mediaType, text)
		case mediaType != "":
			preview = fmt.Sprintf("[%s]", mediaType)
		}
		err := c.chats.Upsert(chatstore.Chat{
			JID:                jid.String(),
			IsGroup:            strings.HasSuffix(jid.String(), "@g.us"),
			LastMessageAt:      now,
			LastMessagePreview: preview,
		})
		if err != nil {
			c.log.Warnf("chatstore upsert failed for sent message to %s: %v", jid.String(), err)
		}
	}
}

// DownloadMedia reconstructs a stored message's media content from its
// RawProto and downloads it via WhatsApp's encrypted media CDN. Returns
// the raw bytes and the mimetype recorded with the message.
func (c *Client) DownloadMedia(ctx context.Context, quoted msgstore.Message) ([]byte, string, error) {
	if strings.Contains(strings.ToLower(quoted.MediaType), "de una sola vez") {
		return nil, "", waerrors.New("el contenido de una sola vez se abre desde el celular")
	}
	original := decodeRawProto(quoted.RawProto)
	if original == nil {
		return nil, "", waerrors.New("original message content isn't available (too old, or not captured)")
	}

	var downloadable whatsmeow.DownloadableMessage
	var mimetype string
	switch {
	case original.GetImageMessage() != nil:
		downloadable = original.GetImageMessage()
		mimetype = original.GetImageMessage().GetMimetype()
	case original.GetVideoMessage() != nil:
		downloadable = original.GetVideoMessage()
		mimetype = original.GetVideoMessage().GetMimetype()
	case original.GetAudioMessage() != nil:
		downloadable = original.GetAudioMessage()
		mimetype = original.GetAudioMessage().GetMimetype()
	case original.GetDocumentMessage() != nil:
		downloadable = original.GetDocumentMessage()
		mimetype = original.GetDocumentMessage().GetMimetype()
	case original.GetStickerMessage() != nil:
		downloadable = original.GetStickerMessage()
		mimetype = original.GetStickerMessage().GetMimetype()
	default:
		return nil, "", waerrors.New("this message doesn't contain downloadable media")
	}

	data, err := c.WA.Download(ctx, downloadable)
	if err != nil {
		return nil, "", waerrors.Wrap(err, "downloading media")
	}
	return data, mimetype, nil
}

// DocumentFileName returns the original name supplied by WhatsApp for a
// stored document. It never returns a path, so callers can safely place the
// result inside their chosen download directory.
func DocumentFileName(quoted msgstore.Message) string {
	original := decodeRawProto(quoted.RawProto)
	if original == nil || original.GetDocumentMessage() == nil {
		return ""
	}
	name := filepath.Base(strings.TrimSpace(original.GetDocumentMessage().GetFileName()))
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return ""
	}
	return name
}

// ForwardMessage re-sends quoted's content to toJID, without the forwarded label.
// For media messages, this reuses the original upload (URL/MediaKey/
// hashes) rather than re-uploading.
func (c *Client) ForwardMessage(ctx context.Context, toJID types.JID, quoted msgstore.Message) (string, error) {
	original := decodeRawProto(quoted.RawProto)
	if original == nil {
		return "", waerrors.New("original message content isn't available to forward (too old, or not captured)")
	}

	msg, err := markForwarded(original)
	if err != nil {
		return "", err
	}

	var resp whatsmeow.SendResponse
	err = withRetry(func() error {
		var sendErr error
		resp, sendErr = c.WA.SendMessage(ctx, toJID, msg)
		return sendErr
	})
	if err != nil {
		return "", waerrors.Wrap(err, "forwarding message")
	}
	mediaType, text := classifyMessage(msg)
	c.recordOutgoingMessage(toJID, resp.ID, text, mediaType, msg)
	return resp.ID, nil
}

// markForwarded returns a message equivalent to original but WITHOUT the
// WhatsApp "Forwarded" label: IsForwarded/ForwardingScore are cleared from
// its ContextInfo (also when the original itself was forwarded), so the
// recipient sees a normal message. Supports text and all 5 media types;
// anything else is rejected.
func markForwarded(original *waProto.Message) (*waProto.Message, error) {
	bump := func(ci *waProto.ContextInfo) *waProto.ContextInfo {
		if ci == nil {
			return nil
		}
		ci.IsForwarded = nil
		ci.ForwardingScore = nil
		return ci
	}

	switch {
	case original.GetConversation() != "":
		return &waProto.Message{Conversation: proto.String(original.GetConversation())}, nil
	case original.GetExtendedTextMessage() != nil:
		m := original.GetExtendedTextMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{ExtendedTextMessage: m}, nil
	case original.GetImageMessage() != nil:
		m := original.GetImageMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{ImageMessage: m}, nil
	case original.GetVideoMessage() != nil:
		m := original.GetVideoMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{VideoMessage: m}, nil
	case original.GetAudioMessage() != nil:
		m := original.GetAudioMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{AudioMessage: m}, nil
	case original.GetDocumentMessage() != nil:
		m := original.GetDocumentMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{DocumentMessage: m}, nil
	case original.GetStickerMessage() != nil:
		m := original.GetStickerMessage()
		m.ContextInfo = bump(m.ContextInfo)
		return &waProto.Message{StickerMessage: m}, nil
	default:
		return nil, waerrors.New("forwarding this message type isn't supported yet")
	}
}

// decodeRawProto reconstructs a stored message's original content from
// its base64-encoded protobuf, returning nil if unavailable or decoding
// fails (callers should degrade gracefully rather than error out — a
// reply/forward without the original quote attached is still useful).
func decodeRawProto(raw string) *waProto.Message {
	if raw == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	msg := &waProto.Message{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil
	}
	return msg
}

// ExtractQuotedContext inspects a message's RawProto to extract any quoted reply
// context, including a quoted audio's duration when WhatsApp supplied it.
func ExtractQuotedContext(rawProto string) (quotedText, participant, stanzaID string, audioSeconds uint32, ok bool) {
	if rawProto == "" {
		return "", "", "", 0, false
	}
	m := decodeRawProto(rawProto)
	if m == nil {
		return "", "", "", 0, false
	}

	var ctxInfo *waProto.ContextInfo
	switch {
	case m.ExtendedTextMessage != nil:
		ctxInfo = m.ExtendedTextMessage.ContextInfo
	case m.ImageMessage != nil:
		ctxInfo = m.ImageMessage.ContextInfo
	case m.VideoMessage != nil:
		ctxInfo = m.VideoMessage.ContextInfo
	case m.AudioMessage != nil:
		ctxInfo = m.AudioMessage.ContextInfo
	case m.DocumentMessage != nil:
		ctxInfo = m.DocumentMessage.ContextInfo
	case m.StickerMessage != nil:
		ctxInfo = m.StickerMessage.ContextInfo
	}

	if ctxInfo == nil {
		return "", "", "", 0, false
	}

	participant = ctxInfo.GetParticipant()
	stanzaID = ctxInfo.GetStanzaID()

	if ctxInfo.QuotedMessage != nil {
		if audio := ctxInfo.QuotedMessage.GetAudioMessage(); audio != nil {
			audioSeconds = audio.GetSeconds()
		}
		mediaType, text := classifyMessage(ctxInfo.QuotedMessage)
		switch {
		case mediaType != "" && text != "":
			quotedText = fmt.Sprintf("[%s] %s", mediaType, text)
		case mediaType != "":
			quotedText = fmt.Sprintf("[%s]", mediaType)
		case text != "":
			quotedText = text
		}
	}

	hasQuote := quotedText != "" || stanzaID != "" || participant != ""
	return quotedText, participant, stanzaID, audioSeconds, hasQuote
}
