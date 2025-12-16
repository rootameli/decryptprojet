package smtp

import (
	"crypto/tls"
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

func TestTLSStrategyForPort(t *testing.T) {
	cases := []struct {
		port     int
		expected tlsStrategy
	}{
		{465, tlsImplicit},
		{587, tlsStartTLSRequired},
		{25, tlsStartTLSIfAvailable},
		{2525, tlsStartTLSIfAvailable},
	}

	for _, tc := range cases {
		if got := tlsStrategyForPort(tc.port); got != tc.expected {
			t.Fatalf("port %d: expected %v got %v", tc.port, tc.expected, got)
		}
	}
}

func TestBuildMessageDeterministicHeaders(t *testing.T) {
	baseHeaders := map[string]string{
		"Message-ID":    "id",
		"X-Campaign-ID": "camp",
		"X-Trace-ID":    "trace",
		"Extra-B":       "b",
		"Extra-A":       "a",
	}
	var first string
	for i := 0; i < 50; i++ {
		copyHeaders := make(map[string]string)
		for k, v := range baseHeaders {
			copyHeaders[k] = v
		}
		msg := BuildMessage("From", "from@example.com", "to@example.com", "s", "<b>body</b>", copyHeaders)
		if len(copyHeaders) != len(baseHeaders) {
			t.Fatalf("headers mutated during build")
		}
		if i == 0 {
			first = string(msg)
		} else if string(msg) != first {
			t.Fatalf("non-deterministic headers on iteration %d", i)
		}
	}
}

type mockTLSClient struct {
	extensions  map[string]bool
	startTLSErr error
	startTLSCnt int
}

func (m *mockTLSClient) Extension(ext string) (bool, string) {
	return m.extensions[ext], ""
}

func (m *mockTLSClient) StartTLS(*tls.Config) error {
	m.startTLSCnt++
	return m.startTLSErr
}

func TestNegotiateTLS(t *testing.T) {
	cfg := &tls.Config{}
	t.Run("required missing", func(t *testing.T) {
		client := &mockTLSClient{extensions: map[string]bool{}}
		if _, err := negotiateTLS(client, cfg, tlsStartTLSRequired); err == nil {
			t.Fatal("expected error when STARTTLS missing on required port")
		}
	})

	t.Run("required executes", func(t *testing.T) {
		client := &mockTLSClient{extensions: map[string]bool{"STARTTLS": true}}
		if upgraded, err := negotiateTLS(client, cfg, tlsStartTLSRequired); err != nil {
			t.Fatalf("unexpected error: %v", err)
		} else if !upgraded {
			t.Fatalf("expected upgrade flag")
		}
		if client.startTLSCnt != 1 {
			t.Fatalf("expected starttls once, got %d", client.startTLSCnt)
		}
	})

	t.Run("opportunistic", func(t *testing.T) {
		client := &mockTLSClient{extensions: map[string]bool{}}
		if upgraded, err := negotiateTLS(client, cfg, tlsStartTLSIfAvailable); err != nil {
			t.Fatalf("unexpected error without STARTTLS on opportunistic: %v", err)
		} else if upgraded {
			t.Fatalf("expected no upgrade flag")
		}
		if client.startTLSCnt != 0 {
			t.Fatalf("expected no starttls call, got %d", client.startTLSCnt)
		}
	})
}
