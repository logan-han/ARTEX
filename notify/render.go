package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, guaranteeing the result is valid UTF-8 and that no
// character is cut.
//
// Why it must cut on character boundaries: WeCom group bot markdown has a hard limit of 4096 **bytes**
// (not characters), and a Chinese character takes 3 bytes. Slicing directly by bytes would cut a
// Chinese character in half and produce invalid UTF-8: the platform either rejects the whole message
// or shows garbled boxes. The approach here is to step back from the budget position to the nearest
// rune start byte (utf8.RuneStart identifies continuation bytes 0b10xxxxxx).
//
// max<=0 means no limit. An ellipsis is appended after truncation, unless max is too small to fit it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis: drop the ellipsis and just cut, so the result doesn't end up over max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine squashes multi-line text into a single line: it collapses all whitespace and then
// truncates by character count. Used for IM message title lines: summaries often contain newlines,
// and putting them straight into a table/title breaks the layout.
// max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an ellipsis when it is
// over. max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's unit of measure: WeCom limits by bytes,
// Telegram by characters. Using the wrong unit doesn't raise an error, it just gets messages cut far
// shorter than expected (1 Chinese character = 3 bytes, so cutting at 4096 bytes leaves only about
// 1365 characters), so both functions must be kept and chosen per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count, guaranteeing that no tag is cut in half.
//
// Truncating HTML by characters directly can leave a broken tag like `<a href="htt`, and the
// platform's parser then either rejects the whole message or swallows the following body as an
// attribute value. The approach here: truncate by characters first, then check whether the tail has
// an unclosed `<`, and if so back off to before it.
//
// No tag balancing (adding </b> and the like): Telegram's HTML parser closes unclosed tags
// automatically, while implementing balancing ourselves means handling quotes in attributes,
// comments and self-closing tags; the complexity is out of proportion to the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (no `>` after the last `<`), back off to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a truncated HTML entity (e.g. `&amp;` cut down to `&amp`), back off as well.
	// An entity fragment may get the **whole message** rejected by a parser that only accepts
	// entities; a digest message over the length limit is common to begin with, and it isn't worth
	// losing the whole notification over that.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount works out how many items fit **whole** within the budget, so digest messages are
// packed item by item.
//
// Why pack whole items rather than render the whole message and then truncate: truncation makes the
// later items vanish while their delivery records still get marked as delivered. It can't be seen in
// the message or in the delivery history, and those vulnerabilities are simply lost. With whole-item
// packing, the items that don't fit stay in the database as the next batch, and the kept value the
// caller receives is the number of items this message really delivered.
//
// Parameters: maxSize<=0 means no limit; reserve is the amount set aside for the message header and
// footer; size does the measuring (platforms use different units: WeCom/DingTalk count bytes,
// Telegram counts characters; using the wrong unit doesn't raise an error, it just squeezes Chinese
// messages far below the limit); render renders item idx as its actual text, whose length varies
// with the content and can't be estimated.
//
// Returns at least 1 (as long as there are items). Even an extremely long single item must still be
// sent, with the caller's final truncation as the backstop; otherwise one oversized vulnerability
// would hold the whole batch in place forever.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are the two units of measure for packItemCount. They are named so call sites
// don't have bare func(s string) int closures, where it would be hard to tell at a glance which unit
// is being used.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders the asset list as one line of display text; beyond limit it omits the rest and
// notes the total. A vulnerability may be anchored to dozens of assets, and listing them all would
// blow up the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " and others, " + itoa(len(assets)) + " in total"
}

// itoa is a short alias for strconv.Itoa, used only for building display text, so strconv doesn't
// have to be imported everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
