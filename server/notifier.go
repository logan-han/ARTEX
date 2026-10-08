package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global setting keys (stored in the settings key-value table, no new table needed).
const (
	// settingNotifyEnabled is the master switch for notifications. On by default: it is for stopping the bleeding with
	// one click during maintenance, not the condition for enabling the feature; the real enabling condition is "whether any channel is configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external access URL used to build deep links to vulnerability details
	// (e.g. https://artex.example.com). Leave it empty and messages carry no deep link button.
	// The project has no reusable external URL setting, so a new one is added here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest mode period (minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the polling interval of the delivery engine. 3 seconds is the upper bound on this engine's latency,
	// and the main source of delay between "vulnerability persisted" and "message arrives in the IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming deliveries. It must be significantly larger than the worst-case
	// time of a single delivery (the notify package's HTTP client timeout of 15 seconds), otherwise the same row could
	// be delivered by two dispatchers at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick limits the number of events fanned out per round, so that when a channel is first enabled
	// the whole historical backlog isn't expanded into delivery tasks in one go.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest period.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap when a channel has no rate limit.
	// It exists to stop "one channel configured with no rate limit + a thousand vulnerabilities found in one scan"
	// from dragging a single loop round into a long block.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the maximum number of deliveries per channel per round.
	//
	// This cap is worked backwards from the **lease duration**: rows are leased when claimed (notifyLease = 3 minutes),
	// and if a round delivers so many items serially that the worst-case time exceeds the lease, the lease on the last
	// few expires before they are sent. Within a single process it doesn't matter (Run runs serially in a single
	// goroutine, and tick doesn't re-enter), but when **two processes share the same database**, the other one
	// re-claims the rows whose lease has expired and sends them again, also incrementing attempts twice and judging
	// them failed while the original process is still delivering.
	//
	// Value: a 3-minute lease / 30-second single timeout = 6 **uses up the lease exactly** with zero margin, so it
	// can't be used; 5 gives a worst case of 150 seconds, leaving 30 seconds of margin. This relationship is pinned
	// down by TestNotifyTickBudgetFitsWithinLease: changing any one of notifyLease, notifySendTimeout or this value
	// makes that assertion fail.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also determines the value of the previous constant:
	// their product can't exceed notifyLease, see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for failure retries, indexed by the number of attempts made.
// The 3 chances (including the first) correspond to db.MaxNotifyAttempts, and the two must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for vulnerability notifications.
//
// It runs alongside the Scheduler as an independent goroutine (see server.New). It deliberately doesn't reuse the
// Scheduler's tick: the latency requirement of notifications (3 seconds) differs from the business rhythm of
// triggers, and the failures of the two don't affect each other: a stuck push shouldn't affect agent triggers.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. There are few channels and little contention, so one mutex is enough;
	// it isn't worth introducing a finer-grained structure for it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is a single channel's token bucket.
//
// A token bucket is used rather than a sliding window that "counts per minute and then resets" because the latter has
// nasty boundary effects: sending a full 20 at the end of a window and another 20 the next instant is 40 within one
// second as far as the platform is concerned, and gets rate limited; a token bucket refills at a constant rate, which
// naturally avoids such bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx is done. It is started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one round: fan out new events first, then deliver the tasks that are due.
//
// A failure at any step is only logged and doesn't break the loop: a fault in the notification system must never
// escalate into a process-level problem. Each tick is independent, and the next round naturally retries.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to fan out events: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to read channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket's unit of measure is **messages** (equivalent to HTTP requests), not vulnerabilities.
		// In realtime mode the two are the same (one message per vulnerability); in digest mode a whole batch of
		// vulnerabilities is combined into one message, so it consumes only one token.
		//
		// Both modes ask the token bucket first and then claim according to the allowance: the order can't be
		// reversed, otherwise deliveries held back by the rate limit would already have used up a retry attempt.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns the token cost and the batch size upper bound for a digest channel this round.
//
// The two return values are in **two different units of measure**, which is exactly why this is a separate function:
//
//   - tokens is a number of messages. A batch of vulnerabilities is combined into one message and sent as one HTTP
//     request, so it is always 1. rate_per_min therefore still applies to digest (at most that many digest messages per minute).
//   - claimLimit is the maximum number of vulnerabilities in this batch. It is bounded only by memory and has nothing
//     to do with the request budget.
//
// To make rate_per_min apply to digest, the per-round request budget (notifyMaxSendsPerChannelPerTick, worked
// backwards from the lease) was once passed straight down as the batch size. The result was that a channel with
// rate_per_min=20 only refilled 1 token per 3-second tick, so each digest message held only 1 vulnerability: digest
// degraded into "realtime push with digest wording", readers received a string of "New vulnerabilities in the last
// 30 min: 1", and db.MaxDigestBatchSize was never reachable.
//
// This symptom isn't easy to spot in end-to-end tests (the existing cases all pass a large enough limit to stepDigest
// by hand, bypassing the allowance calculation in step), so the decision is gathered here and pinned down directly by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers a channel's realtime tasks, one message per vulnerability.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim realtime deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// A render failure is a local data problem; retrying won't make it better.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest combines a channel's pending deliveries into one message and sends it when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to check digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshot is broken and couldn't make it into the message must be explicitly marked failed.
	// Otherwise they would stay outside included, in neither the message nor the failure list: when the send succeeds
	// the later batch marking would miss them, and they would sit in sending forever until the lease expired and they got claimed again and again.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "the event snapshot could not be parsed, so this vulnerability could not be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark bad-snapshot deliveries as failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparsable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Hand only those that made it into the message to send: included[i] corresponds strictly to msg.Items[i], and
	// send relies on that correspondence to apply "the channel reports that the first K fit" to the right delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and moves the state on according to the result.
//
// The same batch of deliveries (possibly dozens in digest mode) shares one send result: either delivered, or the whole
// batch retried. There is no per-item retry: a digest is a single message, and resending part of it would scramble the
// batch semantics.
//
// The only exception is **segmenting caused by the channel length limit**: if the channel reports it could only fit the
// first K items, then items K+1 onwards must stay for the next batch instead of being marked successful along with the
// rest. Otherwise the truncated vulnerabilities would be in neither the message nor the failure list, and would vanish completely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap a single delivery so one stuck channel can't hold up all the remaining channels in this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The count reported by the channel can't exceed the number of deliveries; if it really happens the
			// rendering layer miscounted. Treat it as all delivered and log the problem, which beats scrambling the records.
			log.Printf("[notify] channel reported %d delivered, more than the %d deliveries channel=%s; treating all as delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark as delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message hit the channel length limit: the rest go straight back to the queue for the next tick to continue.
			// Use DeferDeliveries rather than RescheduleDeliveries: this isn't a failure and shouldn't use up the
			// retry budget (attempts was optimistically +1 at claim time, and it is decremented there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message reached the channel length limit; only the first %d were delivered, the rest wait for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to requeue the remainder for the next segment channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither reported an error nor said how many were delivered. Treat it as a failure (with
		// backoff), so the delivery isn't claimed over and over without ever being marked off.
		err = fmt.Errorf("channel did not report a delivered count (delivered=%d)", delivered)
	}

	// How a failure is handled is decided **per item**, not from the maximum attempt count of the whole batch.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts`, writing off the whole batch, but the items in a
	// batch don't have the same attempt counts: an old delivery that had already been retried twice (attempts=2) would
	// drag the brand new deliveries in the same batch (attempts=1) into failed with it. New vulnerabilities were lost
	// for good without a single retry, the exact opposite of the intent "don't let old rows drag new rows down".
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] error marking as failed channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d attempts: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] error marking as failed channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Reschedule grouped by delay: there are only 3 backoff levels, so the number of groups is naturally small, and there
	// is no need to send a separate UPDATE for each item (that would make a 500-item batch cost 500 round trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d exhausted=%d retrying=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns those in all that aren't in keep (compared by pointer identity).
