"use client";

// Shared pieces for LLM retry settings: the "attempts + interval" of each of the five retry layers.
//
// The five layers, inside out: connection (SDK) → empty response (SDK) → same-provider safe window → pool circuit breaker → intent rerun.
// The first three follow the endpoint, so every model profile can override the global defaults; the last two are process-wide with a single global copy.
//
// Every input follows the same "empty = not configured" semantics, consistent with the backend db.RetryRule:
//   attempts   empty/0 = use the built-in default | -1 = turn this retry layer off | >0 = use this count
//   interval   empty/0 = use this layer's own exponential backoff | >0 = use this fixed interval in ms instead

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** Where this retry layer happens and who runs it */
  where: string;
  /** Which errors reach this layer, down to the status code, so nobody has to guess */
  trigger: string;
  /** Errors that look similar but do NOT go through this layer, so a setting with no visible effect isn't mistaken for a bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default attempts when left empty, used for the placeholder */
  defAttempts: number;
  /** Default interval strategy when left empty, used for the placeholder */
  defInterval: string;
  /** What setting attempts to -1 means */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connection retry",
    where: "SDK · before a 200",
    trigger:
      "Cannot connect or no 200 yet: network-level errors such as connection resets / read-write timeouts / DNS failures, plus HTTP 408, 429, 500, 502, 503, 504.",
    skips: "Other status codes (400 / 401 / 403 / 404 / 413 / 422, etc.) are deterministic rejections that would fail again on resend, so they are raised straight away.",
    desc: "Resends the same request unchanged. Once the stream has started (a 200 has been received), a mid-stream disconnect is no longer this layer's job.",
    attemptsLabel: "Retries",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = never retry; failures are raised immediately",
  },
  empty: {
    title: "Empty response retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200 with a normal stop finish_reason, but the whole response has not a single content block. Empty gateway frames, dropped reasoning-field frames and sampling hiccups all look like this.",
    skips: "Responses left empty by max_tokens truncation do not count (raise the output limit for those; a resend would just hit it again).",
    desc: "Resends the entire prompt, so it is costly on long contexts; keep the count low.",
    attemptsLabel: "Retries",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = hand back the empty response as is",
  },
  stream: {
    title: "Same-provider safe-window retry",
    where: "This project · before any output is delivered",
    trigger:
      "Problems only after the stream is established (a 200 received): mid-stream disconnects, provider overloaded, 429 / 5xx error events inside the stream, with not a single token handed to the caller yet.",
    skips:
      "Quota exhausted (402 / insufficient_quota, left to the pool to switch profiles), context too long (413 / context length, left to compaction) and deterministic 400 / 401 / 403 / 404 / 422 rejections are not retried.",
    desc: "Replays the same request on the same profile. Because no output has been delivered yet, a replay cannot duplicate model output or tool runs.",
    attemptsLabel: "Retries",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (capped at 4s)",
    offHint: "-1 = hand a broken stream straight to the outer intent rerun",
  },
  breaker: {
    title: "Pool circuit breaker",
    where: "This project · process-wide, one global copy",
    trigger:
      "Trips when consecutive transient failures (429, 5xx, network errors) reach the threshold; deterministic failures such as insufficient balance (402), invalid key (401 / 403) or model not found (404) ignore the threshold and trip on the first occurrence.",
    skips: "A single success resets the count, so a profile that only misbehaves occasionally will not slowly build up to a trip.",
    desc: "Once tripped it enters a cooldown, during which the pool skips this profile. The state is stored in the database and survives restarts.",
    attemptsLabel: "Failures before tripping",
    defAttempts: 3,
    defInterval: "1min→5min→30min tiers",
    offHint: "-1 = transient failures never trip the breaker (deterministic failures still do)",
  },
  intent: {
    title: "Intent rerun",
    where: "This project · process-wide, one global copy",
    trigger:
      "None of the earlier layers caught it: the worker ended with model_error, either because the inner retries were all used up or because the stream broke after output had started being delivered (a replay is unsafe then, so the whole thing must start over).",
    skips: "Quota exhaustion is already handled by the pool switching profiles, so it is not rerun here; when the task is paused / stopped / finishing, it yields immediately without spending any backoff time.",
    desc: "Runs the whole intent again from the start. It is the outermost layer, so one rerun multiplies the attempts of all the inner layers once more.",
    attemptsLabel: "Reruns",
    defAttempts: 2,
    defInterval: "Fixed 3s",
    offHint: "-1 = no rerun; the intent is marked blocked straight away",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Milliseconds in plain words, only echoed next to the input so nobody has to count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled number input: empty string ↔ 0; intermediate states ("-", "1e") stay local as typed without bothering the parent. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // Follow along when the parent swaps in a whole new set of values (policy loaded, profile switched); local typing never gets here,
  // because by then value already equals the parsed local text.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** The two knobs of one retry layer. idPrefix keeps each label's htmlFor intact when it appears several times on one page. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = compact version for the profile drawer: drops the expanded explanation and keeps only the "which errors reach this layer" line */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* Which errors reach this layer, down to the status code: if a knob seems to have no effect, the error most likely never lands in this layer. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Trigger</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Not handled here</span>: {meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`Default ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            Interval ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="Default backoff"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `Fixed ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">Empty = use the default; {meta.offHint}.</p>}
    </div>
  );
}

/** The three overrides in the model profile drawer (the three layers that follow the endpoint). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">Retry overrides</Label>
        <p className="text-muted-foreground text-xs">
          Applies only to this profile and overrides the global defaults under "Retry and backoff". Empty field = follow global; attempts -1 = turn this retry layer off;
          an interval, if set, replaces exponential backoff with a fixed interval. The circuit breaker and intent rerun are process-wide and can only be tuned on the global page.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** "Retry and backoff" tab: global defaults for the five layers. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`Failed to load retry policy: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // The backend clamps out-of-range values and returns them; refresh straight from the response so what you see is what is stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved and in effect immediately (calls already in flight still use the old parameters)");
    } catch (e) {
      toast.error(`Failed to save: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> Loading retry policy...
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        A failed model call passes through five retry layers in turn, from the inside out:
        <span className="text-foreground"> connection → empty response → same-provider safe window → pool circuit breaker → intent rerun</span>
        . An outer layer only kicks in once the inner ones are exhausted, so the attempts are{" "}
        <span className="text-foreground">multiplied</span>
        : max out every layer and a single blip can burn through dozens of requests.
        Leaving everything empty gives the current defaults, exactly the same behaviour as before this page existed. The first three layers can be overridden in each model profile.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          Save
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          Reset all to defaults
        </Button>
      </div>
    </div>
  );
}
