package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreCanonicalSyncReceiptsAndMessages(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.json")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	// Sync metadata in one batch
	s.SyncMetadata(
		[]Contact{
			{UUID: "uuid-alice", Number: "+48111222333", Name: "Alice"},
			{UUID: "uuid-bob", Number: "+48999888777", ProfileName: "Bob"},
		},
		[]Chat{
			{ChatID: "uuid-alice", ChatType: "dm", Name: "Alice", Number: "+48111222333"},
			{ChatID: "uuid-bob", ChatType: "dm", Name: "Bob", Number: "+48999888777"},
		},
	)

	// CanonicalChatID should resolve both UUID and phone number to the same chat
	if got := s.CanonicalChatID("", "+48111222333"); got != "uuid-alice" {
		t.Fatalf("expected canonical chat id uuid-alice, got %q", got)
	}
	if got := s.ResolveContactName("+48999888777"); got != "Bob" {
		t.Fatalf("expected Bob, got %q", got)
	}

	// Add messages out of order and verify chronological sort + dedup
	ok1 := s.AddMessage(Message{ChatID: "uuid-alice", SenderID: "me", SenderName: "You", Body: "Second", Timestamp: 2000, IsOutgoing: true, Status: "sent"})
	ok2 := s.AddMessage(Message{ChatID: "uuid-alice", SenderID: "uuid-alice", SenderName: "Alice", Body: "First", Timestamp: 1000, IsOutgoing: false})
	okDup := s.AddMessage(Message{ChatID: "uuid-alice", SenderID: "me", SenderName: "You", Body: "Second", Timestamp: 2000, IsOutgoing: true, Status: "sent"})
	if !ok1 || !ok2 || okDup {
		t.Fatalf("unexpected insert results: ok1=%v ok2=%v okDup=%v", ok1, ok2, okDup)
	}

	msgs := s.GetMessages("uuid-alice", 10)
	if len(msgs) != 2 || msgs[0].Body != "First" || msgs[1].Body != "Second" {
		t.Fatalf("expected chronological order [First, Second], got %+v", msgs)
	}

	// Receipt sent via phone number should update chat keyed by UUID!
	if !s.UpdateReceipt("+48111222333", []int64{2000}, "delivered") {
		t.Fatal("expected delivered receipt to match via phone number alias")
	}
	if !s.UpdateReceipt("+48111222333", []int64{2000}, "read") {
		t.Fatal("expected read receipt to match via phone number alias")
	}
	msgs = s.GetMessages("uuid-alice", 10)
	if msgs[1].Status != "read" {
		t.Fatalf("expected status 'read', got %q", msgs[1].Status)
	}

	// Delivery receipt after read must NOT downgrade "read" back to "delivered"
	if s.UpdateReceipt("uuid-alice", []int64{2000}, "delivered") {
		t.Fatal("read status should not downgrade to delivered")
	}

	// Edit, search, reaction, delete
	if !s.EditMessage("uuid-alice", 2000, "Second edited") {
		t.Fatal("EditMessage failed")
	}
	found := s.SearchMessages("uuid-alice", "edited")
	if len(found) != 1 || !found[0].Edited {
		t.Fatalf("expected 1 edited match, got %+v", found)
	}
	if !s.AddReaction("uuid-alice", 1000, "👍", "You", false) {
		t.Fatal("AddReaction failed")
	}
	if !s.DeleteMessage("uuid-alice", 1000) {
		t.Fatal("DeleteMessage failed")
	}
	msgs = s.GetMessages("uuid-alice", 10)
	if !msgs[0].Deleted || msgs[0].Body != "" {
		t.Fatalf("expected deleted message body cleared, got %+v", msgs[0])
	}

	// Persistence of LastChatID
	s.SetLastChat("uuid-alice")
	s2, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	last, ok := s2.GetLastChat()
	if !ok || last.ChatID != "uuid-alice" {
		t.Fatalf("expected last chat uuid-alice after reload, got %+v (ok=%v)", last, ok)
	}
}

func TestLayoutInputAndWrapping(t *testing.T) {
	// Single line with cursor in middle
	lines, row, col := layoutInput([]rune("hello"), 2, 40)
	if len(lines) != 1 || lines[0] != "hello" || row != 0 || col != 5 {
		t.Fatalf("unexpected single-line layout: lines=%v row=%d col=%d", lines, row, col)
	}

	// Explicit newline
	lines, row, col = layoutInput([]rune("ab\ncd"), 4, 40)
	if len(lines) != 2 || lines[0] != "ab" || lines[1] != "cd" || row != 1 || col != 4 {
		t.Fatalf("unexpected newline layout: lines=%v row=%d col=%d", lines, row, col)
	}

	// Soft wrap at usable width (cols=20 -> usable=17)
	long := []rune("12345678901234567890")
	lines, row, col = layoutInput(long, 19, 20)
	if len(lines) != 2 || row != 1 || col != 5 {
		t.Fatalf("unexpected soft-wrap layout: lines=%v row=%d col=%d", lines, row, col)
	}

	// wrapText word boundaries
	wrapped := wrapText("hello world from signal", 12)
	if len(wrapped) != 2 || wrapped[0] != "hello world" || wrapped[1] != "from signal" {
		t.Fatalf("unexpected wrapText: %#v", wrapped)
	}
}

func TestClipboardFilePathDetectionAndSplitAttach(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "my photo.png")
	if err := os.WriteFile(imgPath, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := normalizePastedText("'" + imgPath + "'")
	want := "/attach \"" + imgPath + "\""
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}

	p, cap := splitAttachArg("\"" + imgPath + "\" look at this")
	if p != imgPath || cap != "look at this" {
		t.Fatalf("splitAttachArg failed: p=%q cap=%q", p, cap)
	}
}

func TestListChatsSearchRanking(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SyncMetadata(nil, []Chat{
		{ChatID: "c1", ChatType: "dm", Name: "Mama", LastMessageTS: 9000, LastPreview: "adam dzwonił"},
		{ChatID: "c2", ChatType: "dm", Name: "Jan Adamczyk", LastMessageTS: 5000},
		{ChatID: "c3", ChatType: "dm", Name: "Madam", LastMessageTS: 8000},
		{ChatID: "c4", ChatType: "dm", Name: "Adam", LastMessageTS: 100},
	})

	res := s.ListChats("adam")
	if len(res) != 4 {
		t.Fatalf("expected 4 matches, got %d", len(res))
	}
	if res[0].Name != "Adam" {
		t.Fatalf("expected Adam first, got %q", res[0].Name)
	}
	if res[1].Name != "Jan Adamczyk" {
		t.Fatalf("expected Jan Adamczyk second (word prefix), got %q", res[1].Name)
	}
	if res[2].Name != "Madam" {
		t.Fatalf("expected Madam third (name substring), got %q", res[2].Name)
	}
	if res[3].Name != "Mama" {
		t.Fatalf("expected Mama fourth (preview only), got %q", res[3].Name)
	}
}
