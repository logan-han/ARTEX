package db

import (
	"fmt"
	"testing"
	"time"
)

// InsertSettingIfAbsent is the fallback for auth.password_hash: the caller's GetSetting check can fail
// because of a database error, or be overtaken by a concurrent request (bcrypt takes tens of
// milliseconds), so the "may only be set the first time" guarantee must rest on the primary key
// constraint rather than an if check in the layer above.
func TestInsertSettingIfAbsentDoesNotOverwrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// Use a test-only key and never touch the real auth.password_hash in the dev database.
	key := fmt.Sprintf("test.insert_if_absent.%d", time.Now().UnixNano())
	defer func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) }()

	inserted, err := d.InsertSettingIfAbsent(key, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("first write should return inserted=true")
	}

	inserted, err = d.InsertSettingIfAbsent(key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("should return inserted=false when the key already exists")
	}

	got, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		t.Fatalf("GetSetting: ok=%v err=%v", ok, err)
	}
	if got != "first" {
		t.Fatalf("value was overwritten with %q, should stay %q", got, "first")
	}
}
