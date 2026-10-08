// Channel field table and parsing helpers for config values.
//
// It is split from the page because this is **data**, not a view: it describes which fields each channel has,
// which control each one should use, and the two-way conversion between form text and config values (JSON).
// With it in its own file, adding a channel only touches this file and the page itself needs no change.
// Display names and descriptions of channel types. They live in the frontend because they only affect copy; the backend does not need to know.
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Feishu",
  wecom: "WeCom",
  webhook: "Generic webhook",
  telegram: "Telegram",
  email: "Email",
};

// Config field definitions for each channel.
//
// A frontend field table is kept here on purpose instead of having the backend send a schema: the backend only handles
// Validate (required/format), while the UI needs layout and control types; the two are not concerned with the same thing.
// The only coupling point is secret_keys: the backend says which fields should render as password inputs,
// because only the channel implementation knows which values count as credentials (WeCom's whole webhook is a credential,
// whereas for DingTalk it is just the one secret). A missing entry here for a new channel only leaves the form blank,
// it never fails silently (hasFields below flags it).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "Signing secret",
      kind: "password",
      help: "Fill in when the bot security setting is 'Signature'; leave empty for 'Custom keywords' or when no security setting is enabled",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "Signature verification secret", kind: "password", help: "Fill in when the bot has 'Signature verification' enabled, otherwise leave empty" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "Target URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "Request method",
      kind: "select",
      options: [
        { value: "POST", label: "POST (with body)" },
        { value: "PUT", label: "PUT (with body)" },
        { value: "PATCH", label: "PATCH (with body)" },
        { value: "GET", label: "GET (no body)" },
      ],
    },
    { key: "headers", label: "Custom headers", kind: "kv", help: "One KEY=VALUE per line, e.g. Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "Body template",
      kind: "textarea",
      help:
        "Leave empty to use the built-in default template. Variables: {{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}, " +
        "plus .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel under range .Items. " +
        "Insert strings with {{json .Xxx}} rather than {{.Xxx}}, otherwise quotes in a title will break the JSON.",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API URL",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "Leave empty for the official URL; fill in when going through a self-hosted Bot API reverse proxy",
    },
  ],
  email: [
    { key: "host", label: "SMTP server", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "Port",
      kind: "number",
      placeholder: "587",
      help: "587 uses STARTTLS; for 465 turn on 'Implicit TLS'",
    },
    { key: "username", label: "Username", kind: "text" },
    { key: "password", label: "Password / app password", kind: "password" },
    { key: "from", label: "From", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "To", kind: "list", help: "Separate multiple addresses with commas" },
    { key: "tls", label: "Implicit TLS", kind: "switch", help: "On for port 465; keep off for 587 (STARTTLS is used automatically)" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "Any" },
  { value: "low", label: "Low and above" },
  { value: "medium", label: "Medium and above" },
  { value: "high", label: "High and above" },
  { value: "critical", label: "Critical only" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV parses a "one KEY=VALUE per line" textarea.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs parses a comma/whitespace-separated list of ids.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords parses a line/comma-separated keyword list (vulnerability type names may contain spaces, so split on lines or commas).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