// It is used to find the deliveries that "didn't make it into the message": they must be dealt with explicitly and can't be left in a grey area.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt gets the channel implementation and parses its config.
// ok=false means the type isn't registered, and the delivery should fail straight away rather than retry forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// Give an empty map when config parsing fails: the channel's own Validate will report "which field is missing",
		// and that error does more to guide the user to a fix than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single-vulnerability message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders a digest message. Snapshots are parsed one by one: a single bad one only skips that item,
// rather than dragging the whole digest down with it.
//
// The returned included corresponds **strictly one-to-one** with msg.Items (delivery i ↔ item i).
// This correspondence is a hard requirement: the caller decides that the first K deliveries are marked delivered
// based on "the channel reports that the first K fit". If a bad snapshot were skipped here without removing the
// skipped delivery from included, the indices would shift: the bad item that should fail would be marked delivered,
// while a good item would be wrongly judged undelivered.
// The broken ones are explicitly marked failed by the caller, see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A bad snapshot goes into neither the message nor included: dealing with it is the caller's job
			// (explicitly marking it failed, rather than letting it slip through among the "delivered").
			log.Printf("[notify] skipping unparsable snapshot in digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("none of the %d deliveries in the digest batch could be parsed", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into an item to push, resolving asset names and the details deep link along the way.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// A failure to resolve asset names shouldn't block the push: not getting the names is far less serious than not
		// getting the notification; the message is just missing an assets line.
		log.Printf("[notify] failed to resolve asset names finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// For the details page route see web/src/app/(main)/function/findings/detail/page.tsx,
		// which reads the vulnerability id from the id query parameter.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most want** tokens from the channel's token bucket and returns how many it actually got.
//
// One token = one message (one HTTP request). In realtime mode the caller passes however many it wants; in digest mode
// a whole batch of vulnerabilities is sent as just one message, so it passes 1.
//
// The bucket capacity is the channel's per-minute limit, refilled at a constant rate. ratePerMin<=0 means no rate
// limit, and a finite but large enough value is returned so a single loop round isn't held up by an unbounded backlog.
//
// The want cap is necessary: without it the only option would be to drain the whole bucket, while the caller has its
// own per-round cap, so the extra tokens taken could neither be used nor survive until the next refill. The burst
// capacity saved up would never be reachable, and even "this round has no pending deliveries at all" would still be charged.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Refill based on the real time elapsed, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before rounding down: the token count is accumulated in floating point, and when it is filled
	// in two steps 0.5 + 0.5 may come out as 0.9999999999, which a plain int() truncates to 0: a bucket that is
	// mathematically full yet yields no token. 1e-9 is far smaller than one token, so it never lets a real shortfall through.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external URL used for deep links, with trailing slashes removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest period, falling back to the default when it is invalid or not configured.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot of the event behind a delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("event snapshot for delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse event snapshot for delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event row is the source of truth for the event kind; the copy in the snapshot may have been written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
