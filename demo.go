package main

import (
	"os"
	"path/filepath"
	"time"
)

func runDemoStore() (*Store, func(), error) {
	tmpDir, err := os.MkdirTemp("", "signal-term-demo-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	store, err := NewStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	now := time.Now().UnixMilli()
	min := int64(60 * 1000)

	store.SyncMetadata([]Contact{
		{UUID: "demo-alice", Number: "+1 555 010 1001", Name: "Alice Vance"},
		{UUID: "demo-bob", Number: "+1 555 010 1002", Name: "Bob Chen"},
		{UUID: "demo-emma", Number: "+1 555 010 1003", Name: "Emma Novak"},
		{UUID: "demo-marcus", Number: "+1 555 010 1004", Name: "Marcus Reed"},
	}, nil)

	// Active chat: Alice Vance
	store.UpsertChat("demo-alice", "dm", "Alice Vance", "+1 555 010 1001", "", now-2*min, "Just pushed the release build — looks super clean!", false)
	store.AddMessage(Message{
		ChatID:     "demo-alice",
		Timestamp:  now - 14*min,
		SenderID:   "demo-alice",
		SenderName: "Alice Vance",
		Body:       "Hey! Did you get a chance to test the new terminal client?",
		IsOutgoing: false,
	})
	store.AddMessage(Message{
		ChatID:     "demo-alice",
		Timestamp:  now - 11*min,
		SenderID:   "You",
		SenderName: "You",
		Body:       "Yes, running it right now. Super fast and zero clutter.",
		IsOutgoing: true,
		Status:     "read",
	})
	store.AddReaction("demo-alice", now-11*min, "🔥", "Alice Vance", false)
	store.AddMessage(Message{
		ChatID:      "demo-alice",
		Timestamp:   now - 7*min,
		SenderID:    "demo-alice",
		SenderName:  "Alice Vance",
		Body:        "Nice! Try /chats or Ctrl+O to switch threads, or /react to drop an emoji.",
		IsOutgoing:  false,
		QuoteAuthor: "You",
		QuoteText:   "Yes, running it right now. Super fast and zero clutter.",
	})
	store.AddMessage(Message{
		ChatID:     "demo-alice",
		Timestamp:  now - 4*min,
		SenderID:   "You",
		SenderName: "You",
		Body:       "Already hooked it up with /copy and /view for screenshots too.",
		IsOutgoing: true,
		Status:     "read",
	})
	store.AddMessage(Message{
		ChatID:     "demo-alice",
		Timestamp:  now - 2*min,
		SenderID:   "demo-alice",
		SenderName: "Alice Vance",
		Body:       "Just pushed the release build — looks super clean!",
		IsOutgoing: false,
	})

	// Other chats for /chats selector
	store.UpsertChat("demo-group-core", "group", "Core Dev Team", "", "demo-group-core", now-18*min, "Bob: Windows & macOS binaries are green", true)
	store.AddMessage(Message{
		ChatID:     "demo-group-core",
		Timestamp:  now - 18*min,
		SenderID:   "demo-bob",
		SenderName: "Bob Chen",
		Body:       "Windows & macOS binaries are green",
		IsOutgoing: false,
	})

	store.UpsertChat("demo-bob", "dm", "Bob Chen", "+1 555 010 1002", "", now-45*min, "Let's sync after standup", false)
	store.UpsertChat("demo-emma", "dm", "Emma Novak", "+1 555 010 1003", "", now-120*min, "Sent the updated config", true)
	store.UpsertChat("demo-marcus", "dm", "Marcus Reed", "+1 555 010 1004", "", now-300*min, "See you tomorrow!", false)

	store.SetLastChat("demo-alice")
	return store, cleanup, nil
}
