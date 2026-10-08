package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related hardening measures:
//   1. delivery addresses must not use the server as a springboard into the internal network / cloud metadata (SSRF)
//   2. address validation errors must not carry out credentials contained in the address
//
// About the test environment: many test cases in this package use httptest fake receivers on 127.0.0.1, which the
// guard blocks by default. So TestMain turns on AllowLocalTargetsEnv globally, and every SSRF case below explicitly
// clears it again to assert the **deny by default** behaviour.

func TestMain(m *testing.M) {
	// Let the regular test cases connect to local fake receivers; the SSRF cases temporarily clear it themselves.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core assertion of the SSRF protection:
// with the default config, delivery to a loopback address must be refused at the **connection layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // escape hatch off = default behaviour
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("delivery to a loopback address should not be allowed by default")
	}
	if hit {
		t.Fatal("the request already reached the local service: the guard didn't take effect")
	}
	// The error message must tell the user how to allow it (a local SMTP relay is a legitimate config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the refusal message should explain how to allow it explicitly: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the reverse case: once explicitly turned on it must work,
// otherwise legitimate deployments such as a local postfix / internal relay would be wiped out wholesale.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work once explicitly allowed: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // the cloud metadata endpoint: the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // the IPv4-mapped form must be converted back before checking, otherwise it is a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be refused", s)
		}
	}
	// RFC1918 private ranges are **deliberately allowed**: a self-hosted Mattermost / SMTP relay on the internal network is a common legitimate use.
	// This assertion pins that trade-off down: if someone later casually adds a private-range check, this fails,
	// forcing a conscious decision (instead of silently breaking a batch of deployments).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private ranges are common legitimate delivery targets)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the early hint at config time:
// literal IPs should be refused when the config is saved, instead of waiting for the first delivery to fail.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be refused at config time", raw)
		}
	}
	// Public and private addresses pass as normal (private ones are left to the dial stage, which doesn't block them).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials is the branch the audit pointed out I had missed in the previous round.
//
// When url.Parse **fails** it returns a *url.Error whose Error() contains the full original address. In the previous
// round I only redacted the errors returned by http.Client.Do and missed this one; and the "permanent failure path"
// cases added back then (file://, gopher://, ftp://) can all actually be parsed by url.Parse and go down the scheme
// branch, so all green proved nothing about this path being safe: it was a false guarantee.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // invalid percent-escape
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // non-numeric port
		"http://[::1?access_token=" + leakProbeToken,                  // unmatched bracket
	}
	for _, raw := range cases {
		// First confirm that this input **really** makes url.Parse fail. Without this step the case could end up
		// in another branch without anyone noticing (that is exactly where the previous round's false guarantee came from).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should fail to parse, otherwise this case doesn't cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm that the channel layer's wrapping doesn't carry the address out either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("an invalid address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the SMTP channel's dial guard.
//
// The email channel used to use a bare net.Dialer, the only gap in the whole SSRF protection: a host of
// 169.254.169.254 or 127.0.0.1 could be connected to directly, and when the smtp.NewClient handshake fails it wraps
// the line returned by the remote end into the error, which the delivery history API echoes back via last_error:
// exactly the semi-blind read primitive the other channels had already closed off; the timing difference between
// "connection refused" and "timeout" could also be used to probe ports.
//
// This package's TestMain turns AllowLocalTargetsEnv on globally (many cases use fake receivers on 127.0.0.1), so
// this case has to clear it itself: otherwise it would pass whether or not the guard is there, which is exactly why
// the gap wasn't caught by any test in the first place.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // escape hatch off = default behaviour
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("delivering email to a loopback address should not be allowed by default")
	}
	// The connection should never be established: the guard blocks it in the Control hook, so EHLO can never be sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("the SMTP session was established: the guard didn't take effect")
	}
	// The error message must tell the user how to allow it (a local postfix relay is a legitimate config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the refusal message should explain how to allow it explicitly: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired reverse case: once explicitly turned on, delivery must
// work normally. A self-hosted SMTP on the internal network / a local relay is a very common deployment, and the guard
// can't be one-size-fits-all.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("once explicitly allowed, local SMTP delivery should work: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("no EHLO seen: the session was never really established")
	}
}
