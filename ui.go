package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[90m"
	ansiCyan    = "\033[36m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiReverse = "\033[7m"
)

type slashCmd struct {
	Name string
	Desc string
}

var slashCommands = []slashCmd{
	{"/chats", "Browse and switch chats (Ctrl+O)"},
	{"/open", "Open chat by name (/open Name)"},
	{"/reply", "Reply with quote to a message"},
	{"/react", "React with emoji (/react 👍 or /react)"},
	{"/edit", "Edit your last sent message (/edit text)"},
	{"/attach", "Send image or file (/attach path [caption])"},
	{"/copy", "Copy latest image (/copy list to pick)"},
	{"/view", "Open attachment (/view list to pick)"},
	{"/find", "Search messages in chat (/find query)"},
	{"/unread", "Show chats with unread messages"},
	{"/refresh", "Sync contacts and groups"},
	{"/help", "Show commands and shortcuts"},
	{"/quit", "Exit"},
}

type overlayMode int

const (
	overlayNone overlayMode = iota
	overlayChats
	overlayUnread
	overlayCopy
	overlayView
	overlayReply
	overlayReact
	overlayFind
	overlayHelp
)

type attItem struct {
	TS     int64
	Sender string
	Att    Attachment
}

type AppUI struct {
	account string
	store   *Store
	rpc     *RPCClient

	mu           sync.Mutex
	activeChat   *Chat
	statusText   string
	typingUntil  time.Time
	typingSender string
	typingChatID string
	scrollOffset int

	input      []rune
	cursor     int
	history    []string
	historyIdx int

	replyTarget  *Message
	pendingEmoji string

	overlay      overlayMode
	overlayIdx   int
	overlayQuery []rune
	attItems     []attItem
	msgPicker    []Message
	findResults  []Message
	findQuery    string

	running  bool
	demoMode bool
	redraw   chan struct{}
}

func NewAppUI(account string, store *Store) *AppUI {
	ui := &AppUI{
		account:    account,
		store:      store,
		statusText: "Connecting...",
		historyIdx: -1,
		running:    true,
		redraw:     make(chan struct{}, 16),
	}
	if last, ok := store.GetLastChat(); ok {
		ui.activeChat = &last
		store.MarkChatRead(last.ChatID)
	}
	return ui
}

func (u *AppUI) triggerRedraw() {
	select {
	case u.redraw <- struct{}{}:
	default:
	}
}

func (u *AppUI) Run() error {
	enableVirtualTerminal()
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}

	os.Stdout.WriteString("\033[?1049h\033[H")
	defer func() {
		os.Stdout.WriteString("\033[?1049l\033[?25h")
		_ = term.Restore(fd, oldState)
	}()

	if u.demoMode {
		u.mu.Lock()
		u.statusText = "Connected (Demo)"
		u.mu.Unlock()
	} else {
		u.rpc = NewRPCClient(u.account, u.onEvent, u.onStatus)
		if err := u.rpc.Start(); err != nil {
			return err
		}
		defer u.rpc.Stop()

		go u.syncMetadata()
	}

	stopResize := make(chan struct{})
	defer close(stopResize)
	sigWinch := watchTerminalResize(stopResize)

	keyCh := make(chan []byte, 32)
	go func() {
		buf := make([]byte, 512)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			cp := make([]byte, n)
			copy(cp, buf[:n])
			keyCh <- cp
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	u.render()

	for {
		u.mu.Lock()
		stillRunning := u.running
		u.mu.Unlock()
		if !stillRunning {
			return nil
		}

		select {
		case data := <-keyCh:
			u.handleKeys(data)
			u.render()
		case <-sigWinch:
			u.render()
		case <-u.redraw:
			u.render()
		case <-ticker.C:
			u.mu.Lock()
			if u.typingSender != "" && time.Now().After(u.typingUntil) {
				u.typingSender = ""
				u.typingChatID = ""
				u.mu.Unlock()
				u.render()
			} else {
				u.mu.Unlock()
			}
		}
	}
}

func (u *AppUI) syncMetadata() {
	if u.demoMode || u.rpc == nil {
		return
	}
	var contacts []Contact
	var chats []Chat

	rawContacts, cErr := u.rpc.ListContacts()
	if cErr == nil {
		for _, c := range rawContacts {
			uuid := asString(c["uuid"])
			num := asString(c["number"])
			name := asString(c["name"])
			profile := asString(c["profileName"])
			if p, ok := c["profile"].(map[string]any); ok && profile == "" {
				profile = asString(p["givenName"])
			}
			contacts = append(contacts, Contact{
				UUID:        uuid,
				Number:      num,
				Name:        name,
				ProfileName: profile,
			})
			display := name
			if display == "" {
				display = profile
			}
			if display == "" {
				display = num
			}
			if display == "" {
				display = uuid
			}
			if num == u.account && num != "" {
				display = "Note to Self"
			}
			cid := uuid
			if cid == "" {
				cid = num
			}
			if cid != "" && display != "" {
				chats = append(chats, Chat{
					ChatID:   cid,
					ChatType: "dm",
					Name:     display,
					Number:   num,
				})
			}
		}
	}

	rawGroups, gErr := u.rpc.ListGroups()
	if gErr == nil {
		for _, g := range rawGroups {
			gid := asString(g["id"])
			gname := asString(g["name"])
			if gname == "" {
				gname = gid
			}
			if gid != "" {
				chats = append(chats, Chat{
					ChatID:   "group:" + gid,
					ChatType: "group",
					Name:     gname,
					GroupID:  gid,
				})
			}
		}
	}

	if cErr == nil || gErr == nil {
		u.store.SyncMetadata(contacts, chats)
	}

	u.mu.Lock()
	if cErr != nil {
		u.statusText = fmt.Sprintf("Sync error: %v", cErr)
	} else if gErr != nil {
		u.statusText = fmt.Sprintf("Sync error: %v", gErr)
	} else {
		u.statusText = "Connected"
		if u.activeChat != nil {
			if updated, ok := u.store.GetChat(u.activeChat.ChatID); ok {
				u.activeChat = &updated
			}
		} else {
			if last, ok := u.store.GetLastChat(); ok {
				u.activeChat = &last
				u.store.MarkChatRead(last.ChatID)
			} else if len(chats) > 0 && u.overlay == overlayNone {
				u.overlay = overlayChats
			}
		}
	}
	u.mu.Unlock()
	u.triggerRedraw()
}

func (u *AppUI) onStatus(text string) {
	u.mu.Lock()
	u.statusText = text
	u.mu.Unlock()
	u.triggerRedraw()
}

func (u *AppUI) openChat(chosen Chat) {
	u.mu.Lock()
	u.activeChat = &chosen
	u.typingSender = ""
	u.typingChatID = ""
	u.scrollOffset = 0
	u.replyTarget = nil
	u.overlay = overlayNone
	u.overlayQuery = nil
	u.mu.Unlock()

	hadUnread := chosen.UnreadCount > 0
	u.store.MarkChatRead(chosen.ChatID)
	u.store.SetLastChat(chosen.ChatID)

	if hadUnread && chosen.ChatType == "dm" {
		go u.sendReadReceiptsForChat(chosen)
	}
}

func (u *AppUI) sendReadReceiptsForChat(chat Chat) {
	recipient := chat.Number
	if recipient == "" {
		recipient = u.store.ResolveNumber(chat.ChatID)
	}
	if recipient == "" || recipient == u.account {
		return
	}
	msgs := u.store.GetMessages(chat.ChatID, 25)
	var ts []int64
	for _, m := range msgs {
		if !m.IsOutgoing && m.Timestamp > 0 {
			ts = append(ts, m.Timestamp)
		}
	}
	if len(ts) > 10 {
		ts = ts[len(ts)-10:]
	}
	if len(ts) > 0 && u.rpc != nil {
		_ = u.rpc.SendReadReceipt(recipient, ts)
	}
}

func (u *AppUI) onEvent(ev IncomingEvent) {
	chatID := ev.ChatID
	if ev.ChatType == "dm" && chatID != "" {
		if canon := u.store.CanonicalChatID(chatID, ev.Number); canon != "" {
			chatID = canon
		}
	}

	switch ev.Kind {
	case "typing":
		u.mu.Lock()
		if u.activeChat != nil && chatID == u.activeChat.ChatID {
			if ev.IsTyping {
				name := u.store.ResolveContactName(ev.SenderID)
				if name == ev.SenderID && ev.SenderName != "" {
					name = ev.SenderName
				}
				u.typingSender = name
				u.typingChatID = chatID
				u.typingUntil = time.Now().Add(10 * time.Second)
			} else {
				u.typingSender = ""
				u.typingChatID = ""
			}
		}
		u.mu.Unlock()
		u.triggerRedraw()

	case "receipt":
		id := ev.SenderID
		if id == "" {
			id = ev.SenderNumber
		}
		if u.store.UpdateReceipt(id, ev.ReceiptTimestamps, ev.ReceiptType) {
			u.triggerRedraw()
		}

	case "delete":
		if u.store.DeleteMessage(chatID, ev.TargetTimestamp) {
			u.triggerRedraw()
		}

	case "edit":
		if u.store.EditMessage(chatID, ev.TargetTimestamp, ev.Body) {
			u.triggerRedraw()
		}

	case "reaction":
		if ev.ReactionTargetTS > 0 && ev.ReactionEmoji != "" {
			if u.store.AddReaction(chatID, ev.ReactionTargetTS, ev.ReactionEmoji, ev.SenderName, ev.ReactionRemove) {
				u.triggerRedraw()
			}
		}

	case "message":
		sender := "You"
		if !ev.IsOutgoing {
			sender = u.store.ResolveContactName(ev.SenderID)
			if sender == ev.SenderID && ev.SenderName != "" {
				sender = ev.SenderName
			}
		}
		chatName := ev.ChatName
		if ev.ChatType == "dm" {
			resolved := u.store.ResolveContactName(chatID)
			if resolved != chatID {
				chatName = resolved
			}
		}

		u.mu.Lock()
		isActive := u.activeChat != nil && u.activeChat.ChatID == chatID
		if isActive && !ev.IsOutgoing {
			u.typingSender = ""
			u.typingChatID = ""
		}
		if isActive {
			u.scrollOffset = 0
		}
		u.mu.Unlock()

		preview := ev.Body
		if preview == "" && len(ev.Attachments) > 0 {
			preview = "[" + ev.Attachments[0].Name + "]"
		}
		canonID := u.store.UpsertChat(chatID, ev.ChatType, chatName, ev.Number, ev.GroupID, ev.Timestamp, preview, !isActive && !ev.IsOutgoing)
		inserted := u.store.AddMessage(Message{
			ChatID:      canonID,
			SenderID:    ev.SenderID,
			SenderName:  sender,
			Body:        ev.Body,
			Timestamp:   ev.Timestamp,
			IsOutgoing:  ev.IsOutgoing,
			QuoteAuthor: ev.QuoteAuthor,
			QuoteText:   ev.QuoteText,
			Attachments: ev.Attachments,
		})

		if inserted {
			if isActive && !ev.IsOutgoing && ev.ChatType == "dm" && ev.Timestamp > 0 {
				recip := ev.SenderNumber
				if recip == "" {
					recip = u.store.ResolveNumber(ev.SenderID)
				}
				if recip != "" && recip != u.account && u.rpc != nil {
					go func(r string, ts int64) { _ = u.rpc.SendReadReceipt(r, []int64{ts}) }(recip, ev.Timestamp)
				}
			} else if !isActive && !ev.IsOutgoing {
				NotifyDesktop("Signal · "+chatName, sender+": "+preview)
			}
			u.triggerRedraw()
		}
	}
}

func (u *AppUI) handleKeys(data []byte) {
	i := 0
	for i < len(data) {
		b := data[i]

		// Ctrl+C -> immediate exit
		if b == 0x03 {
			u.mu.Lock()
			u.running = false
			u.mu.Unlock()
			return
		}

		// Escape or CSI / Alt sequences
		if b == 0x1b {
			if i+1 < len(data) {
				next := data[i+1]
				// Alt+Backspace -> delete word backward
				if next == 0x7f || next == 0x08 {
					u.deleteWordBackward()
					i += 2
					continue
				}
				// Alt+Enter -> insert newline
				if next == '\r' || next == '\n' {
					u.insertRune('\n')
					i += 2
					continue
				}
				if next == '[' {
					// Parse CSI sequence up to final byte (0x40..0x7E)
					seqStart := i + 2
					seqEnd := seqStart
					for seqEnd < len(data) && (data[seqEnd] < 0x40 || data[seqEnd] > 0x7e) {
						seqEnd++
					}
					if seqEnd < len(data) {
						params := string(data[seqStart:seqEnd])
						final := data[seqEnd]
						u.handleCSI(params, final)
						i = seqEnd + 1
						continue
					}
				}
			}

			// Plain Escape -> close overlay, or cancel reply, or reset scroll
			u.mu.Lock()
			if u.overlay != overlayNone {
				u.overlay = overlayNone
				u.overlayQuery = nil
				u.overlayIdx = 0
				u.pendingEmoji = ""
			} else if u.replyTarget != nil {
				u.replyTarget = nil
			} else if u.scrollOffset > 0 {
				u.scrollOffset = 0
			}
			u.mu.Unlock()
			i++
			continue
		}

		switch b {
		case 0x01: // Ctrl+A -> home
			u.mu.Lock()
			u.cursor = 0
			u.mu.Unlock()
		case 0x05: // Ctrl+E -> end
			u.mu.Lock()
			u.cursor = len(u.input)
			u.mu.Unlock()
		case 0x15: // Ctrl+U -> clear before cursor (or clear filter)
			u.mu.Lock()
			if u.overlay == overlayChats || u.overlay == overlayUnread {
				u.overlayQuery = nil
				u.overlayIdx = 0
			} else if u.overlay == overlayNone && u.cursor > 0 {
				u.input = u.input[u.cursor:]
				u.cursor = 0
			}
			u.mu.Unlock()
		case 0x17: // Ctrl+W -> delete word backward
			u.deleteWordBackward()
		case 0x08: // Ctrl+Backspace (0x08) vs Backspace (0x7f)
			u.deleteWordBackward()
		case 0x7f: // Backspace
			u.mu.Lock()
			if u.overlay == overlayChats || u.overlay == overlayUnread {
				if len(u.overlayQuery) > 0 {
					u.overlayQuery = u.overlayQuery[:len(u.overlayQuery)-1]
					u.overlayIdx = 0
				}
			} else if u.overlay == overlayNone && u.cursor > 0 {
				u.input = append(u.input[:u.cursor-1], u.input[u.cursor:]...)
				u.cursor--
			}
			u.mu.Unlock()
		case 0x0b: // Ctrl+K -> open chats if empty, else kill to end
			u.mu.Lock()
			if len(u.input) == 0 && u.overlay == overlayNone {
				u.overlay = overlayChats
				u.overlayIdx = 0
				u.overlayQuery = nil
			} else if u.overlay == overlayNone && u.cursor < len(u.input) {
				u.input = u.input[:u.cursor]
			}
			u.mu.Unlock()
		case 0x0f, 0x10: // Ctrl+O / Ctrl+P -> quick chat switcher
			u.mu.Lock()
			if u.overlay == overlayChats {
				u.overlay = overlayNone
			} else {
				u.overlay = overlayChats
				u.overlayIdx = 0
				u.overlayQuery = nil
			}
			u.mu.Unlock()
		case 0x04: // Ctrl+D
			u.mu.Lock()
			if len(u.input) == 0 && u.overlay == overlayNone {
				u.running = false
				u.mu.Unlock()
				return
			}
			u.mu.Unlock()
		case 0x16: // Ctrl+V -> paste text, image, or emoji in /react picker
			clip := ReadClipboard()
			if clip != "" {
				u.mu.Lock()
				if u.overlay == overlayReact {
					trimmed := strings.TrimSpace(clip)
					if trimmed != "" {
						u.pendingEmoji = trimmed
					}
				} else if u.overlay == overlayNone {
					runes := []rune(clip)
					newIn := make([]rune, 0, len(u.input)+len(runes))
					newIn = append(newIn, u.input[:u.cursor]...)
					newIn = append(newIn, runes...)
					newIn = append(newIn, u.input[u.cursor:]...)
					u.input = newIn
					u.cursor += len(runes)
				}
				u.mu.Unlock()
			}
		case '\r', '\n':
			u.handleEnter()
		case '\t':
			u.handleTab()
		default:
			r, size := utf8.DecodeRune(data[i:])
			if r != utf8.RuneError && unicode.IsPrint(r) {
				u.mu.Lock()
				if u.overlay == overlayChats || u.overlay == overlayUnread {
					u.overlayQuery = append(u.overlayQuery, r)
					u.overlayIdx = 0
				} else if u.overlay == overlayReact {
					quickEmojis := map[rune]string{
						'1': "👍",
						'2': "❤️",
						'3': "😂",
						'4': "😮",
						'5': "😢",
						'6': "🙏",
					}
					if em, ok := quickEmojis[r]; ok {
						u.pendingEmoji = em
					} else if r >= 0x1F000 || (r >= 0x2600 && r <= 0x27BF) {
						u.pendingEmoji = string(r)
					} else if r == 0xFE0F && u.pendingEmoji != "" {
						u.pendingEmoji += string(r)
					}
				} else if u.overlay == overlayHelp && r == 'q' {
					u.overlay = overlayNone
				} else if u.overlay == overlayFind && r == 'q' {
					u.overlay = overlayNone
				} else if u.overlay == overlayNone {
					newIn := make([]rune, 0, len(u.input)+1)
					newIn = append(newIn, u.input[:u.cursor]...)
					newIn = append(newIn, r)
					newIn = append(newIn, u.input[u.cursor:]...)
					u.input = newIn
					u.cursor++
				}
				u.mu.Unlock()
			}
			if size <= 0 {
				size = 1
			}
			i += size
			continue
		}
		i++
	}
}

func (u *AppUI) handleCSI(params string, final byte) {
	switch final {
	case 'A': // Up or Ctrl+Up
		if params == "1;5" {
			u.scrollChat(3)
		} else {
			u.handleUp()
		}
	case 'B': // Down or Ctrl+Down
		if params == "1;5" {
			u.scrollChat(-3)
		} else {
			u.handleDown()
		}
	case 'C': // Right or Ctrl+Right
		if params == "1;5" {
			u.moveWordRight()
		} else {
			u.mu.Lock()
			if u.overlay == overlayNone && u.cursor < len(u.input) {
				u.cursor++
			}
			u.mu.Unlock()
		}
	case 'D': // Left or Ctrl+Left
		if params == "1;5" {
			u.moveWordLeft()
		} else {
			u.mu.Lock()
			if u.overlay == overlayNone && u.cursor > 0 {
				u.cursor--
			}
			u.mu.Unlock()
		}
	case 'H': // Home
		u.mu.Lock()
		u.cursor = 0
		u.mu.Unlock()
	case 'F': // End
		u.mu.Lock()
		u.cursor = len(u.input)
		u.mu.Unlock()
	case '~':
		switch params {
		case "3": // Delete
			u.mu.Lock()
			if u.overlay == overlayNone && u.cursor < len(u.input) {
				u.input = append(u.input[:u.cursor], u.input[u.cursor+1:]...)
			}
			u.mu.Unlock()
		case "5": // PageUp
			u.scrollChat(10)
		case "6": // PageDown
			u.scrollChat(-10)
		}
	}
}

func (u *AppUI) scrollChat(delta int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.overlay != overlayNone {
		return
	}
	u.scrollOffset += delta
	if u.scrollOffset < 0 {
		u.scrollOffset = 0
	}
}

func (u *AppUI) insertRune(r rune) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.overlay != overlayNone {
		return
	}
	newIn := make([]rune, 0, len(u.input)+1)
	newIn = append(newIn, u.input[:u.cursor]...)
	newIn = append(newIn, r)
	newIn = append(newIn, u.input[u.cursor:]...)
	u.input = newIn
	u.cursor++
}

