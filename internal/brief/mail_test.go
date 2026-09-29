package brief_test

import (
	"github.com/Cadet94957/Exec-Assist-AI/internal/brief"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"strings"
	"testing"
	"time"
)

func TestMailAppearsInBriefAsBoundedEvidence(t *testing.T) {
	_, s := stores(t)
	now := time.Now().UTC()
	if err := s.Ledger().ReplaceMailSnapshot("gmail", []ledger.MailMessage{{ExternalID: "1", Subject: "Meeting request", Sender: "Jess", Body: strings.Repeat("x", 5000), ReceivedAt: now.Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	in, err := brief.Assemble(s, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Emails) != 1 || in.Empty() {
		t.Fatal("email missing from brief")
	}
	prompt := brief.RenderInput(in)
	if !strings.Contains(prompt, "Meeting request") || strings.Contains(prompt, strings.Repeat("x", 1501)) {
		t.Fatal("email evidence absent or unbounded")
	}
}
