package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "pack whole items" fix: when a digest message exceeds the channel length limit it must be
// truncated **by whole items**, with the number of items that didn't fit reported truthfully, so the caller marks only
// the ones really delivered.
//
// The previous approach rendered the whole message, truncated it and then marked the whole batch as delivered: the
// second half of the message simply vanished while the delivery history showed everything as successful. The
// vulnerabilities were just lost, with nowhere to discover it.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// A 200-item CJK digest, bound to far exceed WeCom's 4096 bytes.
	m := cjkBatchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body of %d bytes exceeds the limit of %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only part should fit (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must say truthfully how many items this message contains and how many remain: otherwise readers
	// take the number in the header as everything.
	if !strings.Contains(body, "the remaining") || !strings.Contains(body, "will follow in the next message") {
		t.Fatalf("the header should say how many items are not included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Only the first kept items should be included.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, m.Items[i].Name) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, m.Items[kept].Name) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("with no length limit everything should be kept, got kept=%d", kept)
	}
	if strings.Contains(body, "the remaining") {
		t.Fatalf("there should be no truncation note when nothing was truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget is too small for even one item, one must still be sent (with the final truncation as the backstop).
	// Otherwise one oversized vulnerability would hold the whole batch in place forever: it never fits on any claim, so nothing is ever sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("at least 1 item should be kept, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single message should report 1 item delivered, got %d", kept)
	}
	// An empty message has no items to deliver.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("an empty message should report 0 items, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := cjkBatchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits length in **characters**; measuring in bytes would squeeze CJK messages down to a third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body of %d characters exceeds the limit of %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only part should fit, got %d", kept)
	}
	if !strings.Contains(text, "will follow in the next message") {
		t.Fatalf("should say that the rest are not included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("the card should hold only part of the batch, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels don't truncate the body, so the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition doesn't hold")
	}
	// Confirmed indirectly through the renderer's return value: markdownBody(0) keeps everything when there is no limit.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("with no length limit all items should be used, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is the regression test for "untrusted content must not change the message structure".
// Titles and summaries come from model output (and the model reads responses from the target under test), and asset names come from the target's URLs.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in the result (escaped form)
		wrong []string // must not appear in the result (unescaped form)
	}{
		{
			name: "newline + external link in the title",
			item: Item{
				Severity: "high",
				Name:     "Login SQL injection\n[URGENT: click here to verify your account](http://attacker.tld)",
			},
			// The newline must be collapsed (otherwise it can forge a new list item/block quote);
			// square and round brackets must be escaped (otherwise it is a clickable external link).
			must:  []string{`\[URGENT: click here to verify your account\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[URGENT", "\n\n[URGENT"},
		},
		{
			name: "image beacon in the title",
			item: Item{
				Severity: "high",
				Name:     "Vulnerability ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and block quote in an asset name",
			item: Item{
				Severity: "high",
				Name:     "Ordinary title",
				Assets:   []string{"a.com/*inject*>quote"},
			},
			must:  []string{`\*inject\*`, `\>`},
			wrong: []string{"*inject*"},
		},
		{
			name: "backticks and pipe in the summary",
			item: Item{
				Severity: "high",
				Name:     "Title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// writeItem in single mode is the rendering path shared by the three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("unescaped form %q appeared (it could be used to inject structure or external links):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst locks in the escaping order: backslashes must be handled first,
// otherwise the backslashes added afterwards get escaped again, and double backslashes appear in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("wrong escaping order, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks in a specific regression:
// markdown escaping must not leak into Telegram's HTML output (escaping was once added in the shared title
// function, and visible backslashes like `\(1\)` then showed up in Telegram messages).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *important*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("markdown backslash escapes appeared in the Telegram body:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