func (u *AppUI) moveWordLeft() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.overlay != overlayNone || u.cursor == 0 {
		return
	}
	pos := u.cursor
	for pos > 0 && unicode.IsSpace(u.input[pos-1]) {
		pos--
	}
	for pos > 0 && !unicode.IsSpace(u.input[pos-1]) {
		pos--
	}
	u.cursor = pos
}

func (u *AppUI) moveWordRight() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.overlay != overlayNone || u.cursor >= len(u.input) {
		return
	}
	pos := u.cursor
	for pos < len(u.input) && !unicode.IsSpace(u.input[pos]) {
		pos++
	}
	for pos < len(u.input) && unicode.IsSpace(u.input[pos]) {
		pos++
	}
	u.cursor = pos
}

func (u *AppUI) deleteWordBackward() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.overlay != overlayNone || u.cursor == 0 {
		return
	}
	pos := u.cursor
	for pos > 0 && unicode.IsSpace(u.input[pos-1]) {
		pos--
	}
	for pos > 0 && !unicode.IsSpace(u.input[pos-1]) {
		pos--
	}
	u.input = append(u.input[:pos], u.input[u.cursor:]...)
	u.cursor = pos
}

func (u *AppUI) handleUp() {
	u.mu.Lock()
	defer u.mu.Unlock()

	switch u.overlay {
	case overlayChats, overlayUnread:
		items := u.filteredOverlayChatsLocked()
		if len(items) > 0 {
			u.overlayIdx = (u.overlayIdx - 1 + len(items)) % len(items)
		}
		return
	case overlayCopy, overlayView:
		if len(u.attItems) > 0 {
			u.overlayIdx = (u.overlayIdx - 1 + len(u.attItems)) % len(u.attItems)
		}
		return
	case overlayReply, overlayReact:
		if len(u.msgPicker) > 0 {
			u.overlayIdx = (u.overlayIdx - 1 + len(u.msgPicker)) % len(u.msgPicker)
		}
		return
	case overlayFind:
		if len(u.findResults) > 0 {
			u.overlayIdx = (u.overlayIdx - 1 + len(u.findResults)) % len(u.findResults)
		}
		return
	}

	if len(u.history) > 0 {
		if u.historyIdx < len(u.history)-1 {
			u.historyIdx++
		}
		u.input = []rune(u.history[len(u.history)-1-u.historyIdx])
		u.cursor = len(u.input)
	}
}

