package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets decides whether messages may be delivered to loopback / link-local addresses.
//
// Denied by default. These ranges are not where IM bots or public mail servers live, and what they
// can reach is sensitive: the admin port of another service on the same host, and the cloud metadata
// endpoint (169.254.169.254, which can reveal instance credentials). Delivery addresses are set by
// admins, but an admin session borrowed through XSS/CSRF, or a second person sharing the same JWT,
// could read response content back just by changing the config: doJSON writes the first 200 bytes of
// a 4xx/5xx response body into last_error, and the delivery history API echoes it back, which makes
// a semi-blind read primitive.
//
// But a "local SMTP relay" (postfix on 127.0.0.1:25) is a common setup for self-hosted mail, and a
// blanket ban would leave people stuck. So there is an explicit escape hatch rather than a hard-coded
// allowance: set ARTEX_NOTIFY_ALLOW_LOCAL=1 to allow it.
//
// It is exported as AllowLocalTargetsEnv so tests can turn it on explicitly: test cases in this
// package and the server package make heavy use of httptest fake receivers on 127.0.0.1, and without
// it they would all be stopped by the guard.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP is in a range that "may not be delivered to by default".
//
// It only rejects loopback, link-local (including the cloud metadata address 169.254.169.254),
// unspecified and multicast. It does **not** reject RFC1918 private ranges: a self-hosted Mattermost /
// SMTP relay on the internal network is a very common legitimate use, and blocking those too would make
// the feature unusable in real environments. This trade-off is deliberate: the protection has to stop
// the genuinely sensitive targets without breaking normal deployments along the way.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) must be converted back to IPv4 before checking, otherwise it bypasses the check.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook of the http.Transport dialer; it checks the target address
// **when the connection is established**.
//
// Why at the dial stage rather than only validating when the config is saved: this is where it
// finally takes effect. It covers both ways of getting round config validation: DNS rebinding
// (resolving to a public IP at validation time and to an internal one when actually connecting) and
// redirects (we already refuse cross-host redirects, but a same-host redirect could still point the
// path somewhere else).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot parse target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds just one dial guard on top of the default Transport.
// Clone keeps all of the default tuning (connection pool, HTTP/2, timeouts, proxy, etc.), so adding
// one check doesn't change any other behaviour.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client shared by all channel deliveries.
//
// It deliberately does **not** reuse the project's global egress proxy (GlobalProxy on the server
// side): that proxy is for traffic to penetration testing targets and is often an unstable tunnel,
// and notification availability shouldn't be held hostage by jitter in the target network. Direct
// connections are fine for IM pushes. The timeout is 15 seconds: a remote end slower than that is
// effectively already failing.
//
// Cross-host redirects are refused: delivery addresses in this feature all have the shape of "one
// fixed endpoint" and normally never redirect to another host, while these providers' credentials
// (DingTalk's access_token, WeCom's key, Telegram's bot token) are **right in the URL**, so following
// a cross-host redirect would hand the credentials to the redirect target. Same-host redirects (such
// as adding a trailing slash) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit caps how much of the response body is read. A misbehaving remote end may send back
// huge content, while we only need the status code and a short error description to show in the
// delivery history.
const respBodyLimit = 8 << 10

// doJSON sends a request and returns the response body (length-limited).
//
// When payload is nil an empty body is sent (for GET, or when the platform needs no body).
// The keys and values in headers are added as is, for the generic webhook's custom headers.
//
// Classifying errors is this function's core job: network-level failures and 5xx/408/429 are
// "retryable", and the other 4xx are "permanent failures"; retrying a 403 just writes the same error
// to the log three times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// A serialisation failure is a local bug (wrong config field type); retrying won't make it better.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Invalid URL: most likely the user entered the address wrong, which is a permanent failure.
		// err can't be passed through here either: url.Parse's error text contains the full address.
		return nil, Permanent(fmt.Errorf("invalid request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout: mostly transient faults, left to backoff retries.
		//
		// The error text must be redacted before it is passed on. The reason: http.Client.Do returns a
		// *url.Error whose Error() is `Op "full URL": underlying error`, and for these providers the
		// credentials are **right in the URL** (DingTalk access_token, WeCom key, Feishu hook id,
		// Telegram /bot<token>/). Without redaction the credentials would flow along this error string
		// to four places: last_error in notification_deliveries (stored in plain text), the delivery
		// history API response (**bypassing the channel config masking**), the server logs, and the
		// 502 text that the test-send API returns to the frontend.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limited) and 408 (timeout) are worth retrying; the other 4xx are config or permission problems, and retrying is pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("remote end rate limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("remote service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("remote end rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet squashes the response body into a short single line for error messages. Responses may
// contain newlines and lots of whitespace, and putting that straight into last_error would break the
// layout of the delivery history page.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget squashes a delivery address down to "scheme://host/…" for error messages.
//
// This is the package's only address redaction rule, and it is deliberately **blunt**: everything
// except the scheme and host is dropped. The reason is that there is no "generic and safe" way to tell
// which part of a URL is the credential:
//
//	DingTalk  credential in the query                   /robot/send?access_token=xxx
//	WeCom     credential in the query                   /cgi-bin/webhook/send?key=xxx
//	Feishu    credential in the **last path segment**   /open-apis/bot/v2/hook/<hook_id>
//	Telegram  credential in the **middle of the path**  /bot<token>/sendMessage
//
// "Keeping only the useful part" would need per-channel patches, and missing any one provider means a
// credential leak. Keeping the host is enough for troubleshooting (an unresolvable DNS name, an
// unreachable host and a bad certificate can all be pinned down), and which bot it is can be
// recognised from the masked tail hint in the channel config.
//
// On a parse failure a fixed placeholder is returned: the raw string is never echoed back.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparsable address)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address from a transport-level error, keeping only the underlying
// cause.
//
// *url.Error has the structure {Op, URL, Err}, and Error() prints the URL along with the rest. Here
// the Err field is taken explicitly, bypassing its Error(): this is more reliable than string
// replacement after the fact, because replacement would have to handle all the URL-encoded/escaped
// variants correctly, which is easy to get wrong.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// A non-*url.Error (e.g. an error returned by the redirect policy) may also carry an address, so redact it the same way.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces the http(s) addresses that appear in a piece of text with their redacted
// form.
//
// It is the fallback for errors whose structured fields aren't available (redirect policy errors,
// custom errors from third-party libraries). It only recognises the http/https prefixes and splits on
// whitespace and quotes: an address contains neither.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
