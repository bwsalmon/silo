package chats

import (
	"strings"
	"testing"
)

func TestMentionsAndFormatting(t *testing.T) {
	if ok, _ := MentionsAgent("hey @agent please check this", "ops-agent", nil); !ok {
		t.Fatal("expected @agent to match")
	}
	if ok, _ := MentionsAgent("hey @ops-agent please check", "ops-agent", nil); !ok {
		t.Fatal("expected @ops-agent to match")
	}
	if ok, linked := MentionsAgent("can @design-agent take a look?", "ops-agent", []string{"design-agent"}); !ok || len(linked) != 1 || linked[0] != "design-agent" {
		t.Fatalf("expected linked deployment @design-agent to match, got ok=%v linked=%v", ok, linked)
	}
	if ok, _ := MentionsAgent("hey @bob@example.com what do you think?", "ops-agent", []string{"design-agent"}); ok {
		t.Fatal("did not expect @bob@example.com to match agent mention")
	}
	if !MentionsUser("hey @bob@example.com check this", "bob@example.com") {
		t.Fatal("expected full email mention to match")
	}
	if !MentionsUser("hey @bob check this", "bob@example.com") {
		t.Fatal("expected local-part @bob mention to match")
	}
	slice := FormatChatTranscriptSlice(
		ChatSession{Title: "Payments Ops", Deployment: "ops-agent"},
		[]ChatMessage{
			{Sender: "alice@example.com", Role: "user", Text: "first"},
			{Sender: "bob@example.com", Role: "user", Text: "second"},
		},
		[]string{"design-agent"},
	)
	if !strings.Contains(slice, "[alice@example.com]: first") || !strings.Contains(slice, "[bob@example.com]: second") {
		t.Fatalf("unexpected formatted slice: %q", slice)
	}
}