func (u *AppUI) handleDown() {
	u.mu.Lock()
	defer u.mu.Unlock()

	switch u.overlay {
	case overlayChats, overlayUnread:
		items := u.filteredOverlayChatsLocked()
		if len(items) > 0 {
			u.overlayIdx = (u.overlayIdx + 1) % len(items)
		}
		return
	case overlayCopy, overlayView:
		if len(u.attItems) > 0 {
			u.overlayIdx = (u.overlayIdx + 1) % len(u.attItems)
		}
		return
	case overlayReply, overlayReact:
		if len(u.msgPicker) > 0 {
			u.overlayIdx = (u.overlayIdx + 1) % len(u.msgPicker)
		}
		return
	case overlayFind:
		if len(u.findResults) > 0 {
			u.overlayIdx = (u.overlayIdx + 1) % len(u.findResults)
		}
		return
	}

	if u.historyIdx > 0 {
		u.historyIdx--
		u.input = []rune(u.history[len(u.history)-1-u.historyIdx])
		u.cursor = len(u.input)
	} else if u.historyIdx == 0 {
		u.historyIdx = -1
		u.input = nil
		u.cursor = 0
	}
}

func (u *AppUI) handleTab() {
	u.mu.Lock()
	if u.overlay != overlayNone {
		u.mu.Unlock()
		return
	}
	if len(u.input) == 0 {
		u.overlay = overlayChats
		u.overlayIdx = 0
		u.overlayQuery = nil
		u.mu.Unlock()
		return
	}
	txt := string(u.input)
	if !strings.HasPrefix(txt, "/") || strings.Contains(txt, " ") {
		u.mu.Unlock()
		return
	}
	for _, sc := range slashCommands {
		if strings.HasPrefix(sc.Name, strings.ToLower(txt)) {
			u.input = []rune(sc.Name)
			u.cursor = len(u.input)
			u.mu.Unlock()
			return
		}
	}
	u.mu.Unlock()
}

