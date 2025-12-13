package smtp

import (
	"strings"
	"testing"
)

func TestBuildMessageUniqueID(t *testing.T) {
	headers1 := map[string]string{"Message-ID": "id1", "X-Campaign-ID": "c"}
	msg1 := BuildMessage("From", "from@example.com", "to@example.com", "subj", "<b>hi</b>", headers1)
	headers2 := map[string]string{"Message-ID": "id2", "X-Campaign-ID": "c"}
	msg2 := BuildMessage("From", "from@example.com", "to@example.com", "subj", "<b>hi</b>", headers2)
	if string(msg1) == string(msg2) {
		t.Fatal("messages should differ by id")
	}
	if !strings.Contains(string(msg1), "Message-ID: <id1>") {
		t.Fatalf("expected angle-bracketed message id, got %s", msg1)
	}
}
