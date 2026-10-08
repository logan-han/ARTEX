package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// This file is the channel configuration and event layer of IM notifications. For claiming delivery jobs and
// their state transitions, see db/notification_delivery.go.
//
// Two invariants that must hold whenever this file changes:
//
//  1. The transaction that writes a vulnerability (RecordFindingTx) only calls InsertNotificationEventTx for one blind insert;
//     it reads no notification tables and does no filter matching. Any read introduced here could let a filter
//     the user misconfigured poison or even abort the vulnerability write transaction.
//  2. Filter matching never errors: a malformed config is always treated as a "match" (see notify.Match). Better to
//     push too much than to miss a push.

// ErrNotificationChannelNotFound means the channel does not exist.
var ErrNotificationChannelNotFound = errors.New("notification channel not found")

// Delivery states.
const (
	NotifyStatePending = "pending" // awaiting send
	NotifyStateSending = "sending" // claimed by a dispatcher, lease not expired
	NotifyStateSent    = "sent"    // delivered
	NotifyStateFailed  = "failed"  // retries exhausted or permanent failure; can be resent manually
	NotifyStateSkipped = "skipped" // channel disabled, will not be sent
)

// Push modes.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode validates the push mode against an allowlist (same reasoning as findings.status: no DB CHECK,
// so it is easy to extend later).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel is the config of one channel instance. Config and Filter stay as raw JSON
// and parsing is left to the notify package: the db layer does not understand what their fields mean.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled is a pointer to tell "field not sent" apart from "explicitly sent false":
	// the frontend toggle only submits the fields that changed.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled reports whether the channel is enabled; a nil Enabled (not loaded) counts as enabled.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent is one event fact.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels returns all channel instances, enabled ones first, then by id.