func (u *AppUI) handleEnter() {
	u.mu.Lock()
	switch u.overlay {
	case overlayChats, overlayUnread:
		items := u.filteredOverlayChatsLocked()
		if len(items) > 0 {
			idx := min(u.overlayIdx, len(items)-1)
			chosen := items[idx]
			u.mu.Unlock()
			u.openChat(chosen)
			return
		}
		u.overlay = overlayNone
		u.mu.Unlock()
		return
	case overlayCopy:
		if len(u.attItems) > 0 {
			idx := min(u.overlayIdx, len(u.attItems)-1)
			chosen := u.attItems[idx].Att
			u.overlay = overlayNone
			u.mu.Unlock()
			_, msg := CopyAttachment(chosen)
			u.onStatus(msg)
			return
		}
		u.overlay = overlayNone
		u.mu.Unlock()
		return
	case overlayView:
		if len(u.attItems) > 0 {
			idx := min(u.overlayIdx, len(u.attItems)-1)
			chosen := u.attItems[idx].Att
			u.overlay = overlayNone
			u.mu.Unlock()
			_, msg := OpenAttachment(chosen)
			u.onStatus(msg)
			return
		}
		u.overlay = overlayNone
		u.mu.Unlock()
		return
	case overlayReply:
		if len(u.msgPicker) > 0 {
			idx := min(u.overlayIdx, len(u.msgPicker)-1)
			m := u.msgPicker[idx]
			u.replyTarget = &m
		}
		u.overlay = overlayNone
		u.mu.Unlock()
		return
	case overlayReact:
		if len(u.msgPicker) > 0 {
			idx := min(u.overlayIdx, len(u.msgPicker)-1)
			m := u.msgPicker[idx]
			emoji := u.pendingEmoji
			if emoji == "" {
				emoji = "👍"
			}
			u.pendingEmoji = ""
			u.overlay = overlayNone
			chat := u.activeChat
			u.mu.Unlock()
			if chat != nil {
				go u.sendReaction(*chat, m, emoji)
			}
			return
		}
		u.overlay = overlayNone
		u.mu.Unlock()
		return
	case overlayHelp, overlayFind:
		u.overlay = overlayNone
		u.mu.Unlock()
		return
	}

	line := strings.TrimSpace(string(u.input))
	u.input = nil
	u.cursor = 0
	u.historyIdx = -1
	if line != "" {
		u.history = append(u.history, line)
		if len(u.history) > 100 {
			u.history = u.history[len(u.history)-100:]
		}
	}
	u.mu.Unlock()

	if line == "" {
		return
	}
	u.executeLine(line)
}

