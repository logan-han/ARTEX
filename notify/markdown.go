package notify

import (
	"fmt"
	"strings"
)

// This file holds the message rendering shared by the "Markdown family" channels (DingTalk, WeCom).
// Feishu uses card JSON, Telegram uses HTML and email uses HTML, each rendered in its own adapter.

// maxAssetsShown is the maximum number of assets listed in a message. A vulnerability may be
// anchored to dozens of assets; listing them all would blow up the message and adds no information:
// nobody reads the 4th domain onwards in an IM.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to. An IM message is a "go and
// look at the details" prompt, not the report itself; the full content is in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is set aside for the message header (digest line + severity breakdown +
// possible truncation note) and footer (platform link). Whole-item packing deducts it from the
// budget so the header and footer never get truncated: once they are cut, readers can't even tell
// "which batch this is and how many items are not shown".
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why it is required: vulnerability titles, summaries, classes and asset display names all come
// from **untrusted sources**. Titles and summaries come from model output (and the model reads the
// responses of the target under test), while an asset's url is the full scanned URL (including a
// query string the target controls). Without escaping, a vulnerability titled
//
//	Login SQL injection\n[URGENT: click here to verify your account](http://attacker.tld)
//
// would render as a **clickable external link** in the security engineer's DingTalk/Feishu, and
// `![](http://attacker.tld/beacon)` would be fetched by the client on render, effectively announcing
// "this vulnerability has been viewed" and leaking the reader's IP. Even with harmless content,
// injected bold or block quotes can push the critical vulnerabilities below the fold.
//
// The escape set covers the characters for headings/links/emphasis/lists/quotes/strikethrough,
// which change the structure or produce clickable elements. `\` must be handled first, otherwise
// the backslashes added afterwards would be escaped again.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText squashes untrusted text into a single line and escapes it, for use in markdown
// bodies. Single-lining is the other half alongside escaping: a newline on its own can forge a new
// list item or block quote, and escaping characters doesn't stop that.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the IM platform's title bar/card title) as **unescaped
// original text**.
//
// It is deliberately not escaped here: this title is shared by renderers in four contexts, markdown
// bodies, Telegram HTML, Feishu card plain_text, and the generic webhook JSON plus the email subject.
// Each context has different escaping rules (markdown escapes put into HTML leave visible
// backslashes, and put into JSON they pollute the data), so escaping must be done by each output, see
// writeItem / feishuItemLines / telegramEscape. Markdown escaping was once added in the shared
// function, and visible backslashes like `\(1\)` then showed up in Telegram messages.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability digest · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability alert"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body and returns the body and the **number of items actually
// written**.
//
// The returned kept is the number of items this delivery really delivered, and the caller marks
// only the first kept as delivered: items kept out by the channel length limit must wait for the
// next batch instead of being marked successful along with the rest. This is exactly where "silent
// loss" comes from: the message was truncated, but the delivery records show everything delivered,
// and nothing anywhere reveals that the second half was never sent.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single message is sent even if it is too long (with the final truncation as the
		// backstop): partial information about one vulnerability beats sending nothing at all.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all on the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the start of a digest message: time window, count and severity
// breakdown. With these, whoever receives the digest can decide whether the batch needs attention
// right away without opening the platform.
//
// items are the items that **actually fit**, and total is how many the batch should have. When they
// differ it must say explicitly "how many more follow in the next message"; otherwise readers assume
// the number in the header is everything, and the items that were never sent don't exist anywhere in
// the interface.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**New vulnerabilities in the last %d min: %d**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**New vulnerabilities: %d**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (this message shows the first %d; the remaining %d will follow in the next message)", len(items), extra)
	}
	// Give the breakdown by severity so readers can see at a glance whether anything is critical.
	// Only the items **actually included in this message** are counted, so "Critical 3" matches the
	// items that can be counted below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single vulnerability item.
//
// prefix is used for the numbering in the digest list. single=true renders the full version (with
// summary and deep link), while the digest list renders only a one-line summary; otherwise a 50-item
// digest would turn into a long document.
//
// All externally sourced content (title/class/assets/summary) goes through markdownText:
// single-lining + escaping. The deep link is built from the admin-configured public_base_url, so it
// isn't untrusted content, and it has to be a clickable link, so it is output as is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: a single line, with the compressed assets and summary after it.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
