package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. The writer is the db
// layer's finding-persistence transaction; the readers are the server layer's delivery engine and
// filter matching. It is defined in this package because it is a "notification domain" payload: db
// only serialises it and does not understand what the fields mean.
//
// Why the vulnerability fields are stored redundantly instead of looked up at render time: a
// vulnerability may later be renamed or have its severity or status changed, while the push content
// should reflect the conclusion **at the time of the event**. A lookup would give the dangerously
// misleading "later changed to low". It also means fan-out and rendering don't need to JOIN the
// findings/tasks/assets tables.
type Snapshot struct {
	// Event kind: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Only non-empty when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is one vulnerability to push, for the channels to render.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets are the resolved asset display names (e.g. domain/IP). Filled in by the server layer:
	// this package doesn't touch the database and can't get the names.
	Assets []string
	// DetailURL is the deep link to the vulnerability details; empty means public_base_url is not
	// configured, and rendering omits it.
	DetailURL string
	// For status change events only; when both are non-empty it renders as "Pending → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether the item is a status change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: prefer the human-assigned name, fall back to the
// vulnerability class (vulnclass), and use a placeholder when both are empty. Never output an
// empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(Unnamed vulnerability)"
}

// Message is the complete content of one channel send.
type Message struct {
	// Length 1 for a single push; a whole batch for a digest push.
	// An empty slice is invalid: the caller must provide at least one item.
	Items []Item
	// Batch=true renders it as a digest message (different title, with the time window and count).
	Batch bool
	// WindowMinutes is the digest period in minutes, used only when Batch=true for the "last N min"
	// wording. It is deliberately passed in explicitly from config instead of computing time.Since
	// at render time: rendering stays deterministic, which keeps it testable.
	WindowMinutes int
	// HomeURL is the platform dashboard URL (the global public_base_url); empty means no dashboard link.
	HomeURL string
}
