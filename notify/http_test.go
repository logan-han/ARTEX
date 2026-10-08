package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// This file covers doJSON's HTTP-level error classification.
//
// Why it is tested separately: each channel adapter only handles its platform's own business error codes
// (DingTalk errcode, Feishu code, Telegram's ok field), while classification at the **HTTP level** is done
// centrally by doJSON; the two are independent lines of defence. Without this one, a relay gateway returning 503
// would be treated as a permanent failure and retries abandoned outright, while a 403 would be treated as
// retryable and waste three rounds of backoff.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 success is not an error", 200, false},
		{"429 rate limited is retryable", 429, false},
		{"408 request timeout is retryable", 408, false},
		{"500 server error is retryable", 500, false},
		{"502 bad gateway is retryable", 502, false},
		{"503 service unavailable is retryable", 503, false},
		{"400 bad request is permanent", 400, true},
		{"401 auth failure is permanent", 401, true},
		{"403 forbidden is permanent", 403, true},
		{"404 address not found is permanent", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx should not return an error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("non-2xx should return an error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("wrong permanent verdict for HTTP %d: expected %v, got %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// The status code must appear in the error, otherwise users can't tell whether they misconfigured something or the remote end is down.
			// Assert on the number rather than Go's StatusText: this package writes its own messages
			// (consistent with the rest of the project), and the number is the language-independent part that can be asserted reliably.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("the error message should include HTTP status code %d, got %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet covers snippet: the error description returned by the remote end must be
// brought back, otherwise users only know "it failed" and not why the remote end refused.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should return an error")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("the error message should bring back the remote end's description, got %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded constrains the shape of snippet:
// the remote end's response goes as is into the last_error column and the frontend table, and multiple or overlong
// lines would break the layout and the payload.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// A response with newlines, tabs and 5000 characters of overlong content.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should return an error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("the error message should be squashed into a single line, got %q", msg)
	}
	// snippet is capped at 200 characters + a fixed prefix, so the total must be far smaller than the raw response.
	if len(msg) > 400 {
		t.Errorf("error message too long (%d bytes), it should be truncated by snippet: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse confirms that reading is capped: when the remote end misbehaves and returns
// huge content, the whole response must not be read into memory (every row in the delivery history stores a last_error).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should return an error")
	}
	if len(err.Error()) > 400 {
		t.Errorf("an oversized response should be read with a cap and truncated, error message length %d", len(err.Error()))
	}
}
