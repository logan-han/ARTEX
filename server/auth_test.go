package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetSetting's return values for "key does not exist" and "read failed" differ only in the error:
// in both cases value is an empty string. Once a password-related handler treats the error as "no
// password set yet", it opens the setup entry whenever the database is flaky: authInit lets an
// unauthenticated request overwrite the existing admin password, and authStatus sends the frontend
// straight to /setup to do exactly that.
//
// These two tests close the handler's connection pool to force read failures and assert that both
// places fail closed.
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// Close the pool so subsequent GetSetting calls return an error rather than sql.ErrNoRows.
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (treating a failed read as not initialised sends the user to /setup to overwrite the password); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("must not answer initialized when the read fails, got %v", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (letting the request through on a failed read lets an unauthenticated request overwrite the existing password); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("must not issue a token when the read fails: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"empty", "", true},
		{"seven characters", "1234567", true},
		{"eight characters", "12345678", false},
		{"eight CJK characters count as characters, not bytes", "密码密码密码密码", false},
		{"three CJK characters reach 9 bytes but are only 3 characters", "密码强", true},
		{"72 bytes", strings.Repeat("a", 72), false},
		{"73 bytes exceeds the bcrypt limit", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}
