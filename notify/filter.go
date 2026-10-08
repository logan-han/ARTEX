package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column: the filter conditions of
// a channel instance. Every field is optional and defaults to "no filtering", which is exactly the
// fallback semantics for malformed config; see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical); empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs: an empty array means unrestricted; non-empty requires the event to intersect it.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude empty means accept everything; non-empty requires vulnclass to match one of its keywords.
	// VulnClassExclude excludes the event when any keyword matches (exclusion takes precedence over inclusion).
	// Matching is a case-insensitive substring match, which is safer than regex: a user getting a regex
	// wrong won't make the channel silently stop working.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether this channel receives vulnerability status change events (only
	// meaningful in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses a channel's filter config.
//
// **It never returns an error.** This is a deliberate design choice: malformed filter config always
// degrades to the zero Filter (= no filtering = everything matches), because for a vulnerability
// notification system **pushing one message too many is far better than silently missing a
// high-severity one**. Turning a parse failure into "don't push" would hand the user a channel that
// looks configured but actually pushes nothing, which is the worst possible failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On a parse failure f stays the zero value, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (an empty string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the **restricted-value** fields in the filter config, for use when saving a channel.
//
// Why it must be caught at write time: Match's verdict for an unknown threshold is `rank >= 0`,
// which is always true. In other words a typo in min_severity ("hgih") makes the filter **silently
// stop working** and push everything. That is in line with this package's "rather push too much
// than miss one" trade-off (nothing gets missed), but the consequence is that users think they are
// doing severity-based pushing while actually flooding the group with every vulnerability, with no
// sign that they got it wrong. This kind of "silent degradation" is exactly what should be stopped
// at the entry point.
//
// Note that Validate is only for the **write** path. The read path still goes through ParseFilter's
// lenient semantics, so bad values already in historical data don't make a whole channel unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("invalid minimum severity %q; options: low / medium / high / critical, or leave blank for no limit", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel with this filter.
//
// **It never returns an error**, for the same reason as ParseFilter: any internal anomaly is treated
// as a "match". Order of checks: event kind → severity threshold → task/asset scope → vulnerability
// class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status change events are only received by channels that explicitly opt in. Off by default,
	// because most users expect "push" to mean "a new vulnerability was found", not a running log
	// that follows every status transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusion first: matching any exclusion keyword rules it out, even if it also matches the inclusion list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan is enough for small sets; both sides are on the scale of "a few dozen picked by
	// hand", and building a map costs more than it saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of the keywords (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