func (u *AppUI) executeLine(line string) {
	if strings.HasPrefix(line, "/") {
		parts := strings.SplitN(line, " ", 2)
		cmd := strings.ToLower(parts[0])
		arg := ""
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}

		switch cmd {
		case "/quit", "/q", "/exit":
			u.mu.Lock()
			u.running = false
			u.mu.Unlock()
		case "/chats", "/c":
			u.mu.Lock()
			u.overlay = overlayChats
			u.overlayIdx = 0
			if arg != "" {
				u.overlayQuery = []rune(arg)
			} else {
				u.overlayQuery = nil
			}
			u.mu.Unlock()
		case "/unread", "/u":
			u.mu.Lock()
			u.overlay = overlayUnread
			u.overlayIdx = 0
			if arg != "" {
				u.overlayQuery = []rune(arg)
			} else {
				u.overlayQuery = nil
			}
			u.mu.Unlock()
		case "/help":
			u.mu.Lock()
			u.overlay = overlayHelp
			u.mu.Unlock()
		case "/refresh", "/r":
			u.onStatus("Syncing...")
			go u.syncMetadata()
		case "/open", "/o":
			if arg == "" {
				u.onStatus("Usage: /open <name>")
				return
			}
			chats := u.store.ListChats(arg)
			if len(chats) == 0 {
				u.onStatus("Chat not found: " + arg)
				return
			}
			chosen := chats[0]
			for _, c := range chats {
				if strings.EqualFold(c.Name, arg) {
					chosen = c
					break
				}
			}
			u.openChat(chosen)
		case "/reply":
			chat := u.getActiveChat()
			if chat == nil {
				u.onStatus("Select a chat first (/chats)")
				return
			}
			msgs := u.recentNonDeletedMessages(chat.ChatID, 15)
			if len(msgs) == 0 {
				u.onStatus("No messages to reply to")
				return
			}
			u.mu.Lock()
			u.msgPicker = msgs
			u.overlay = overlayReply
			u.overlayIdx = 0
			u.mu.Unlock()
		case "/react":
			chat := u.getActiveChat()
			if chat == nil {
				u.onStatus("Select a chat first (/chats)")
				return
			}
			msgs := u.recentNonDeletedMessages(chat.ChatID, 15)
			if len(msgs) == 0 {
				u.onStatus("No messages to react to")
				return
			}
			emoji := arg
			if emoji == "" {
				emoji = "👍"
			}
			u.mu.Lock()
			u.msgPicker = msgs
			u.pendingEmoji = emoji
			u.overlay = overlayReact
			u.overlayIdx = 0
			u.mu.Unlock()
		case "/edit":
			if arg == "" {
				u.onStatus("Usage: /edit <new text>")
				return
			}
			chat := u.getActiveChat()
			if chat == nil {
				u.onStatus("Select a chat first (/chats)")
				return
			}
			go u.editLastOutgoing(*chat, arg)
		case "/find", "/f":
			if arg == "" {
				u.onStatus("Usage: /find <query>")
				return
			}
			chat := u.getActiveChat()
			if chat == nil {
				u.onStatus("Select a chat first (/chats)")
				return
			}
			results := u.store.SearchMessages(chat.ChatID, arg)
			if len(results) == 0 {
				u.onStatus("No matches for: " + arg)
				return
			}
			// Reverse so newest match is first
			for l, r := 0, len(results)-1; l < r; l, r = l+1, r-1 {
				results[l], results[r] = results[r], results[l]
			}
			u.mu.Lock()
			u.findResults = results
			u.findQuery = arg
			u.overlay = overlayFind
			u.overlayIdx = 0
			u.mu.Unlock()
		case "/attach", "/a":
			if arg == "" {
				u.onStatus("Usage: /attach <file_path> [caption]")
				return
			}
			fpath, caption := splitAttachArg(arg)
			fpath = expandHome(fpath)
			info, err := os.Stat(fpath)
			if err != nil || info.IsDir() {
				u.onStatus("File not found: " + fpath)
				return
			}
			go u.sendPayload(caption, []string{fpath})
		case "/copy", "/cp", "/view":
			chat := u.getActiveChat()
			if chat == nil {
				u.onStatus("Select a chat first (/chats)")
				return
			}
			msgs := u.store.GetMessages(chat.ChatID, 100)
			var items []attItem
			for i := len(msgs) - 1; i >= 0; i-- {
				for _, att := range msgs[i].Attachments {
					if att.Path != "" {
						items = append(items, attItem{TS: msgs[i].Timestamp, Sender: msgs[i].SenderName, Att: att})
					}
				}
			}
			if len(items) == 0 {
				u.onStatus("No attachments in this chat")
				return
			}
			if strings.EqualFold(arg, "list") {
				u.mu.Lock()
				u.attItems = items
				if cmd == "/view" {
					u.overlay = overlayView
				} else {
					u.overlay = overlayCopy
				}
				u.overlayIdx = 0
				u.mu.Unlock()
				return
			}
			if cmd == "/view" {
				_, msg := OpenAttachment(items[0].Att)
				u.onStatus(msg)
			} else {
				_, msg := CopyAttachment(items[0].Att)
				u.onStatus(msg)
			}
		default:
			u.onStatus("Unknown command: " + cmd)
		}
		return
	}

	go u.sendPayload(line, nil)
}

func (u *AppUI) getActiveChat() *Chat {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.activeChat
}

func (u *AppUI) recentNonDeletedMessages(chatID string, limit int) []Message {
	msgs := u.store.GetMessages(chatID, 60)
	var out []Message
	for i := len(msgs) - 1; i >= 0 && len(out) < limit; i-- {
		if !msgs[i].Deleted && (msgs[i].Body != "" || len(msgs[i].Attachments) > 0) {
			out = append(out, msgs[i])
		}
	}
	return out
}

func (u *AppUI) sendReaction(chat Chat, target Message, emoji string) {
	recipient := chat.Number
	if recipient == "" {
		recipient = chat.ChatID
	}
	author := target.SenderID
	if author == "" {
		author = recipient
	}
	if num := u.store.ResolveNumber(author); num != "" {
		author = num
	}
	if !u.demoMode {
		if err := u.rpc.SendReaction(recipient, chat.GroupID, author, emoji, target.Timestamp, false); err != nil {
			u.onStatus(fmt.Sprintf("Reaction failed: %v", err))
			return
		}
	}
	u.store.AddReaction(chat.ChatID, target.Timestamp, emoji, "You", false)
	u.onStatus("Reacted " + emoji)
}

func (u *AppUI) editLastOutgoing(chat Chat, newBody string) {
	msgs := u.store.GetMessages(chat.ChatID, 50)
	var lastOut *Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].IsOutgoing && !msgs[i].Deleted && msgs[i].Timestamp > 0 {
			m := msgs[i]
			lastOut = &m
			break
		}
	}
	if lastOut == nil {
		u.onStatus("No sent message to edit")
		return
	}
	recipient := chat.Number
	if recipient == "" {
		recipient = chat.ChatID
	}
	if !u.demoMode {
		_, err := u.rpc.SendMessage(SendOptions{
			Text:          newBody,
			Recipient:     recipient,
			GroupID:       chat.GroupID,
			EditTimestamp: lastOut.Timestamp,
		})
		if err != nil {
			u.onStatus(fmt.Sprintf("Edit failed: %v", err))
			return
		}
	}
	u.store.EditMessage(chat.ChatID, lastOut.Timestamp, newBody)
	u.onStatus("Edited message")
}

func (u *AppUI) sendPayload(text string, attachments []string) {
	u.mu.Lock()
	chat := u.activeChat
	reply := u.replyTarget
	u.replyTarget = nil
	u.scrollOffset = 0
	u.mu.Unlock()

	if chat == nil {
		u.onStatus("Select a chat first (/chats)")
		return
	}

	recipient := chat.Number
	if recipient == "" {
		recipient = chat.ChatID
	}
	opts := SendOptions{
		Text:        text,
		Recipient:   recipient,
		GroupID:     chat.GroupID,
		Attachments: attachments,
	}
	var qAuthor, qText string
	if reply != nil {
		qa := reply.SenderID
		if num := u.store.ResolveNumber(qa); num != "" {
			qa = num
		}
		opts.QuoteTimestamp = reply.Timestamp
		opts.QuoteAuthor = qa
		opts.QuoteMessage = reply.Body
		qAuthor = reply.SenderName
		qText = reply.Body
	}

	var ts int64
	if u.demoMode {
		ts = time.Now().UnixMilli()
	} else {
		var err error
		ts, err = u.rpc.SendMessage(opts)
		if err != nil {
			u.onStatus(fmt.Sprintf("Send failed: %v", err))
			return
		}
	}

	var atts []Attachment
	for _, p := range attachments {
		ctype := "application/octet-stream"
		low := strings.ToLower(p)
		if strings.HasSuffix(low, ".png") {
			ctype = "image/png"
		} else if strings.HasSuffix(low, ".jpg") || strings.HasSuffix(low, ".jpeg") {
			ctype = "image/jpeg"
		} else if strings.HasSuffix(low, ".webp") {
			ctype = "image/webp"
		}
		atts = append(atts, Attachment{Name: filepath.Base(p), Path: p, ContentType: ctype})
	}

	preview := text
	if preview == "" && len(atts) > 0 {
		preview = "[" + atts[0].Name + "]"
	}
	u.store.UpsertChat(chat.ChatID, chat.ChatType, chat.Name, chat.Number, chat.GroupID, ts, preview, false)
	u.store.AddMessage(Message{
		ChatID:      chat.ChatID,
		SenderID:    u.account,
		SenderName:  "You",
		Body:        text,
		Timestamp:   ts,
		IsOutgoing:  true,
		Status:      "sent",
		QuoteAuthor: qAuthor,
		QuoteText:   qText,
		Attachments: atts,
	})
	u.mu.Lock()
	if len(atts) > 0 {
		u.statusText = "Sent: " + atts[0].Name
	} else {
		u.statusText = "Connected"
	}
	u.mu.Unlock()
	u.triggerRedraw()
}

