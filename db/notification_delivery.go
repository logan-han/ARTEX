package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file covers claiming delivery jobs and their state transitions.
//
// Claiming uses a "lease" rather than a long transaction: the row is set to sending and next_attempt_at is pushed into the future as
// the lease expiry, and the network delivery happens only after the transaction commits. That way no database lock is held during delivery:
// a network request can take several seconds (15 second client timeout), and holding the row lock would drag down other writes on the same database.
//
// The cost is that if the process crashes mid-delivery, the row stays in sending. This **heals itself**: once the lease expires,
// next_attempt_at is in the past and the next claim round picks up the same row again (see the claim condition
// state IN ('pending','sending')). The retry count is already +1 at claim time, so a crash cannot cause
// infinite retries: after MaxNotifyAttempts chances are used up, the row lands in failed and waits for manual handling.

// MaxNotifyAttempts is the maximum number of attempts for one delivery (including the first).
// It is defined here rather than in the delivery engine: it is the state machine's own policy, and the engine only executes it.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize is the most deliveries a single digest batch merges at once.
//
// The reason it exists is resources: if tens of thousands of vulnerabilities turn up within one digest period (entirely possible: one full scan
// can do it), then without an upper bound the claim would read every row into memory and render one enormous message,
// most of which the channel's length limit would then cut off, wasting memory and **silently losing** the vulnerabilities that were cut.
// With an upper bound, the excess stays in the database as the next batch and goes out naturally in the next period, so nothing is lost.
//
// Why 500: it is the order of magnitude that, once rendered into a message, still leaves "something readable" within WeCom's 4096-byte limit;
// anything larger only moves the truncation further along.
const MaxDigestBatchSize = 500

// NotificationDelivery is one delivery job, including the channel config and event snapshot needed for rendering.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// Render context loaded by the join; not in the JSON (the server layer assembles the DTO).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind come from the event, so the history list can jump straight to the vulnerability details.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind are redundant fields for the list display, sparing the frontend a second query.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery is the common read shape for delivery rows: delivery + event snapshot + channel config.
// Rendering a message needs all three, and querying them separately would mean three round trips.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery describes one claim: first select and lock the candidates with sel, then set them to sending and
// extend the lease. The caller fills the lease position in sel with a $n placeholder and passes the argument itself.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries claims a batch of due realtime deliveries for one channel, at most limit rows.
//
// It deliberately claims **per channel** rather than "claim a global batch, then pick what to send": the rate limiter in the delivery engine is kept
// per channel, and only by first knowing how many more this channel may send this round, then claiming that many rows, does rate limiting avoid using up
// retry attempts. Done the other way round (claim first, then drop), rows held back by the rate limit have already had one attempt counted,
// and the budget of 3 would be used up purely by waiting, ending in failed.
//
// The condition includes "sending with an expired lease": that is where crash self-healing lands. lease must be well above the worst-case time of a single
// delivery (the channel HTTP client timeout is 15 seconds), otherwise the same row would be delivered by two dispatchers
// at once. Disabled channels are also excluded: disabling already marks existing deliveries as skipped,
// and this second check catches anything that slips through when disabling and claiming run concurrently.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue reports whether the channel has built up a due batch: there are pending deliveries, and **the oldest one**
// has reached the digest period in age.
//
// The decision is based on the age of the oldest delivery rather than the wall clock: that way a freshly created channel does not, by aligning to the hour,
// immediately spit out a "digest" holding a single item, and a long-backlogged batch does not wait another round for nothing.
//
// It is separate from ClaimDigestBatch because the semantics differ: this function only answers "should it be sent",
// while the claim takes **all** of the channel's pending rows (including those not yet old enough); otherwise one period
// would be split into several messages and the digest would lose its point.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch claims a channel's currently due pending deliveries as one digest batch,
// at most MaxDigestBatchSize per batch.
//
// All deliveries in the same batch share batch_id, using the smallest id in the set as the batch number (stable, readable,
// no extra sequence needed). Retries keep the original batch number via COALESCE, so "these N were sent together"
// still holds after several retries.
//
// It takes the first N by ascending id rather than at random: the earliest deliveries go out first, so a backlog cannot cause
// the starvation of "new vulnerabilities go first and old ones stay at the back forever".
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit is the **memory upper bound**; callers pass MaxDigestBatchSize, and this clamps it again
	// in case a caller passes a larger value.
	//
	// The "rate limit allowance" is deliberately not accepted as the batch size: rate limiting counts messages (a batch sends
	// only one message and consumes one token, deducted by takeTokens in the server layer), which is a different dimension from "how many
	// vulnerabilities fit in one batch". Once, to make rate_per_min apply to digest, the per-round
	// request budget was passed in as the batch size, and a channel at rate=20/min ended up with only 1 vulnerability per batch,
	// degrading digest into realtime pushes with digest wording. To change rate limiting, change want in takeTokens,
	// not this.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries performs "select + set sending and extend the lease + read the full rows", all in one transaction.
// postClaim is an optional extra step (digest batches use it to write batch_id).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// Set sending and push next_attempt_at into the future: that future moment is the lease expiry,
	// so "lease not expired" and "retry time not reached" share the same condition, with no new column needed.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent marks a batch of deliveries as delivered.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries returns a batch of deliveries to pending and pushes back the retry time.
//
// It returns them to pending rather than adding a new intermediate state so that "how many chances are left" is expressed
// in only one place (MaxNotifyAttempts), keeping the state machine's branches from growing with the retry policy.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries returns a batch of deliveries to pending, claimable again immediately, and **reverts the attempt counted at claim time**.
//
// It has exactly one use: when a digest message is sent in segments to fit the channel's length limit, entries that did not fit in this one wait for the next batch.
// That is not a failure, so it must not use up the retry budget: attempts was already optimistically +1 at claim time,
// so it has to be decremented here. Otherwise a backlog of 500 split into 25 segments of 20
// would have its tail entries marked failed by MaxNotifyAttempts at the 3rd segment, even though they never had any error.
//
// GREATEST(...,0) covers the case of "someone resent manually, resetting attempts to zero, and then this runs",
// so the count never goes negative.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries marks a batch of deliveries as finally failed, waiting for someone to resend them from the delivery history.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// Placeholders start at $3: $1 is state, $2 is last_error.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery manually resends one delivery: reset to pending, retry count cleared,
// due immediately. Clearing the count is deliberate: a person clicking "Resend" means the cause of the earlier failures has been dealt with,
// so there is no reason to limit it by the old count.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delivery %d does not exist or its current state does not allow a resend", id)
	}
	return nil
}

// NotificationDeliveryFilter holds the query conditions for the delivery history.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries returns the delivery history page by page, newest first.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError truncates an error message to a length the column accepts. A channel's response body can be very long
// (especially a generic webhook hitting a self-hosted service), and without truncation the history list payload would bloat.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// Back off to a character boundary so half a UTF-8 character is not left behind for the frontend to show as garbage.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders builds a $n placeholder string starting at start, plus the matching arguments, for use in IN (...).
// For example start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
