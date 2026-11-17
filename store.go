package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type Attachment struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
}

type Message struct {
	ChatID      string              `json:"chat_id"`
	SenderID    string              `json:"sender_id"`
	SenderName  string              `json:"sender_name"`
	Body        string              `json:"body"`
	Timestamp   int64               `json:"timestamp"`
	IsOutgoing  bool                `json:"is_outgoing"`
	Status      string              `json:"status,omitempty"` // "", "sent", "delivered", "read"
	Edited      bool                `json:"edited,omitempty"`
	Deleted     bool                `json:"deleted,omitempty"`
	QuoteAuthor string              `json:"quote_author,omitempty"`
	QuoteText   string              `json:"quote_text,omitempty"`
	Attachments []Attachment        `json:"attachments,omitempty"`
	Reactions   map[string][]string `json:"reactions,omitempty"`
}

type Chat struct {
	ChatID        string `json:"chat_id"`
	ChatType      string `json:"chat_type"` // "dm" | "group"
	Name          string `json:"name"`
	Number        string `json:"number,omitempty"`
	GroupID       string `json:"group_id,omitempty"`
	LastMessageTS int64  `json:"last_message_ts"`
	LastPreview   string `json:"last_preview,omitempty"`
	UnreadCount   int    `json:"unread_count"`
}

type Contact struct {
	Key         string `json:"key"`
	UUID        string `json:"uuid,omitempty"`
	Number      string `json:"number,omitempty"`
	Name        string `json:"name,omitempty"`
	ProfileName string `json:"profile_name,omitempty"`
}

type persistedState struct {
	LastChatID string               `json:"last_chat_id,omitempty"`
	Contacts   map[string]Contact   `json:"contacts"`
	Chats      map[string]Chat      `json:"chats"`
	Messages   map[string][]Message `json:"messages"`
}

type Store struct {
	mu         sync.RWMutex
	path       string
	lastChatID string
	contacts   map[string]Contact
	chats      map[string]Chat
	messages   map[string][]Message
}

func NewStore(customPath ...string) (*Store, error) {
	p := ""
	if len(customPath) > 0 && customPath[0] != "" {
		p = customPath[0]
	} else {
		var dir string
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			dir = filepath.Join(localAppData, "signal-term")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			dir = filepath.Join(home, ".local", "share", "signal-term")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		p = filepath.Join(dir, "state.json")
	}

	s := &Store{
		path:     p,
		contacts: make(map[string]Contact),
		chats:    make(map[string]Chat),
		messages: make(map[string][]Message),
	}
	s.load()
	return s, nil
}

func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	s.lastChatID = st.LastChatID
	if st.Contacts != nil {
		s.contacts = st.Contacts
	}
	if st.Chats != nil {
		s.chats = st.Chats
	}
	if st.Messages != nil {
		s.messages = st.Messages
	}
}

