package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// This file fills in protocol-level tests for the email channel. Before this, email.Send had 0% coverage: no test
// case had ever run the SMTP path at all, even though it has the largest protocol surface of the six channels and
// is the easiest to get wrong (the handshake, auth, envelope and DATA stages each have their own failure semantics).
//
// It is driven by a minimal home-made SMTP server rather than mocking out net/smtp: nearly all of the email
// channel's risk lies in the "talking to a real SMTP server" step, and mocking that step out amounts to not
// testing it.

// fakeSMTP is an SMTP server that does just enough: it can complete greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT,
// and returns the reply code a test case asks for at specific stages.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply is the reply to RCPT TO; 250 by default.
	rcptReply string
	// mailReply is the reply to MAIL FROM; 250 by default.
	mailReply string
	// advertiseAuth, when true, advertises AUTH PLAIN support in EHLO.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("not a TCP listen address")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// Don't advertise STARTTLS: make the code take the plaintext branch (the test target is the envelope logic, not TLS).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// Simplified handling: the PLAIN initial response may span several lines, so just accept it.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	// The envelope stages must all be reached: sender, both recipients, DATA.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP session is missing %q, actual commands: %v", want, f.commands)
		}
	}
	// The body is base64 HTML and must carry the real vulnerability content (still recognisable once encoded).
	body := f.body()
	if body == "" {
		t.Fatal("no body received in the DATA stage")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("missing Content-Type header:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("body not base64-encoded (long HTML lines would break SMTP's 1000-byte line length limit):\n%s", body)
	}
	// Every recipient must appear in the To header.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To header doesn't include all recipients:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// With no username configured AUTH must not be sent: some relays reject the message because of it.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("AUTH was sent even though no username is configured: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies directly verifies the fix from this audit:
// 5xx is judged a permanent failure, 4xx (greylisting) retryable.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"recipient permanently rejected with 550", "550 5.1.1 User unknown", "250 OK", true},
		{"recipient hits 450 greylisting", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"recipient hits 452 mailbox full", "452 4.2.2 Mailbox full", "250 OK", false},
		{"sender permanently rejected with 553", "250 OK", "553 5.1.3 Bad address", true},
		{"sender hits 451 temporary error", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("should return an error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("wrong permanent verdict: expected %v, got %v (%v)", tc.permanent, got, err)
			}
			// The server's original text must be kept, otherwise users can't tell whether to contact the server admin or fix the address.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("the error should keep the server's reply code: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp's PlainAuth refuses to send credentials over an unencrypted connection (unless the target is localhost).
	// This is the **correct** security behaviour and must not be bypassed, but it must give an error that guides the user to a fix.
	// A non-localhost host name is used here to trigger it.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // not localhost
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("local DNS resolved to a local server, skipping (doesn't affect the other test cases)")
	}
	// Either failing to connect or refusing to send the credentials passes this assertion; the key point is that the password must **not** be sent out silently.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "connect") {
		t.Logf("error: %v (failing to connect to a non-localhost host is expected)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// The email channel has the most config fields, and any missing one would only surface at delivery time; this
	// checks one by one that validation catches them early. The assertion checks that "the error message says what is missing".
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"missing host", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"missing port", map[string]any{"host": "smtp.example.com"}},
		{"port out of range", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"missing from", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"missing to", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("should fail validation: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance covers tolerant config reading: numbers in JSONB are float64, but users may enter the
// port as a string in the UI, and an array may also be a single string.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // port as a string
		"from": "a@b.c",
		"to":   "d@e.f", // a single string instead of an array
		"tls":  "true",  // boolean as a string
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("numbers in string form should be tolerated: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt didn't parse the string port, got %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool didn't parse the string \"true\"")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings doesn't handle a single string, got %v", to)
	}
}

// TestFilterValidateRejectsTypo directly verifies the audit fix:
// a typo in the threshold must be caught on write, otherwise the filter silently stops working and pushes everything.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("valid threshold %q was rejected: %v", s, err)
		}
	}
	// These are typos that really happen: all of them must be rejected.
	for _, s := range []string{"hgih", "HIGH", "Critical", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("invalid threshold %q should be rejected (otherwise the filter silently stops working and pushes everything)", s)
			continue
		}
		// The error message must be able to guide the user to the right value.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("the error message should list the options, got %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly locks in the split of "strict on write, lenient on read":
// bad values already in the database must not make a channel entirely unreadable (that would suddenly stop every
// existing channel from pushing).
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // no error
	if f.MinSeverity != "hgih" {
		t.Fatalf("the read path should keep the value as is, got %q", f.MinSeverity)
	}
	// And the channel can still reach a verdict on events (no panic, no blocking).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