func (u *AppUI) filteredOverlayChatsLocked() []Chat {
	all := u.store.ListChats(string(u.overlayQuery))
	if u.overlay == overlayUnread {
		var filtered []Chat
		for _, c := range all {
			if c.UnreadCount > 0 {
				filtered = append(filtered, c)
			}
		}
		return filtered
	}
	return all
}

// layoutInput wraps u.input across lines of terminal width and computes exact cursor coordinates.
func layoutInput(runes []rune, cursor int, cols int) (lines []string, cursorRow int, cursorCol int) {
	usable := max(10, cols-3) // "> " on first line, "  " on wrapped lines
	var cur []rune
	row := 0
	cRow, cCol := 0, 0
	foundCursor := false

	for idx := 0; idx <= len(runes); idx++ {
		if idx == cursor && !foundCursor {
			cRow = row
			cCol = 3 + len(cur) // 1-based column after "> " or "  "
			foundCursor = true
		}
		if idx == len(runes) {
			break
		}
		r := runes[idx]
		if r == '\n' {
			lines = append(lines, string(cur))
			cur = nil
			row++
			continue
		}
		cur = append(cur, r)
		if len(cur) >= usable {
			lines = append(lines, string(cur))
			cur = nil
			row++
		}
	}
	lines = append(lines, string(cur))
	if !foundCursor {
		cRow = len(lines) - 1
		cCol = 3 + len(cur)
	}
	if cCol > cols {
		cCol = cols
	}
	return lines, cRow, cCol
}