func (s *Store) saveLocked() {
	st := persistedState{
		LastChatID: s.lastChatID,
		Contacts:   s.contacts,
		Chats:      s.chats,
		Messages:   s.messages,
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func (s *Store) SetLastChat(chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastChatID != chatID {
		s.lastChatID = chatID
		s.saveLocked()
	}
}

func (s *Store) GetLastChat() (Chat, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.lastChatID != "" {
		if c, ok := s.chats[s.lastChatID]; ok {
			return c, true
		}
	}

	var best Chat
	found := false
	for _, c := range s.chats {
		if c.LastMessageTS > 0 && (!found || c.LastMessageTS > best.LastMessageTS) {
			best = c
			found = true
		}
	}
	return best, found
}

// canonicalChatIDLocked finds an existing DM chat matching either uuid or number
// to avoid splitting one contact into two separate chats.
func (s *Store) canonicalChatIDLocked(uuid, number string) string {
	if uuid != "" {
		if _, ok := s.chats[uuid]; ok {
			return uuid
		}
	}
	if number != "" {
		if _, ok := s.chats[number]; ok {
			// If we now know the UUID, prefer merging number chat into uuid?
			// Keeping existing ID avoids moving messages unnecessarily, or check contact:
			return number
		}
	}
	for _, c := range s.contacts {
		if (uuid != "" && c.UUID == uuid) || (number != "" && c.Number == number) {
			if c.UUID != "" {
				if _, ok := s.chats[c.UUID]; ok {
					return c.UUID
				}
			}
			if c.Number != "" {
				if _, ok := s.chats[c.Number]; ok {
					return c.Number
				}
			}
		}
	}
	if uuid != "" {
		return uuid
	}
	return number
}

func (s *Store) CanonicalChatID(uuid, number string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.canonicalChatIDLocked(uuid, number)
}

func (s *Store) upsertContactLocked(uuid, number, name, profileName string) {
	key := ""
	for k, existing := range s.contacts {
		if (uuid != "" && existing.UUID == uuid) || (number != "" && existing.Number == number) {
			key = k
			break
		}
	}
	if key == "" {
		key = uuid
		if key == "" {
			key = number
		}
	}
	if key == "" {
		return
	}

	c := s.contacts[key]
	c.Key = key
	if uuid != "" {
		c.UUID = uuid
	}
	if number != "" {
		c.Number = number
	}
	if name != "" {
		c.Name = name
	}
	if profileName != "" {
		c.ProfileName = profileName
	}
	s.contacts[key] = c
}

func (s *Store) upsertChatLocked(chatID, chatType, name, number, groupID string, lastTS int64, preview string, incUnread bool) {
	if chatID == "" {
		return
	}
	c, exists := s.chats[chatID]
	if !exists {
		if name == "" {
			name = chatID
		}
		unread := 0
		if incUnread {
			unread = 1
		}
		s.chats[chatID] = Chat{
			ChatID:        chatID,
			ChatType:      chatType,
			Name:          name,
			Number:        number,
			GroupID:       groupID,
			LastMessageTS: lastTS,
			LastPreview:   preview,
			UnreadCount:   unread,
		}
		return
	}
	c.ChatType = chatType
	if name != "" && name != chatID {
		c.Name = name
	}
	if number != "" {
		c.Number = number
	}
	if groupID != "" {
		c.GroupID = groupID
	}
	if lastTS >= c.LastMessageTS {
		if lastTS > c.LastMessageTS {
			c.LastMessageTS = lastTS
		}
		if preview != "" {
			c.LastPreview = preview
		}
	}
	if incUnread {
		c.UnreadCount++
	}
	s.chats[chatID] = c
}

// SyncMetadata applies all contacts and groups in memory and saves state.json once.
func (s *Store) SyncMetadata(contacts []Contact, chats []Chat) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range contacts {
		s.upsertContactLocked(c.UUID, c.Number, c.Name, c.ProfileName)
	}
	for _, ch := range chats {
		cid := ch.ChatID
		if ch.ChatType == "dm" {
			// Resolve canonical chat ID so UUID/number duplicates don't occur
			resolved := s.canonicalChatIDLocked(ch.ChatID, ch.Number)
			if resolved != "" {
				cid = resolved
			}
		}
		s.upsertChatLocked(cid, ch.ChatType, ch.Name, ch.Number, ch.GroupID, ch.LastMessageTS, ch.LastPreview, false)
	}
	s.saveLocked()
}

func (s *Store) ResolveContactName(identifier string) string {
	if identifier == "" {
		return "Unknown"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, c := range s.contacts {
		if c.Key == identifier || c.UUID == identifier || c.Number == identifier {
			if c.Name != "" {
				return c.Name
			}
			if c.ProfileName != "" {
				return c.ProfileName
			}
			if c.Number != "" {
				return c.Number
			}
			if c.UUID != "" {
				return c.UUID
			}
		}
	}
	return identifier
}

func (s *Store) ResolveNumber(identifier string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.contacts {
		if c.Key == identifier || c.UUID == identifier || c.Number == identifier {
			if c.Number != "" {
				return c.Number
			}
		}
	}
	return ""
}

func (s *Store) UpsertChat(chatID, chatType, name, number, groupID string, lastTS int64, preview string, incUnread bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if chatType == "dm" {
		if canon := s.canonicalChatIDLocked(chatID, number); canon != "" {
			chatID = canon
		}
	}
	s.upsertChatLocked(chatID, chatType, name, number, groupID, lastTS, preview, incUnread)
	s.saveLocked()
	return chatID
}

func (s *Store) MarkChatRead(chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.chats[chatID]; ok && c.UnreadCount > 0 {
		c.UnreadCount = 0
		s.chats[chatID] = c
		s.saveLocked()
	}
}

func (s *Store) TotalUnread() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, c := range s.chats {
		total += c.UnreadCount
	}
	return total
}

func chatMatchRank(c Chat, q string) int {
	name := strings.ToLower(c.Name)
	if name == q {
		return 0
	}
	if strings.HasPrefix(name, q) {
		return 1
	}
	for _, word := range strings.Fields(name) {
		if strings.HasPrefix(word, q) {
			return 2
		}
	}
	if strings.Contains(name, q) {
		return 3
	}
	if strings.Contains(strings.ToLower(c.Number), q) {
		return 4
	}
	return 5
}

func (s *Store) ListChats(query string) []Chat {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]Chat, 0, len(s.chats))
	for _, c := range s.chats {
		if q == "" || strings.Contains(strings.ToLower(c.Name), q) || strings.Contains(strings.ToLower(c.Number), q) || strings.Contains(strings.ToLower(c.LastPreview), q) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if q != "" {
			ri, rj := chatMatchRank(out[i], q), chatMatchRank(out[j], q)
			if ri != rj {
				return ri < rj
			}
		}
		// Unread chats first, then recent activity, then alphabetical
		if (out[i].UnreadCount > 0) != (out[j].UnreadCount > 0) {
			return out[i].UnreadCount > 0
		}
		if out[i].LastMessageTS != out[j].LastMessageTS {
			return out[i].LastMessageTS > out[j].LastMessageTS
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (s *Store) GetChat(chatID string) (Chat, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.chats[chatID]
	return c, ok
}

func (s *Store) AddMessage(msg Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	list := s.messages[msg.ChatID]
	for _, m := range list {
		if m.Timestamp == msg.Timestamp && m.SenderID == msg.SenderID && m.Body == msg.Body {
			return false
		}
	}
	list = append(list, msg)
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].Timestamp < list[j].Timestamp
	})
	if len(list) > 500 {
		list = list[len(list)-500:]
	}
	s.messages[msg.ChatID] = list

	if c, ok := s.chats[msg.ChatID]; ok {
		if msg.Timestamp >= c.LastMessageTS {
			c.LastMessageTS = msg.Timestamp
			c.LastPreview = messagePreview(msg)
			s.chats[msg.ChatID] = c
		}
	}
	s.saveLocked()
	return true
}