// The ordering is done in SQL so the UI and the dispatcher see the same stable order.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID fetches a single channel.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel creates or updates a channel.
//
// An update only overwrites the fields the caller gave explicitly (non-nil / non-empty), so the frontend can submit a
// partially edited drawer form without sending back the config fields it does not show; sending them back would cause
// the "masked value overwrites the real secret" accident.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// This deliberately does **not** touch 0 in any way: 0 is a valid setting meaning "no rate limit".
	//
	// It used to read `if c.RatePerMin <= 0 { c.RatePerMin = defaultValue }`, meant as "give a safe default when
	// unspecified", but that also swallowed "explicitly set to 0": the docs, the UI hint and
	// takeTokens all read 0 as no rate limit, yet this one place quietly changed it to 20 (DingTalk/WeCom/Telegram)
	// or 100 (Feishu), so operators thought they had lifted the limit while actually being held at 20/minute with no hint at all.
	//
	// Only the caller knows the difference between "unspecified" and "explicit 0" (field omitted in the request body vs 0 passed explicitly),
	// so the server layer fills in the default when the field is omitted; see notifyCreateChannel.
	if c.RatePerMin < 0 {
		return 0, errors.New("rate limit cannot be negative")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled toggles a channel on or off.
//
// Disabling a channel also marks its unsent deliveries as skipped: otherwise, once re-enabled, it would
// suddenly receive a batch of old vulnerabilities "backlogged while disabled", already stale and easily mistaken for new ones.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "channel disabled", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel deletes a channel. Its delivery history is cascade-deleted via the foreign key
// (with the channel config gone, the history cannot be interpreted).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx writes one notification event, on a **best-effort** basis, inside the caller's transaction.
//
// This is the only notification-related change on the vulnerability write path: one INSERT that reads no tables, knows no channels
// and runs no filters. Committing the transaction guarantees that "vulnerability persisted" and "notification job exists" are atomic,
// with no window where the commit succeeds without enqueueing and the message is lost forever.
//
// Two key design choices, neither of them casual:
//
//  1. **Why a SAVEPOINT**: in PostgreSQL, any failing statement inside a transaction puts the whole transaction into the
//     aborted state, after which every statement (COMMIT included) fails. So "ignore this INSERT's
//     error and let the caller keep committing" is impossible in PG, unless a savepoint isolates the error to
//     this one statement. Without a savepoint, the only option left is "roll back everything".
//
//  2. **Why rolling back everything is wrong**: notifications are a convenience; the vulnerability record is the product itself. A problem
//     with the notification tables (old database not migrated, a transient disk fault) must not stop a high-severity vulnerability from being saved. So this isolates
//     the error, logs it and returns false, letting the vulnerability write commit as usual, at the cost of losing this one notification.
//     Returning bool rather than error is deliberate: callers must not treat it as an error that decides whether the write succeeds.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] failed to serialise notification event finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] failed to create savepoint finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] failed to write notification event finding=%d (vulnerability record unaffected): %v", findingID, err)
		// Roll back to the savepoint to rescue the transaction from the aborted state.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] failed to roll back to savepoint finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// Release the savepoint so a long transaction does not pile up useless savepoints.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent is the standalone-transaction version of InsertNotificationEventTx, for call sites that are not
// inside an existing transaction (such as a channel's "Send test message", which has no real finding).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("failed to serialise notification event snapshot: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents expands vulnerability events that have not been dispatched yet into delivery jobs for the currently enabled channels,
// returning the number of events processed this round and the number of deliveries created.
//
// The whole round runs in one transaction: events are claimed with FOR UPDATE SKIP LOCKED, so several processes running at once
// each claim different rows (the project's archive queue claims jobs the same way; see
// completeNextArchiveJob in db/task_archives.go).
//
// Filter matching is deliberately done in Go rather than SQL: a channel's filter is JSONB with a set of optional fields,
// expressing the matching of six combinations in SQL would make the query hard to maintain, and channels number only "a handful configured by hand",
// so loading them all and comparing one by one in memory is faster and easier to test.
//
// Events that match no channel are marked fanned_out too; otherwise they would stay in the pending-dispatch set forever
// and be rescanned on every tick.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// We write the snapshot ourselves, so in theory it always parses; a parse failure does not block delivery,
		// but with all fields empty this event is skipped by every channel that has a filter: better to miss one push
		// than to let one bad row jam the whole queue.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// The row's kind is authoritative: the copy in the snapshot is for rendering and may have been written by an older version.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// Mark this round's events as dispatched. Events that matched no channel are marked too (see the function comment).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx fetches the enabled channels inside a transaction. There are very few,
// so there is no paging and no cache: a cache would add the extra timing question of "when does a config change take effect".
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames resolves asset ids into short display names for notification messages.
//
// The result keeps the input order and may be shorter than the input (ids that do not exist are skipped). Keeping the input order
// keeps the asset order stable across repeated deliveries of the same vulnerability's message; otherwise a message received after a retry
// with the assets in a different order would be misread as "the assets changed".
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName picks the most recognisable identifier for the asset type.
// The fallback is an empty string, leaving the caller to decide how to present "an asset whose name cannot be resolved": this function does not invent placeholders,
// otherwise noise like "Asset#42" would creep into notification messages and readers would take it for a real domain.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify updates a vulnerability's triage status and records a status change
// notification event in the same transaction.
//
// Returns from = the status before the change; found = whether the vulnerability exists; notified = whether the event was recorded.
//
// Three deliberate behaviours:
//   - No event is recorded when the status does not actually change. The frontend drawer resubmitting the same value, or an automation script
//     replaying idempotently, should not produce notification noise.
//   - When the vulnerability does not exist it returns found=false and writes nothing; the caller translates that into a 404.
//   - A failure to record the event does not affect the status update (see the savepoint notes on RecordNotificationEventTx),
//     so with notified=false the status has already been changed and the caller should not report an error because of it.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx updates a vulnerability's status and records a status change notification event inside **the caller's transaction**.
//
// It is a transaction-level function so every status-changing path shares the same semantics: previously only
// patchFinding used the notifying version, while **a retest verdict of "Fixed"** (the `UPDATE findings SET status=...`
// in finding_retests) wrote straight to the database, so channels configured with
// `on_status_change` never got a notification for these transitions: the status changed quietly in the UI,
// and operations staff only found out when they opened the platform.
//
// Returns from = the status before the change, found = whether the vulnerability exists, changed = whether the status really changed,
// notified = whether the event was recorded (a recording failure does not affect the status update; see RecordNotificationEventTx).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// No event when the status did not really change: resubmitting the same value or an idempotent replay should not
		// produce notification noise.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats holds the overview counts at the top of the notifications page.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // milliseconds since the oldest pending delivery was created
}

// NotificationStatsSnapshot summarises the health of the notification system.
// BacklogAgeMS is the most direct indicator of "are notifications stuck", far more useful than the pending count,
// because a backlog of 3 and a backlog of 3 can differ anywhere from 3 seconds to 3 hours.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