func (u *AppUI) render() {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || cols < 24 || rows < 8 {
		cols, rows = 80, 24
	}

	u.mu.Lock()
	chat := u.activeChat
	status := u.statusText
	typing := u.typingSender
	scrollOff := u.scrollOffset
	reply := u.replyTarget
	pendingEmoji := u.pendingEmoji
	overlay := u.overlay
	overlayIdx := u.overlayIdx
	overlayQ := string(u.overlayQuery)
	attList := u.attItems
	msgPick := u.msgPicker
	findRes := u.findResults
	findQ := u.findQuery
	inRunes := make([]rune, len(u.input))
	copy(inRunes, u.input)
	cursorPos := u.cursor
	u.mu.Unlock()

	totalUnread := u.store.TotalUnread()

	var sb strings.Builder
	sb.WriteString("\033[?2026h\033[H\033[J")

	// Prepare bottom overlays & autocomplete
	var bottomLines []string

	switch overlay {
	case overlayChats, overlayUnread:
		allChats := u.store.ListChats(overlayQ)
		if overlay == overlayUnread {
			var unread []Chat
			for _, c := range allChats {
				if c.UnreadCount > 0 {
					unread = append(unread, c)
				}
			}
			allChats = unread
		}
		title := "Chats"
		if overlay == overlayUnread {
			title = "Unread Chats"
		}
		bottomLines = append(bottomLines, fmt.Sprintf("  %s%s%s  %s(↑/↓ select · Enter open · Esc cancel · type to filter)%s", ansiBold, title, ansiReset, ansiDim, ansiReset))
		if overlayQ != "" {
			bottomLines = append(bottomLines, fmt.Sprintf("  %sFilter:%s %s", ansiDim, ansiReset, overlayQ))
		}
		if len(allChats) == 0 {
			bottomLines = append(bottomLines, fmt.Sprintf("  %sNo matching chats.%s", ansiDim, ansiReset))
		} else {
			maxShow := min(10, max(3, rows-8))
			idx := min(overlayIdx, len(allChats)-1)
			start := max(0, idx-maxShow/2)
			end := min(len(allChats), start+maxShow)
			if end-start < maxShow {
				start = max(0, end-maxShow)
			}
			for i := start; i < end; i++ {
				c := allChats[i]
				ptr := "    "
				nameStyle := ""
				if i == idx {
					ptr = "  ❯ "
					nameStyle = ansiBold + ansiCyan
				} else if c.UnreadCount > 0 {
					nameStyle = ansiBold
				}
				pfx := ""
				if c.ChatType == "group" {
					pfx = "#"
				}
				unreadStr := ""
				if c.UnreadCount > 0 {
					unreadStr = fmt.Sprintf(" %s(%d new)%s", ansiCyan, c.UnreadCount, ansiReset)
				}
				previewStr := ""
				if c.LastPreview != "" {
					previewStr = "  " + c.LastPreview
				}
				tsStr := ""
				if c.LastMessageTS > 0 {
					tsStr = " · " + formatTS(c.LastMessageTS)
				}
				bottomLines = append(bottomLines, fmt.Sprintf("%s%s%s%s%s%s%s%s%s",
					ptr, nameStyle, pfx, c.Name, ansiReset, unreadStr, ansiDim, previewStr+tsStr, ansiReset))
			}
		}
	case overlayCopy, overlayView:
		title := "Copy Attachment"
		actionHint := "Enter copy"
		if overlay == overlayView {
			title = "Open Attachment"
			actionHint = "Enter open"
		}
		bottomLines = append(bottomLines, fmt.Sprintf("  %s%s%s  %s(↑/↓ select · %s · Esc cancel)%s", ansiBold, title, ansiReset, ansiDim, actionHint, ansiReset))
		maxShow := 8
		idx := min(overlayIdx, len(attList)-1)
		start := max(0, idx-maxShow/2)
		end := min(len(attList), start+maxShow)
		for i := start; i < end; i++ {
			it := attList[i]
			ptr := "    "
			st := ""
			if i == idx {
				ptr = "  ❯ "
				st = ansiBold + ansiCyan
			}
			bottomLines = append(bottomLines, fmt.Sprintf("%s%s%s%s  %s· %s · %s%s", ptr, st, it.Att.Name, ansiReset, ansiDim, it.Sender, formatTS(it.TS), ansiReset))
		}
	case overlayReply, overlayReact:
		title := "Reply to Message"
		actionHint := "↑/↓ select · Enter quote · Esc cancel"
		if overlay == overlayReact {
			em := pendingEmoji
			if em == "" {
				em = "👍"
			}
			title = fmt.Sprintf("React %s to Message", em)
			actionHint = "↑/↓ select · 1:👍 2:❤️ 3:😂 4:😮 5:😢 6:🙏 · Enter send · Esc cancel"
		}
		bottomLines = append(bottomLines, fmt.Sprintf("  %s%s%s  %s(%s)%s", ansiBold, title, ansiReset, ansiDim, actionHint, ansiReset))
		maxShow := 8
		idx := min(overlayIdx, len(msgPick)-1)
		start := max(0, idx-maxShow/2)
		end := min(len(msgPick), start+maxShow)
		for i := start; i < end; i++ {
			m := msgPick[i]
			ptr := "    "
			st := ""
			if i == idx {
				ptr = "  ❯ "
				st = ansiBold + ansiCyan
			}
			snip := strings.ReplaceAll(m.Body, "\n", " ")
			if snip == "" && len(m.Attachments) > 0 {
				snip = "[" + m.Attachments[0].Name + "]"
			}
			if len([]rune(snip)) > max(20, cols-28) {
				r := []rune(snip)
				snip = string(r[:max(15, cols-30)]) + "…"
			}
			bottomLines = append(bottomLines, fmt.Sprintf("%s%s%s%s: %s  %s%s%s", ptr, st, m.SenderName, ansiReset, snip, ansiDim, formatTS(m.Timestamp), ansiReset))
		}
	case overlayFind:
		bottomLines = append(bottomLines, fmt.Sprintf("  %sSearch: %q%s  %s(%d found · ↑/↓ · Esc close)%s", ansiBold, findQ, ansiReset, ansiDim, len(findRes), ansiReset))
		maxShow := 8
		idx := min(overlayIdx, len(findRes)-1)
		start := max(0, idx-maxShow/2)
		end := min(len(findRes), start+maxShow)
		for i := start; i < end; i++ {
			m := findRes[i]
			ptr := "    "
			st := ""
			if i == idx {
				ptr = "  ❯ "
				st = ansiBold + ansiCyan
			}
			snip := strings.ReplaceAll(m.Body, "\n", " ")
			if len([]rune(snip)) > max(20, cols-28) {
				r := []rune(snip)
				snip = string(r[:max(15, cols-30)]) + "…"
			}
			bottomLines = append(bottomLines, fmt.Sprintf("%s%s%s%s: %s  %s%s%s", ptr, st, m.SenderName, ansiReset, snip, ansiDim, formatTS(m.Timestamp), ansiReset))
		}
	case overlayHelp:
		bottomLines = append(bottomLines, fmt.Sprintf("  %sHelp%s  %s(Esc / Enter to close)%s", ansiBold, ansiReset, ansiDim, ansiReset))
		for _, sc := range slashCommands {
			bottomLines = append(bottomLines, fmt.Sprintf("  %s%-10s%s %s%s%s", ansiBold, sc.Name, ansiReset, ansiDim, sc.Desc, ansiReset))
		}
		bottomLines = append(bottomLines, fmt.Sprintf("  %sCtrl+O chats · PgUp/PgDn scroll · Ctrl+V paste · Ctrl+W del word · Ctrl+C quit%s", ansiDim, ansiReset))
	default:
		inStr := string(inRunes)
		if strings.HasPrefix(inStr, "/") && !strings.Contains(inStr, " ") {
			low := strings.ToLower(inStr)
			for _, sc := range slashCommands {
				if strings.HasPrefix(sc.Name, low) {
					bottomLines = append(bottomLines, fmt.Sprintf("  %s%-10s%s %s%s%s", ansiBold, sc.Name, ansiReset, ansiDim, sc.Desc, ansiReset))
				}
			}
		}
	}

	if reply != nil {
		snip := strings.ReplaceAll(reply.Body, "\n", " ")
		if snip == "" && len(reply.Attachments) > 0 {
			snip = "[" + reply.Attachments[0].Name + "]"
		}
		if len([]rune(snip)) > max(20, cols-26) {
			r := []rune(snip)
			snip = string(r[:max(15, cols-28)]) + "…"
		}
		bottomLines = append(bottomLines, fmt.Sprintf("  %s╭ Replying to %s: %s  (Esc cancel)%s", ansiCyan, reply.SenderName, snip, ansiReset))
	}

	inLines, cRowOff, cCol := layoutInput(inRunes, cursorPos, cols)
	divider := ansiDim + strings.Repeat("─", cols) + ansiReset

	who := "No active chat (/chats)"
	if chat != nil {
		pfx := ""
		if chat.ChatType == "group" {
			pfx = "#"
		}
		who = pfx + chat.Name
		if chat.Number != "" {
			who += " (" + chat.Number + ")"
		}
	}
	unreadBadge := ""
	if totalUnread > 0 {
		unreadBadge = fmt.Sprintf("  ·  %s%d unread%s", ansiCyan+ansiBold, totalUnread, ansiReset)
	}
	typingPart := ""
	if typing != "" {
		typingPart = fmt.Sprintf("  ·  %s%s is typing...%s", ansiCyan, typing, ansiReset)
	}
	hintPart := ""
	if typingPart == "" && unreadBadge == "" {
		hintPart = fmt.Sprintf("  ·  %sCtrl+O chats  /help%s", ansiDim, ansiReset)
	}
	statusLine := fmt.Sprintf("  %s●%s %s%s%s  ·  %s%s%s%s%s%s",
		ansiGreen, ansiReset, ansiDim, status, ansiReset, ansiBold, who, ansiReset, unreadBadge, typingPart, hintPart)

	// Calculate viewport height for bottom-anchored messages
	bottomChrome := len(bottomLines) + 1 + len(inLines) + 1 + 1 // overlay + div + input + div + status
	viewportH := rows - 1 - bottomChrome
	if viewportH < 1 {
		viewportH = 1
	}

	var allMsgLines []string
	if chat != nil {
		msgs := u.store.GetMessages(chat.ChatID, 300)
		allMsgLines = u.buildMessageLines(msgs, cols)
	} else {
		allMsgLines = append(allMsgLines, fmt.Sprintf("  %sPress Ctrl+O (or type /chats) to open a conversation, /help for commands.%s", ansiDim, ansiReset))
		allMsgLines = append(allMsgLines, "")
	}

	maxScroll := max(0, len(allMsgLines)-viewportH)
	if scrollOff > maxScroll {
		scrollOff = maxScroll
		u.mu.Lock()
		u.scrollOffset = scrollOff
		u.mu.Unlock()
	}

	endIdx := len(allMsgLines) - scrollOff
	startIdx := max(0, endIdx-viewportH)
	visible := allMsgLines[startIdx:endIdx]

	// Row 1: Header (with scroll status indicator if scrolled)
	scrollNote := ""
	if scrollOff > 0 {
		scrollNote = fmt.Sprintf("  %s↑ scrolled +%d (PgDn / Esc)%s", ansiYellow, scrollOff, ansiReset)
	} else if startIdx > 0 {
		scrollNote = fmt.Sprintf("  %s↑ %d more (PgUp)%s", ansiDim, startIdx, ansiReset)
	}
	if chat != nil {
		prefix := ""
		if chat.ChatType == "group" {
			prefix = "#"
		}
		extra := ""
		if chat.Number != "" {
			extra = " (" + chat.Number + ")"
		}
		sb.WriteString(fmt.Sprintf("  %s%s%s%s%s%s%s\r\n", ansiBold, prefix, chat.Name, ansiReset, ansiDim, extra+ansiReset, scrollNote))
	} else {
		sb.WriteString(fmt.Sprintf("  %ssignal%s %s· %s%s\r\n", ansiBold, ansiReset, ansiDim, u.account, ansiReset))
	}

	// Pad empty lines at TOP so messages anchor to the bottom!
	padTop := viewportH - len(visible)
	for i := 0; i < padTop; i++ {
		sb.WriteString("\r\n")
	}
	for _, ml := range visible {
		sb.WriteString(ml)
		sb.WriteString("\r\n")
	}

	for _, bl := range bottomLines {
		sb.WriteString(bl)
		sb.WriteString("\r\n")
	}

	sb.WriteString(divider + "\r\n")
	firstInputRow := rows - 1 - len(inLines) // rows = status, rows-1 = bottom div
	for idx, il := range inLines {
		if idx == 0 {
			sb.WriteString(fmt.Sprintf("%s>%s %s\r\n", ansiBold, ansiReset, il))
		} else {
			sb.WriteString("  " + il + "\r\n")
		}
	}
	sb.WriteString(divider + "\r\n")
	sb.WriteString(statusLine)

	targetRow := firstInputRow + cRowOff
	sb.WriteString(fmt.Sprintf("\033[%d;%dH\033[?25h\033[?2026l", targetRow, cCol))
	os.Stdout.WriteString(sb.String())
}