func messagePreview(msg Message) string {
	if msg.Deleted {
		return "(deleted)"
	}
	if msg.Body != "" {
		p := strings.ReplaceAll(msg.Body, "\n", " ")
		runes := []rune(p)
		if len(runes) > 36 {
			return string(runes[:35]) + "…"
		}
		return p
	}
	if len(msg.Attachments) > 0 {
		return "[" + msg.Attachments[0].Name + "]"
	}
	return ""
}

func (s *Store) GetMessages(chatID string, limit int) []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := s.messages[chatID]
	if len(list) == 0 {
		return nil
	}
	if limit > 0 && len(list) > limit {
		list = list[len(list)-limit:]
	}
	cp := make([]Message, len(list))
	copy(cp, list)
	return cp
}

func (s *Store) SearchMessages(chatID, query string) []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var out []Message
	for _, m := range s.messages[chatID] {
		if m.Deleted {
			continue
		}
		if strings.Contains(strings.ToLower(m.Body), q) || strings.Contains(strings.ToLower(m.SenderName), q) {
			out = append(out, m)
		}
	}
	return out
}

func (s *Store) EditMessage(chatID string, targetTS int64, newBody string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	list := s.messages[chatID]
	for i := range list {
		if list[i].Timestamp == targetTS {
			list[i].Body = newBody
			list[i].Edited = true
			s.messages[chatID] = list
			if i == len(list)-1 {
				if c, ok := s.chats[chatID]; ok {
					c.LastPreview = messagePreview(list[i])
					s.chats[chatID] = c
				}
			}
			s.saveLocked()
			return true
		}
	}
	return false
}

func (s *Store) DeleteMessage(chatID string, targetTS int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	list := s.messages[chatID]
	for i := range list {
		if list[i].Timestamp == targetTS {
			list[i].Deleted = true
			list[i].Body = ""
			list[i].Attachments = nil
			s.messages[chatID] = list
			if i == len(list)-1 {
				if c, ok := s.chats[chatID]; ok {
					c.LastPreview = "(deleted)"
					s.chats[chatID] = c
				}
			}
			s.saveLocked()
			return true
		}
	}
	return false
}

func (s *Store) UpdateReceipt(senderID string, timestamps []int64, newStatus string) bool {
	if len(timestamps) == 0 {
		return false
	}
	tsSet := make(map[int64]bool, len(timestamps))
	for _, t := range timestamps {
		tsSet[t] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Collect all known identifiers (UUID + number) for senderID
	aliases := map[string]bool{senderID: true}
	for _, c := range s.contacts {
		if c.Key == senderID || c.UUID == senderID || c.Number == senderID {
			if c.Key != "" {
				aliases[c.Key] = true
			}
			if c.UUID != "" {
				aliases[c.UUID] = true
			}
			if c.Number != "" {
				aliases[c.Number] = true
			}
		}
	}

	changed := false
	for chatID, list := range s.messages {
		if !aliases[chatID] && !strings.HasPrefix(chatID, "group:") {
			continue
		}
		for i := range list {
			m := &list[i]
			if !m.IsOutgoing || !tsSet[m.Timestamp] {
				continue
			}
			if newStatus == "read" || (newStatus == "delivered" && m.Status != "read") {
				if m.Status != newStatus {
					m.Status = newStatus
					changed = true
				}
			}
		}
		s.messages[chatID] = list
	}
	if changed {
		s.saveLocked()
	}
	return changed
}

func (s *Store) AddReaction(chatID string, targetTS int64, emoji, sender string, remove bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	list := s.messages[chatID]
	for i := range list {
		if list[i].Timestamp == targetTS {
			if list[i].Reactions == nil {
				list[i].Reactions = make(map[string][]string)
			}
			users := list[i].Reactions[emoji]
			if remove {
				filtered := users[:0]
				for _, u := range users {
					if u != sender {
						filtered = append(filtered, u)
					}
				}
				if len(filtered) == 0 {
					delete(list[i].Reactions, emoji)
				} else {
					list[i].Reactions[emoji] = filtered
				}
			} else {
				found := false
				for _, u := range users {
					if u == sender {
						found = true
						break
					}
				}
				if !found {
					list[i].Reactions[emoji] = append(users, sender)
				}
			}
			s.messages[chatID] = list
			s.saveLocked()
			return true
		}
	}
	return false
}
