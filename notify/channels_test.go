package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg builds a single message containing quotes and a newline. The title/summary deliberately contain `"`
// and `\n`: these are exactly the inputs most likely to make template interpolation produce invalid JSON.
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `Login "SQL injection" risk`,
			VulnClass: "SQL injection",
			Severity:  "high",
			Summary:   "Parameter id\nis not filtered, causing injection",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg builds a digest batch.
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "Vulnerability " + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "Reflected cross-site scripting",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// cjkBatchMsg is batchMsg with CJK names and summaries (3 bytes per character in UTF-8), for the tests that
// exercise byte vs character limits and UTF-8-safe truncation. The CJK sample data is deliberate.
func cjkBatchMsg(n int) Message {
	m := batchMsg(n)
	for i := range m.Items {
		m.Items[i].Name = "漏洞" + itoa(i+1)
		m.Items[i].Summary = "反射型跨站脚本"
	}
	return m
}

// capturePost starts a fake receiver and passes the request body and headers it receives to the assert function.
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("request body is not valid JSON: %v\nraw: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("should send actionCard when there is a deep link, got %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("deep link lost: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("a digest should be sent as markdown, got %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "in the last 30 min") {
			t.Errorf("digest body is missing the time window: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent locks in the handling of "HTTP 200 but non-zero errcode".
// Not checking errcode records failed deliveries as successful: a trap shared by all the Chinese IM platforms.
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("a non-zero errcode should return an error")
	}
	if !IsPermanent(err) {
		t.Fatalf("a keyword mismatch is a config error and should be marked permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("the error should include the platform error code, got %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("not valid UTF-8 after truncation: WeCom rejects the whole message")
		}
	})
	// Build a CJK digest long enough that it is bound to exceed 4096 bytes.
	m := cjkBatchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("body of %d bytes exceeds the WeCom limit of %d", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("body is empty")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009 is a rolling-window rate limit and should be retryable, got %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000 means the key is invalid and retrying won't fix it, so it should be permanent, got %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("should send an interactive card, got %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high severity should use the orange template, got %v", header["template"])
		}
		// With a secret configured the signing parameters are required, otherwise Feishu rejects it with 19021.
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("missing signing parameters: %v", body)
		}
		// The card elements should contain a button whose url points to the vulnerability details.
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("the card has no button pointing to the details page")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("signing parameters should not be sent when no secret is configured: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("should use HTML parse mode, got %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// Titles and summaries come from the target under test / model output, so they are untrusted.
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML not escaped, injection possible: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("expected escaped entities, got %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& not escaped, got %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429 should be retryable, got %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403 is a config problem and should be permanent, got %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// This is why the default template exists: when the title contains quotes and newlines, any naive
	// `"title": "{{.Title}}"` style produces invalid JSON. {{json .}} doesn't.
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 High] Login "SQL injection" risk` {
			t.Errorf("title not restored correctly: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items count should be 1, got %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "Parameter id\nis not filtered, causing injection" {
			t.Errorf("summary not restored correctly: %v", it["summary"])
		}
		// Numbers must be JSON numbers, not strings (a `json:"...,string"` style would fall into this trap).
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id should be a number, got %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("custom header lost: %v", r.Header)
		}
		if body["msg"] != "3 items" {
			t.Errorf("custom template rendered incorrectly: %v", body["msg"])
		}
		if body["first"] != "Vulnerability 1" {
			t.Errorf("range extraction incorrect: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d items" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a non-JSON render result should be a permanent failure (the template is wrong, retrying is useless), got %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("config set %d should be rejected: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("failed to assemble email: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("incorrect From header:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("incorrect To header:\n%s", msg)
	}
	// A non-ASCII subject (here the severity emoji) must be RFC 2047 encoded, otherwise clients show it garbled.
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("subject not RFC 2047 encoded:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("subject cannot be decoded: %v", err)
	} else if !strings.Contains(dec, "SQL injection") {
		t.Fatalf("decoded subject has the wrong content: %q", dec)
	}

	// The body is base64 and should decode to valid HTML.
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("email is missing the header/body separator")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("failed to base64-decode the body: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("body is not HTML: %.80s", html)
	}
	// The title appears as is in a text position: double quotes are legal characters in HTML text content and
	// need no escaping. Asserting "kept as is" here guards against someone later adding a layer of quote
	// escaping by mistake, which would make the quotes in the title display as &quot;.
	if !strings.Contains(html, `"SQL injection"`) {
		t.Fatalf("quotes in the title should be kept as is in a text position: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection covers the injection the email body really needs to guard against:
// vulnerability titles and summaries come from the target under test and model output, so they are untrusted.
// Text positions must escape & < > (otherwise tags can be injected), and attribute positions must also escape
// quotes (otherwise href can be closed).
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("title not escaped, tags can be injected: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("expected escaped entities: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& and > not escaped: %s", html)
	}
	// The deep link is the admin-configured public_base_url, which is fairly trustworthy in itself, but quotes in
	// attribute positions must still be escaped: otherwise an address containing a quote closes href and injects
	// an event handler.
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href attribute not escaped correctly: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("quotes in attribute positions should be escaped: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("header %s not found", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// Validation errors are shown directly to whoever configures the channel, so they must say what is missing
	// rather than a vague "invalid config".
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "port"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "recipient"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("channel %s is not registered", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s config %v should fail validation", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("the %s error should mention %q, got %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification locks in the semantic distinction between SMTP 4xx and 5xx.
// If 4xx were also judged permanent, a mail server with greylisting enabled would send every push to failed
// after the first attempt, when greylisting is exactly where automatic retries should do the most good.
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// With no reply code it is treated as "retryable": better to try once more than to write off a
		// possibly transient fault.
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("recipient rejected", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("reply %q: expected permanent=%v, got %v", tc.reply, tc.permanent, got)
		}
		// However it is classified, the original text must be kept for the user to troubleshoot.
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("the original text of reply %q was dropped: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// All six channels are required: a missing one would silently disappear from the UI dropdown.
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("there should be %d channels, got %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("channel %s is not registered", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("Kind() of channel %s doesn't match its registry key", k)
		}
	}
	if ValidKind("nope") {
		t.Error("an unregistered kind should not pass validation")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("should be recognised as a permanent failure")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("the error message should pass the underlying error through: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must return nil")
	}
	if IsPermanent(nil) {
		t.Fatal("nil is not a permanent failure")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