func (u *AppUI) buildMessageLines(msgs []Message, cols int) []string {
	var lines []string
	lastDay := ""
	prevSender := ""
	var prevTS int64

	for _, m := range msgs {
		day := formatDay(m.Timestamp)
		if day != "" && day != lastDay {
			lines = append(lines, fmt.Sprintf("  %s── %s ──%s", ansiDim, day, ansiReset))
			lastDay = day
			prevSender = ""
		}

		// Group consecutive incoming messages from the same sender within 2 minutes
		grouped := !m.IsOutgoing && m.SenderID == prevSender && m.Timestamp-prevTS < 2*60*1000 && m.QuoteText == ""
		lines = append(lines, u.formatMessageLines(m, cols, grouped)...)
		prevSender = m.SenderID
		if m.IsOutgoing {
			prevSender = ""
		}
		prevTS = m.Timestamp
	}
	return lines
}

func (u *AppUI) formatMessageLines(m Message, cols int, grouped bool) []string {
	var lines []string
	ts := formatTS(m.Timestamp)

	if m.Deleted {
		if m.IsOutgoing {
			lines = append(lines, fmt.Sprintf("%s> (message deleted)  %s%s", ansiDim, ts, ansiReset))
		} else {
			lines = append(lines, fmt.Sprintf("  %s%s: (message deleted)  %s%s", ansiDim, m.SenderName, ts, ansiReset))
		}
		lines = append(lines, "")
		return lines
	}

	if m.QuoteText != "" {
		qa := u.store.ResolveContactName(m.QuoteAuthor)
		qt := strings.ReplaceAll(m.QuoteText, "\n", " ")
		if len([]rune(qt)) > cols-16 && cols > 20 {
			r := []rune(qt)
			qt = string(r[:cols-18]) + "…"
		}
		lines = append(lines, fmt.Sprintf("  %s╭ %s: %s%s", ansiDim, qa, qt, ansiReset))
	}

	body := m.Body
	if len(m.Attachments) > 0 {
		var names []string
		for _, a := range m.Attachments {
			names = append(names, a.Name)
		}
		hint := ""
		if !m.IsOutgoing {
			hint = " (/copy · /view)"
		}
		attTag := "[" + strings.Join(names, ", ") + hint + "]"
		if body == "" {
			body = attTag
		} else {
			body = body + " " + attTag
		}
	}
	receiptStr := ""
	if m.IsOutgoing {
		switch m.Status {
		case "read":
			receiptStr = fmt.Sprintf(" %s✓✓ read%s", ansiCyan, ansiReset)
		case "delivered":
			receiptStr = fmt.Sprintf(" %s✓✓%s", ansiDim, ansiReset)
		default:
			receiptStr = fmt.Sprintf(" %s✓%s", ansiDim, ansiReset)
		}
	}

	wrapped := wrapText(body, max(20, cols-6))
	if m.Edited {
		if len(wrapped) == 0 {
			wrapped = []string{ansiDim + "(edited)" + ansiReset}
		} else {
			wrapped[len(wrapped)-1] += " " + ansiDim + "(edited)" + ansiReset
		}
	}
	if m.IsOutgoing {
		first := ""
		if len(wrapped) > 0 {
			first = wrapped[0]
		}
		lines = append(lines, fmt.Sprintf("%s>%s %s  %s%s%s%s", ansiDim, ansiReset, first, ansiDim, ts, ansiReset, receiptStr))
		for _, extra := range wrapped[1:] {
			lines = append(lines, "  "+extra)
		}
	} else {
		if !grouped {
			lines = append(lines, fmt.Sprintf("%s✦%s %s%s%s %s· %s%s", ansiCyan, ansiReset, ansiBold, m.SenderName, ansiReset, ansiDim, ts, ansiReset))
		}
		for _, wl := range wrapped {
			lines = append(lines, "  "+wl)
		}
	}

	if len(m.Reactions) > 0 {
		var rparts []string
		for e, users := range m.Reactions {
			rparts = append(rparts, fmt.Sprintf("%s×%d", e, len(users)))
		}
		lines = append(lines, fmt.Sprintf("  %s(%s)%s", ansiDim, strings.Join(rparts, " "), ansiReset))
	}

	if !grouped {
		lines = append(lines, "")
	} else {
		// Slight breathing room if there were reactions or attachments, else compact
		if len(m.Reactions) > 0 || len(m.Attachments) > 0 {
			lines = append(lines, "")
		}
	}
	return lines
}

func wrapText(text string, width int) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		runes := []rune(para)
		if len(runes) == 0 {
			out = append(out, "")
			continue
		}
		for len(runes) > width {
			cut := width
			for i := width; i > width/2; i-- {
				if runes[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, string(runes[:cut]))
			runes = []rune(strings.TrimLeft(string(runes[cut:]), " "))
		}
		out = append(out, string(runes))
	}
	return out
}

func formatDay(tsMs int64) string {
	if tsMs == 0 {
		return ""
	}
	t := time.UnixMilli(tsMs)
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return "Today"
	}
	yest := now.AddDate(0, 0, -1)
	if t.Year() == yest.Year() && t.YearDay() == yest.YearDay() {
		return "Yesterday"
	}
	return t.Format("02 Jan 2006")
}

func formatTS(tsMs int64) string {
	if tsMs == 0 {
		return ""
	}
	t := time.UnixMilli(tsMs)
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("02.01 15:04")
}

func splitAttachArg(arg string) (string, string) {
	if strings.HasPrefix(arg, "\"") || strings.HasPrefix(arg, "'") {
		q := arg[0]
		if end := strings.IndexByte(arg[1:], q); end >= 0 {
			return arg[1 : 1+end], strings.TrimSpace(arg[2+end:])
		}
	}
	parts := strings.SplitN(arg, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
