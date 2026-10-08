package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements the DingTalk custom bot.
//
// Platform characteristics (which drive the implementation choices here):
//   - Each bot is rate limited to 20 messages/minute, and messages over that are silently dropped
//     (HTTP may still be 200), so rate limiting has to happen on the client; see DefaultRatePerMin.
//   - The security settings offer three options: signing / custom keywords / IP allowlist. Signing is
//     the only one that doesn't depend on message content, so only signing is supported (as well as a
//     bare webhook with none of the three turned on).
//   - Success and failure both return HTTP 200 and are told apart by errcode in the body: not checking
//     errcode would record failed deliveries as successful.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// The DingTalk webhook URL carries the access_token and is itself a credential, so it is masked as a whole.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The destination is the DingTalk webhook URL itself; changing the URL requires restating the signing secret for the new one.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

// Send delivers one message. With a deep link and a single item it uses an ActionCard (with a button), otherwise markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown bodies have no explicit byte limit, but a cap is applied anyway in case an evidence field balloons abnormally.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside 200 responses.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is a signature check failure and 310000 a keyword mismatch: both are config errors
		// that retrying won't fix.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends the timestamp and sign parameters to the webhook following the official
// signing rules.
//
// Rule: string to sign = timestamp + "\n" + secret, the HMAC-SHA256 **key is also the secret**, and
// the result is base64-encoded and then URL-encoded. timestamp is in milliseconds. When secret is empty
// the URL is returned unchanged, to support bots that don't have signing turned on.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Don't pass err through: url.Parse's error text includes the full address (including the access_token).
		return "", fmt.Errorf("failed to parse Webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks that an address is usable and its scheme is supported, and runs the
// internal-address check on literal IP targets.
//
// Two points of care:
//
//  1. **The error message must be redacted**. url.Parse itself returns a *url.Error whose Error()
//     carries the **full original address**, and these providers embed credentials right in the
//     address (DingTalk access_token, WeCom key, Telegram bot token, Feishu hook id). This used to
//     `return err` directly, so the "malformed address" error carried the credentials out, to the test
//     endpoint's 400 response, the last_error stored on every delivery, the server logs and the
//     delivery history API.
//
//  2. **Literal IPs are checked for internal ranges right here**, while domain names are left to the
//     dial stage (blockInternalDial is where it finally takes effect, and it also covers DNS
//     rebinding). Doing it once here means a hint is given when the config is saved, instead of waiting
//     for the first delivery to fail.
//
// Restricting the scheme is defensive: file:// / gopher:// and the like would make http.Client behave
// in unexpected ways (they are already stopped by the scheme check, but there is no reason to open up
// that surface).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("cannot parse address (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https is supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host name")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing to deliver to loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
