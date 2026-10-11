# Changelog

All notable changes to this project are recorded in this file. The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Deployment and updates

#### Changed

- **This fork no longer depends on the original repository**, which is no longer public: the in-app updater checks this fork's Releases, the release workflow pushes the image to `ghcr.io/logan-han/artex` (with the workflow's own token) instead of Docker Hub, and the `norma` SDK v0.4.3 is vendored unmodified in `third_party/norma`.
- **The Docker image builds from source**: the `Dockerfile` now compiles the frontend and the binary itself, so `docker compose up -d` works from a plain checkout. `install.sh` and `update.sh` build the image locally when there is no published image to pull.

## [0.3.15] - 2026-10-07

### Intercept

#### Added

- **New built-in intercept rule "Delete endpoint paths"**: the built-in HTTP destructive rule used to recognise only the DELETE **method**, yet in most applications the delete endpoints can be triggered with GET/POST, so a call like `curl 'http://t/api/user/delete?id=1'` really deletes data on the target. A new `deny` rule now covers `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details` and `/delta` are not intercepted by mistake). Existing instances also get it after upgrading, and it can be disabled or deleted under "System → Command intercept".
- **Token usage of model fallback approval can be viewed separately**: approval calls used to have no metering of their own, so how much they cost was guesswork. They are now metered separately, and a usage card appears below the "Model fallback approval" switch under "System → Command intercept": running totals for five figures (calls, input, output, cache reads and cache writes), plus a mini bar chart of daily usage over the last 30 days.

### Vulnerability notifications

#### Added

- **Vulnerability findings can be pushed to IM**: a new "System → Notifications" page pushes scan findings to six kinds of channel: **DingTalk, Feishu, WeCom, generic Webhook, Telegram and email**. Each type can have any number of instances, and each instance has its own on/off switch, rate limit and filter rules.
- **Two delivery timing modes, real-time and digest**: real-time mode sends each finding as soon as it matches; digest mode merges them into one message per global period (30 minutes by default), opening with the number of new findings and the severity breakdown. For "High in real time, everything else in a digest", create two channels and configure each separately.
- **Filter rules cover four dimensions**: minimum severity, restriction to specific tasks / asset scope, vulnerability type keywords to include and exclude (exclude wins), and whether to receive triage status changes (off by default). Messages carry a "View details" button whose target is set by the global "deep link base URL"; leave it empty to omit the button.
- **Delivery history and manual resend**: lists each delivery's status, attempt count, failure reason and channel, filterable by channel and status; failed items can be resent with one click (resending resets the retry count).
- **Channel credentials are echoed back masked**: Webhook URLs, signing secrets, Bot Tokens, SMTP passwords and the like are only echoed back as masked values with a hint of the last few characters; submitting the value unchanged means "leave as is", and clearing the input deletes the field.

#### Fixed

> This section is the outcome of a security audit carried out after the first version of the feature was finished. Each item was reproduced first, then fixed, then covered by a regression test.

- **Fixed masking being bypassable by "change the target URL, keep the credentials" (Critical)**: config merging always kept the stored value for "keys not mentioned", so changing only the URL while saying nothing about the credentials made the server send the real stored credentials to an arbitrary address, completely silently. Now, as soon as the target URL changes, every credential field must be stated explicitly: either a new value, or explicitly left empty to say it is no longer needed.
- **Fixed markdown-based channels not escaping untrusted content at all**: vulnerability titles and summaries come from model output and asset URLs from scan results, so a crafted title would render as a clickable external link in DingTalk / WeCom / Feishu, and image syntax would be fetched by the client at render time, leaking the reader's IP. Content is now consistently flattened to a single line + markdown metacharacters are escaped, and the escaping happens at each channel's rendering output rather than in the shared title function.
- **Fixed the SSRF surface of delivery addresses**: loopback, internal and cloud metadata addresses could all be delivered to, and the first 200 bytes of a failed response body went into `last_error` and were echoed back by the delivery history, forming a semi-blind read primitive. The guard now sits at the **dial stage**, covering DNS rebinding, and cross-host redirects are refused; loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`, while RFC1918 private networks are still allowed (self-hosted SMTP relays on internal networks are common).
- **Fixed notification credentials leaking through error messages**: DingTalk's `access_token`, WeCom's `key` and Telegram's bot token all live in the URL, and when a delivery failed the full URL flowed into four places: `last_error`, the delivery history, server logs and frontend messages. It is now redacted everywhere, keeping only `scheme://host` and the underlying cause.
- **Fixed silent loss when a truncated digest message marked the whole batch as delivered**: the findings that were cut off were neither in the message nor in the failure list, yet the delivery history showed success. Messages are now packed by **whole entries** and only the entries that fit into this message are marked; the rest go back to the queue to continue in the next message without using up retry attempts, and the message header states how many will continue in the next message.
- **Fixed a typo in the minimum severity threshold silently disabling the filter**: with a typo such as `hgih` for `min_severity`, the unknown level had ordinal 0 and the check degenerated to always true, so users who thought they only received High findings actually had every vulnerability poured into the group. The value is now validated on the write path, listing the allowed values.
- **Fixed `rate_per_min = 0` (no rate limit) being unreachable**: the database write layer quietly turned an explicit 0 into the default, so operators thought they had lifted the rate limit while it was actually still throttling. The default is now filled in at the API layer when the field is omitted.
- **Fixed digest channels bypassing the token bucket entirely**: `rate_per_min` previously had no effect at all in digest mode. The number of entries claimed is now bounded both by this round's quota and by an in-memory cap.
- **Fixed failure handling going by the batch's maximum attempt count, so old deliveries dragged new ones down with them**: an old delivery already retried twice would drag brand-new deliveries in the same batch into failed, and new findings were lost for good without using a single retry. It is now decided per entry.
- **Fixed deliveries with unparseable snapshots in a digest batch being silently marked successful**: such entries were skipped at the render stage yet counted as successful along with the rest of the batch. They are now explicitly failed with a reason.
- **Fixed the number of deliveries per channel per round possibly outlasting the lease**: in multi-instance deployments, rows whose lease had expired were claimed again, sent twice and had their attempt count incremented twice. The per-round cap is now derived from "lease / per-send timeout", and an assertion pins down the relationship between these constants.
- **Fixed Telegram truncation possibly cutting HTML entities in half**: truncation only avoided half-cut tags, not entity fragments such as `&amp`, which could make the parser reject the **whole** message. Both are now avoided.
- **Fixed the email channel treating temporary SMTP failures as permanent**: 4xx codes such as greylisting's `450` are temporary rejections that should be retried later, but they were all treated as fatal, so a server with greylisting enabled made every notification fail after its first attempt. They are now distinguished by the first digit of the reply code: 4xx can be retried, 5xx is a permanent failure.
- **Fixed submitting a nested structure actually writing the mask literal into the database**: object fields such as `webhook.headers` can only be masked or submitted as a whole; putting the mask sentinel inside the object stored it as a real value, silently breaking authentication. Such submissions are now explicitly rejected.

#### Design notes

- **The transaction that writes a vulnerability does only one blind INSERT**: `notification_events` is written in the same transaction as the vulnerability, so a commit guarantees the two are atomically consistent. That INSERT deliberately does not read the channel table or run the user's filter rules, and it is wrapped in a `SAVEPOINT`; otherwise one misconfigured filter condition could abort the transaction and keep a high-severity vulnerability out of the database.
- **Deliveries are claimed with a lease rather than a long transaction**: after claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as a lease, so no database lock is held during delivery; rows left behind by a crashed process are claimed again once the lease expires, self-healing without retrying forever.
- **Rate limiting does not consume the retry budget**: this round's quota is worked out from the token bucket first, and then that many entries are claimed. The reverse order would charge an attempt to deliveries held back by the rate limit for nothing, and the three-attempt budget would be used up purely by waiting before they failed.

### Test infrastructure

#### Fixed

- **Fixed assets left behind permanently because of the cleanup order in the company ICP attribution test**: `defer d.Close()` ran first when the function returned and `t.Cleanup` ran after it, so every cleanup statement hit an already-closed connection, and the errors were discarded; test assets and companies stayed in the database for good, and other tests asserting on asset counts failed for no apparent reason. Closing the connection now also goes through `t.Cleanup`, registered first, and cleanup failures are made visible.

### Traffic

#### Added

- **The traffic list supports content search, path / status code / length filters and column sorting**: the filter row can filter by **response content** (using the full-text index), **path**, **status code** (an exact value or a bucket such as `2xx`) and **response length range**; the table header can sort by time, status code and response length, and the sort preference is remembered locally. Two new indexes take effect automatically once existing instances upgrade; the Agent tool's query contract is unchanged.
- **New "Clear all" in the traffic list**: deletes every traffic record in one go (ignoring the current filters) and removes old host directories the index no longer tracks; after clearing it also runs a full compaction and reports how much space was actually freed. Traffic evidence already bound to vulnerabilities lives in a separate evidence store and is not affected.

#### Fixed

- **Fixed disk space not being released after deleting traffic**: the index database was created without `auto_vacuum`, so the file never shrank; on top of that, the full-text index is `contentless_delete`, so `DELETE` only writes tombstones and does not reclaim the original postings. Deleting traffic actually made the index bigger (measured: capturing 6MB of bodies → a 16MB index, still 16MB after deleting everything). Newly created index databases now enable `auto_vacuum=incremental`, and after each delete the full-text index merge and incremental vacuum run in the background in chunks, gradually returning space to the system (the same scenario drops back to 104KB); reclamation releases the write lock between chunks and does not block traffic recording.

> Upgrade note: `auto_vacuum` can only be set when the database is created, so **index databases on existing instances stay in the old mode** and incremental vacuum is a no-op on them (a one-line notice is printed at startup). After upgrading, tombstone growth on such databases has stopped, and the space already taken can be reclaimed once with "Clear all": that step converts the database to incremental vacuum mode, after which everyday deletes take effect on their own.

### LLM

#### Fixed

- **Fixed the custom session header not taking effect on the "one-off LLM call" paths**: goal decomposition (round 0) and cold-node compaction are both one-off calls with no transcript store attached, so there was no session id on the context and the gateway never saw the header. With endpoints such as opencode zen that return 400 outright when the header is missing, this showed up as "round 0 fails while later planner rounds work perfectly". Both paths now explicitly attach a session id that is stable per exploration, which also lets token usage be attributed to the right exploration (previously it was not recorded at all).

### Tools

#### Fixed

- **Fixed output truncation not applying to MCP and custom tools, so large results flooded the context whole**: MCP tool results were not truncated at all, and HTTP custom tools, because they were passed an empty tool context, always used a hard-coded 6000 characters instead of the session's configured limit. Both paths now go through `actool.Capture`: output is truncated to the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is configured the full output is spilled to disk, leaving only the opening fragment and a file pointer in the message.
- **Upgraded norma to v0.4.3**: the harness layer applies `CaptureOnce` uniformly, so no tool (including ones added in future) can overflow the context again by missing out on truncation.

### Deployment

#### Fixed

- **Fixed the activity feed failing to connect behind a reverse proxy**: the SSE URL was unconditionally built as `${hostname}:8787`, so in deployments reverse-proxied to 443 the page loaded from `https://<domain>/` while SSE was pointed at port 8787, which is not open to the internet, and the activity feed kept reconnecting. It now depends on `NODE_ENV`: production uses the same origin by default, and only `next dev` keeps `:8787` to get around Next.js buffering SSE; the `NEXT_PUBLIC_SSE_BASE` override remains. The README now covers reverse proxy deployment.

### Findings

#### Fixed

- **Fixed the asset list in the "By asset" view having no scrollbar when it overflowed**: the outer card only had a `max-height` and no definite height, so the scroll viewport's percentage height could not be resolved; with many assets the list either burst out of the card or was cut off with no way to scroll. The height cap now sits directly on the asset tree's native scroll container and adapts to the window height.

### Database

#### Fixed

- **Capped the connection pool**: by default `database/sql` does not limit how many connections the application itself can open; when the pool has no idle connection it unconditionally opens a new one, all the way up to PostgreSQL's `max_connections` (default 100) before being refused. It is now capped at 32, and queries beyond that wait for an idle connection, so the same load shows up as slowness rather than errors; if you have lowered `max_connections`, lower this to match.
- **Improved connection reuse**: `MaxIdleConns` defaults to 2, so with more than 2 concurrent connections the extras were closed after use and the next request had to redo the TCP and PostgreSQL authentication handshakes. The idle cap now matches the total cap, plus a 30-minute connection lifetime and 5-minute idle eviction, so the pool naturally replaces long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task list

#### Added

- **New "Vulns" column in the task list**: shows the Critical / High / Medium / Low counts per severity tier, with non-zero tiers coloured by severity, so you can see each task's vulnerability volume and distribution at a glance.

#### Changed

- **Narrower "Description / Goal" columns**: overlong content is truncated with an ellipsis and the full text shows on hover, so long text takes up less horizontal space in the list.

### Exploration graph and planning situation overview

#### Changed

- **Slimmed down the planning situation overview (graph_overview) and capped every list**: recent facts, finished intents, pending intents, cold-region digests and confirmed vulnerability details all switched to "latest window + count as fallback", with anything not shown available on demand; this markedly reduces the LLM context size per round and keeps long tasks from bloating their context (the cold region of linked tasks' overviews is capped as well).

#### Fixed

- **Removed reverse duplicate edges between digest and finding in the exploration graph after folding cold nodes**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset intercept

#### Added

- **New global asset intercept rules (blocklist)**: exact-match and fuzzy-match domains / IPs / URLs as well as CIDR ranges can be entered, with create, read, update and delete plus enable/disable for rules; the management page is under "System → Asset intercept". Built-in defaults fuzzy-intercept government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites.
- **Asset intercept is wired into the execution chain**: before dispatching an intent (`add_intent`) or inserting assets (`insert_assets`), the Agent first runs the intercept check on the target assets: intents that match are not dispatched, assets that match are not inserted, and the asset details and intercept reason are returned to the Agent.
- **New task-level intercept / allow (allowlist) rules**: independent of the global rules and effective only for this task, evaluated "intercept first, then allow": matching an intercept rule means denied; if no intercept rule matches but the task has allow rules and none of them match, the result is "not allowed to test"; with no allow rules configured, the allowlist is not enabled. They can be entered when creating a task, or added, edited, deleted and enabled/disabled in the "Overview" of the task detail page.
- **Task templates support preset categories and task-level intercept/allow rules**: a template can store a task category and a set of task-level rules, which are carried into the new task form when the template is applied.

### Operation review

#### Fixed

- **Tightened the model judge's output protocol to reduce wrongful allows caused by truncation**: the verdict explanation (comment) limit was lowered from 500 Chinese characters to 120, and the output is forced to be "JSON only, no preamble or code block", so a verdict truncated by `MaxTokens` no longer fails to parse and gets allowed through under the model failure policy (fail-open).

### Task archives

#### Fixed

- **Archiving now skips symbolic links instead of failing the whole archive**: the archive format only supports regular files and directories end to end, so any symbolic link in the working directory used to make the entire task archive fail; the symbolic link is now skipped and logged, and the remaining files are archived normally (links are not followed and nothing escapes the directory tree).

### Account and compliance

#### Added

- **New "Terms of use and disclaimer" dialog before login**: you must tick the box to agree before you can log in.

### Licence and dependencies

#### Changed

- **The project adopts the AGPL-3.0 open source licence**, and the README's licence and disclaimer notes were expanded.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration chain broadcast board

#### Added

- **New "Exploration chain broadcast board" on the task detail page** (#144): presents the task's exploration nodes (start/goal/intent/fact/vulnerability/hint/compaction) as a running timeline, with filtering by type, keyword search, ascending/descending order, pagination and auto-refresh. On "the first page, newest first" it is the live position and refreshes every round; away from it, it only accumulates an unread count without disturbing what you are reading, and "N new entries · Back to latest" takes you back in one click. Entries are grouped by day, so even when paging through a long task you can tell "which day this happened".
- **Each broadcast board row shows the node id** (#145), making it easier to cross-reference the exploration chain graph and locate specific nodes.
- **Expanding a broadcast board node shows its upstream/downstream and anchored assets** (#147): expanding an entry shows the node's upstream (where it came from) / downstream (what it produced) relationships, and hovering over a related entry pops up that node's card (type/status/source/time/summary/payload snippet); the assets the node is anchored to are listed as well (type label + recognisable text). The related data is sent along with the broadcast page, so expanding sends no extra requests.
- **Broadcast board search can filter by node id** (#150): besides "content / source", the search box now also matches node ids; entering a plain number or the "#41" form shown in the UI pinpoints the corresponding node.

### Intent management

#### Added

- **Intent deletion supports two modes, "soft delete / hard delete"** (#149): intents that are unclaimed, running or paused can all be deleted, and a reason is required; the deletion mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is set to "Deleted" and the reason is recorded in a separate field; the intent node and all its outputs and lineage are kept.
  - **Hard delete**: physically removes the intent along with the exclusive descendant nodes "supported only by it" (cascading along output / intent chains down to the leaves), so no orphaned data is left behind; the deleted intent's token metering is archived and kept under its original dates; shared nodes (still referenced by other intents), goals and task root facts are always kept, and the hard delete dialog shows the expected number of nodes the cascade will affect.

  Both modes notify the planner that "this intent was deleted by the user + the reason", and it replans accordingly.

### Operation review

#### Added

- **Approval records can be filtered by status and verdict source** (#139): filter approval records by approval status and verdict source to find the record you want quickly.

#### Fixed

- **Pending approval requests load independently of history pagination** (#133): pending requests are no longer affected by the history list's pagination and stay fully visible while you page through history.

### Agent

#### Changed

- **The Worker role description now uses generic cyber-security platform wording** (#138).

#### Fixed

- **Fixed cold-node compaction never triggering**: the per-task planner is now wired to the Compactor; cold-node compaction (cold-digest) never ran before because it was not wired in, and it now works again.

### Chat

#### Added

- **Chat supports @ mentions of several record types with scroll pagination** (#135): you can @ mention many types of records in chat, and the mention candidates load page by page as you scroll.

#### Fixed

- **Long message bubbles are kept inside the conversation panel** (#137): overly long message bubbles no longer overflow the conversation area.
- **Fixed uploading a file before a new conversation was created failing with "missing upload file"**: the Composer's file picker now snapshots the `FileList` into an array before clearing the input and then calls back; previously, in the draft state the conversation had to be created asynchronously first, and by the time execution resumed the `FileList` live-bound to the input had been cleared, so the upload lacked the file field and the backend returned 400.

### Traffic

#### Fixed

- **The traffic recording proxy listens only on 127.0.0.1 by default** (#129, #130): so the default configuration does not expose an open proxy on other network interfaces.

### Web

#### Fixed

- **Fixed the task list page losing the global header in the static export build**.
- **Added the missing linked-traffic mock for the demo vulnerability detail page, fixing a blank screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation review

#### Added

- **Approval records can locate their source** (#125): clicking an approval record's "Source" jumps back to the exact tool run that triggered that approval: it opens the original full session, auto-paginates to the target position, expands the command and result in place and highlights it centred in the scroll area, with the surrounding messages kept readable; automatic correction stops when the user scrolls manually. Across regular conversations, task Workers, the planner and the main Agent's segmented sessions, it locates the call precisely via the persisted `tool_use_id` and task mapping (with a new scoped call ID index); when the call is missing, duplicated or ambiguously linked it says so explicitly instead of jumping to a different run. Clear messages are shown when the session has been deleted or the task archived / the record is missing.
- **Approval records support pagination** (#115).

#### Changed

- **Slimmed down the model review input** (#124, #125): the input for model review (the LLM judge used after no rule matches) is narrowed to versioned JSON of "the current complete tool call + a short, explicitly selected background + the local working directory". The background takes only real user messages (the current user message of chat / the task's main Agent); Workers no longer attach an intent summary and also drop background inherited from the parent Agent; the planner and auto-triggered sessions do not fabricate user messages. The task description, goals, operation constraints, global exploration situation, call history and the Worker's full intent are no longer attached: these are still used as before for Agent execution and independent session audits, they just do not go into operation review. Every verdict must output JSON `decision`/`comment`, and the explanation must include "Actual action", "Consequence if successful" and "Matched rule"; instructions inside arguments or the background cannot change the review policy. The input snapshot actually sent to the model is saved along with a fingerprint; old snapshots are kept and labelled with their version, and old inputs are not reconstructed from current data.

#### Fixed

- **Model verdicts wrapped in a code block are no longer silently allowed** (#126): if the verdict JSON returned by the model was wrapped in a ``` code block, parsing failed and the call was silently allowed through the model failure policy; the code fence is now stripped before parsing, and the configured failure policy applies only if parsing still fails.

### Agent

#### Added

- **New experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature that can be turned on in System settings, off by default. When on, noa (model-driven context compaction) takes over context compaction for four kinds of Agent (main Agent, planner, Worker and chat), replacing the built-in compaction; the original compacted text is archived persistently, all under `<workDir>/noa/<sessionID>/` (rather than scattered across task directories), with one globally unique directory per session ID. If hooking it up fails it falls back to the built-in compaction automatically without interrupting the real task; the switch is read once per run, so toggling it only affects runs started afterwards.

#### Fixed

- **Exploration graph tools refuse instead of crashing on nil when there is no task context**: calling exploration graph tools outside a task context returns a clear error instead of crashing on a nil store dereference.

### MCP

#### Added

- **Support for legacy SSE MCP servers** (#117): compatible with MCP servers that only offer the legacy SSE transport.

### Traffic

#### Fixed

- **Traffic search matches record hosts port-aware** (#114): `traffic_search` host matching now includes the port, so records for the same host on different ports no longer bleed into each other.
- **An error in the `traffic_search` description migration no longer aborts the subsequent reporter migrations**: a single failed migration is isolated and does not affect the migrations after it.

### Web

#### Changed

- **Added dividers between the task retest vulnerability options** (#122): separators between the vulnerability options for a retest make them visually clearer.

#### Fixed

- **Fixed the whole demo task detail page crashing**: in mock mode the task detail "Sessions" tab fetches `GET /api/tasks/<id>/side-questions` (side question history), but the mock handler had no such route, and the read fallback returned `[]` on the basis that "a path ending in s is a collection", so `data.items` was `undefined` and the side-question hook's `merge()` threw `TypeError: t is not iterable` while iterating it; the exception happened inside the `setItems` updater, was deferred by React and rethrown in the render phase, where the caller's `catch` could not catch it, so the error boundary took over the whole page and showed "This page couldn't load". The mock handler now explicitly returns an empty side question history, and `sideAPI.history` also normalises its return value (`items` that is not an array always becomes `[]`) as defence in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **New official DeepSeek source for web search**: reuses the currently active LLM profile directly. It differs in nature from the other three sources: DeepSeek has no directly callable search API, and search exists only inside its Anthropic-compatible API (the `web_search_20250305` server tool), executed by DeepSeek's servers. So it **only supports the official DeepSeek endpoint + the anthropic protocol** (OpenAI-protocol endpoints reject server tools outright), every search **costs an extra model call**, its requests **do not go through the search egress proxy** and are **not included in the traffic audit trail**, and results contain **only titles and links** (no summaries; fetch the body with WebFetch when needed). The settings page explains these limitations but **does not validate or block**; it is up to the user to confirm the conditions are met, and the "Test search" button runs a real search to verify.

### Agent

#### Added

- **Workers can look back across works**: new `search_all_worker_traces` (no need to know the intent_id first; searches the execution traces of every work in this task by keyword for matching steps) and `get_worker_trace` (once a work is pinned down, lists its step stream, searches it in place by keyword and fetches full content by step_id), used to reuse observations other works saw but never wrote into a fact, avoiding duplicated effort.
- **`node_detail` is now available to workers**: together with the look-back tools above, once a worker has an intent_id / node id it can look up that node's full details directly.
- **Planning rounds triggered by `add_hint` are announced explicitly**: previously a new hint was only folded into the situation overview for the planner to discover on its own; now each `add_hint` records a trigger for the planner saying "a human added N strategic hints: ..." (a batch counts as one, so it does not spam one per hint), and the planner is told explicitly "this round was triggered by new hints" and sees the hint content directly.

#### Changed

- **The global situation prompt now takes a "loosened" tone**: it encourages divergent exploration and prompt reporting of cross-intent leads, rather than converging too early.
- **Tightened the worker boundary wording**: being blocked the first time does not mean the direction is exhausted; the focus is on "working through every bypass technique within this intent before drawing a conclusion".
- `insert_assets` drops the per-asset `related` parameter: its only purpose was deciding whether to add the asset to this task's scope, yet the value was never persisted (it was voided as soon as the same asset was registered again, and the UI gave no way to see which assets were judged unrelated), so in practice it just made the model make one more judgement that could not be kept.
- **The test scope (`task_scope`) is no longer affected by the asset coverage switch**: `insert_assets`' automatic scope addition (`source='auto'`) runs whether coverage is on or not, and `add_task_scope` is always provided to the planner / the in-task main Agent / the goal decomposition Agent. Scope is the task's authorisation boundary and the filter for asset queries; the coverage switch only decides whether to use it as the denominator for metrics, and should not decide whether the scope itself accumulates. Previously, turning coverage off disabled both the `auto` and `agent` write paths, leaving `task_scope` with only rows added by hand in the UI. `list_untested_assets` is still hidden when coverage is off (it is purely a coverage view).

#### Fixed

- **Fixed assets leaking between tasks** (#59): `list_assets` hard-coded the task id to 0 and queried the whole shared asset store unfiltered, and the model had no way to write a scope filter, so it treated other tasks' assets (especially IPs) as this task's targets and the testing went off course. It now returns only assets within the test scope (`task_scope`) of [this task and its directly linked tasks], matched by **ownership** rather than literal value: a root domain in scope finds all the subdomains/services/endpoints under it, and a CIDR range finds the hosts and services in it; fetching an out-of-scope asset directly by id fails as well. Outside a task context (Auto/pentest) there is no scope to go on, so it still falls back to the whole store. This also fixes an ownership blind spot where hosts reached directly by IP (such as `http://1.2.3.4/api`) could not be matched by a CIDR range scope. The UI's "Test assets" view (filtered by task producer) is not affected.
- **Fixed the worker cross-work look-back tools being removed by mistake**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker's default tool set, an old "narrow the worker tool surface" migration unbound them again at startup, so workers never actually got these tools. They have been taken off that unbind list, and databases that already ran the old migration get them bound back to the worker once.
- **Fixed `/btw` side questions occasionally failing to save**: the side_question checkpoint now strips NUL (`\u0000`) escapes, which JSONB does not support, before saving, so content containing that character no longer errors on write.

### Triggers

#### Fixed

- **Merged trigger sessions deduplicate task descriptions/goals per task**: when triggers from several tasks are merged, the same task's description/goals are no longer appended to the message repeatedly, so long goals do not pile up and blow out the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- The in-task main Agent supports **multiple sessions**: create them, switch between them and reset their context independently, and interact in each session.
- Support for persistent `/btw` **side questions**: ask follow-ups without interrupting the main thread; questions and answers are saved to the database and survive restarts.
- `spawn_task` gains `source_task_ids`, letting a new task **inherit read-only** the source tasks' assets and findings.

#### Changed

- Turned off workers' cross-engagement memory and narrowed their default tool surface: reading context and cross-work review are planning duties, while a worker only handles executing a single intent and writing back its results.
- Slimmed down the worker default prompt; `get_worker_output` drops the `terminated` and `worker_name` fields; slimmed down the `insert_assets` / `list_assets` descriptions.

#### Fixed

- Fixed works ending early because of an **empty turn**: the model sometimes spends a whole turn producing only thinking, with no text and no tool calls; the harness then sees a natural end (`end_turn` with no `tool_use`) and finishes straight away with `completed` + an empty summary, so an intent that was not done yet was cut off midway, showing up as "the task ended normally but without any written summary". None of the five LLM retry layers could reach it: it is not an error; the same-provider safe-window retry is entered on "stream failure"; the circuit breaker counts `err == nil` as success; and intent rerun only reacts to `model_error`. The SDK's empty response retry could not reach it either, because that layer judges emptiness by "whether any event was yielded", and thinking deltas are themselves events. Now the work's Stop hook recognises such a turn and injects a continuation instruction so the model carries on to the next step with the thinking it has already produced. "Resend as is" was deliberately not chosen, because this kind of empty turn is usually determined by the shape of the prompt and context, a stable behaviour rather than a random glitch, and resending would only make the model think it all over again.
- The count reuses the **empty response retry count** from the "Retry and backoff" tab on the LLM page (both answer "the model finished but produced nothing of substance", just with different criteria and means); the default is 2, and `-1` turns it off, reverting to the old "empty turn means finish" behaviour. It is a **total** cap per intent rather than a consecutive count: the harness itself already limits consecutive empty turns to a single nudge, and the quota only refreshes after a real tool turn, so this number stops loops like "tool → empty turn → nudge → tool → empty turn" from using up the intent's budget. When it triggers, the log records `[work <worker> · #<intent>] empty turn...injecting continuation instruction (n/N)`. See `docs/LLM-retry-design.md` §1.1 for the design.
- Fixed `/btw`'s context budget and input layout in long conversations; in non-secure contexts (access without HTTPS) side-question request IDs are generated with a fallback.

### Traffic

#### Added

- Vulnerabilities can **link multiple pieces of traffic evidence**, which can be ordered, annotated and distinguished by role (request/response/supporting).
- The report Agent **automatically links** the relevant traffic evidence before writing the report.
- New Agent **traffic binding switch**, and the handover of evidence between Agents is now complete.

### Tasks

#### Added

- Support for **session-level vulnerability retests**: retests run in a separate Agent session, and the list and detail views show their running status.

### Intercept

#### Added

- Approval records gain **details and an execution audit**: view the tool request context, the initial model/rule verdict, the execution output and the argument fingerprint.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed the connection test not sending the custom session header, which made opencode zen return 400.

### Network

#### Added

- MCP's HTTP transport can **skip TLS certificate verification**, making it easier to connect to services with self-signed certificates.

### UI

#### Added

- The session list is grouped by Agent, with expand/collapse and separate pinning; conversations can be filtered by Agent.
- The attack chain graph renders compaction (digest) nodes and collapses their members.

#### Fixed

- Fixed a blank screen caused by login credentials getting out of sync.
- The intercept message now explicitly says "platform control", so it is not misread as a defence on the target side.

### Deployment and updates

#### Added

- Support for **one-click update in the UI**: the System settings page gains a "Version and updates" card, and the top bar shows a notice when a new version is available. The flow is: download the release package → verify against `SHA256SUMS` → smoke test → stage → exit and let the supervisor script restart it to complete the swap; the page refreshes automatically.
- New supervisor start scripts `start.sh` / `start.bat` as the official entry point (included in both the release packages and the Docker image; `install.sh` is unchanged). They use the exit code to decide whether to restart and forward SIGTERM to artex. The verification and swap logic all lives in Go, and the scripts stay dead simple.
- Automatic fallback on failure: if verification or the smoke test fails, the update is discarded and the current version keeps running; if the new version fails to start 3 times in a row, it rolls back to the previous version. The settings page also offers a manual roll back (note that the database schema is not rolled back).
- Updates only accept GitHub domains and enforce HTTPS, and the release source is not configurable; one-click update is disabled for development builds. GitHub query results are cached for 30 minutes so the top bar notice does not exhaust the API quota.

#### Known limitations

- In Docker only the program is replaced, not the image: the toolchain is not upgraded with it, and recreating the container reverts to the version baked into the image; use `docker compose pull artex` when needed.
- `skills/` from the release package is not synced, so built-in skills added in a new version do not take effect automatically.
- An update is a restart, so it interrupts running tasks.

### Dependencies

#### Changed

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- New "Retry and backoff" tab on the LLM page: the **attempts** and **interval** of all five retry layers are configurable. A failed model call passes through five layers from the inside out: connection retry (SDK; connection resets / timeouts / 429 / 5xx before the stream starts), empty response retry (SDK; a normal finish with no content at all, openai format only), same-provider safe-window retry (replaying a broken stream before any output has been delivered to the caller), pool circuit breaker (cool down and skip once consecutive failures reach the threshold) and intent rerun (the whole intent reruns after the worker finishes with model_error); an outer layer only gets its turn once the inner one is used up. Each layer has two knobs with uniform semantics: empty = use the original default attempts and exponential backoff; an attempt count = use that count; an interval = replace exponential backoff with a fixed interval; -1 = turn this retry layer off. The first three layers follow the endpoint, and each model profile can override the global defaults field by field (a profile that pins only the interval still inherits the global attempts); the circuit breaker and intent rerun are process-wide, with a single global setting. Changes take effect on save, with no restart needed. Leaving everything empty keeps the current behaviour, and old databases are byte-for-byte unchanged after upgrading (new columns default to 0, and a missing settings key means all defaults). The connection test deliberately ignores these parameters: it has a hard 30s timeout, and stacking user-configured retries on top would only make a working endpoint fail the test with a timeout. See `docs/LLM-retry-design.md` for the design.
- Each LLM profile supports a custom **session header** (`session_header_key`): when set, every LLM request carries that HTTP header, with the current run's session id as the value (`conv-<id>` for chat sessions, `exp<x>-worker-i<intent>` for workers, and so on), for gateways that do prompt caching / sticky routing on a session-id header. It is implemented by reading the session id from the request context and injecting it, with no change to norma, so the same shared provider can send different header values per session. Old databases get a migration, and the frontend LLM profile dialog gains an input box.

#### Fixed

- Fixed saving an LLM profile failing straight away with `23502` when the session header field was empty: the column is `NOT NULL DEFAULT ''`, but `NULLIF($n,'')` was wrongly applied, writing an unfilled session header as NULL and tripping the not-null constraint; the value is now passed directly with empty-string semantics.

### Agent

#### Added

- Wall-clock timeouts now **wrap up in place**: relying on norma v0.3.6, when `MaxDuration` is reached the running tool is interrupted and the run continues on the live ctx for the wrap-up turns (writing back what has been identified + a summary, with timeout as the final state), instead of relying on the separate `maxDur+90s` external hard ctx of the worker/planner to kill a stuck run outright as `aborted_tools`. Chat goes through the same harness and gets the same behaviour automatically; mainagent has no `MaxDuration` and is unaffected.
- Stall fallback: when the planner is woken by a heartbeat / with no changes and the whole graph has no open or running intents left, its opening message becomes a stall alert, stating explicitly that no worker is running and no direction is queued, and that this round must produce one or more distinct new intents (producing 0 intents is not allowed).

#### Changed

- Worker prompt restructured: the intent / launch instruction / raw JSON of the anchored assets moved into the system prompt, rebuilt every turn and never compacted away, so continuing a run no longer depends on the transcript's first message surviving; the launch user message is slimmed down to just the global situation overview (can degrade, tolerates staleness). The cost is per-intent data mixed into the system prompt and losing cache reuse across intents, a deliberate trade-off for "never losing the intent".
- Slimmed down the planner / worker default prompt bodies and fixed several real-world problems: the planner treats `recent_done` by status (for blocked/exhausted, check the trace before deciding, neither treating it as a dead end nor blindly rerunning), negative conclusions become "observation / doubtful, not final" with the evidence checked before accepting them, it must produce intents when the goal is unmet and no intent is open/running (a hard floor), and depth takes priority over coverage; workers write negative conclusions only as "observation + tentative reading" with the verdict left to the planner, and cross-intent leads go into the fact's summary for the planner instead of being chased by the worker itself. reseed appends the new defaults as a new version and switches to it; user customisations / old versions stay in the history and can be rolled back to.
- Narrowed the work agent's default tool set and asset write-back prompt: a worker is only responsible for executing a single intent and writing back the results, while reading context / cross-work review are planning duties, so its default tools drop `list_facts` / `node_detail` / `list_companies` and cross-work search (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`), keeping only `list_findings` (to check for duplicates before reporting a vulnerability) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; the prompt drops stale field descriptions that conflicted with the `insert_assets` schema (`type=tech`/`on_url`/`props`). Old databases get a one-off migration that strips the corresponding bindings, leaving the planner/main bindings of the same names untouched.

### Exploration graph

#### Added

- **Exploration graph cold-node compaction (cold-digest)**: intents / facts that are old and have long been inactive are folded into digest nodes shown in `graph_overview`; the original nodes are kept forever and can be fully restored by id (lossless storage, only the presentation is compacted, folding is reversible). Hot/cold is decided by reverse reachability, where any live branch means hot, with R=6-round debouncing and grouping by connected component; background compaction (minor folds uncovered cold blocks, major goes back to the source to recompact and merge fragments) re-checks activity and uses a cooldown and mutual exclusion, never touching the hot path and never overwriting revived nodes. The overview side provides `cold_digests` and a per-asset `cold_index`, with `expand_digest` / `expand_index` doing the restoring. Overviews of linked / inherited tasks likewise reuse those tasks' own folded views, and `expand_digest` supports read-only restoring across tasks. `expand_digest` / `expand_index` are given only to the planner and the main agent, not to workers. All of this comes with migrations for old databases.
- `graph_overview` outputs the full `finding_list`: findings are a task's most valuable output and a single task usually has few of them, so the overview now carries all of them (unlike facts, which only get a recent window); the planner/worker can see every confirmed vulnerability at a glance each round without calling `list_findings` again. Each entry is trimmed to `{id, summary, evidence?, from_intent?, assets?}`, where affected assets are given as readable content (url / domain / ip:port) rather than bare ids.

#### Changed

- `graph_overview` no longer lists `hosts` flat: the coverage block drops the host list (in large-scope tasks it carried up to 500 host strings every round, of limited value for planning decisions) and keeps only `host_count`; specific hosts can be looked up with `list_assets` as needed. A new top-level `done_intents_total` (the total number of finished intents) sits alongside `recent_done_intents`, which is truncated to ≤15, so the planner knows whether anything was cut off when deduplicating.

### Tools

#### Changed

- `add_company_scope`'s default binding moved from worker to planner: defining a company's asset scope is the job of planning / main control / Auto, while workers only carry out exploration. New databases seed the default bindings as mainagent/planner/auto, and old databases get a one-off migration.
- The planner binds `list_assets` by default: it now has both `list_assets` (DSL search over the whole store) and `list_untested_assets` (untested within scope); old databases are backfilled once, without overriding bindings the user removed.

### Tasks

#### Added

- New "Running workers" column in the task list: counts the task's intent nodes with `state='running'`, measured exactly like "Running workers" on the task detail overview, and shows 0 when no Worker is running.

### Network

#### Added

- New **global egress proxy** setting: all target traffic can go out through one global proxy (http/https/socks5, with `user:pass` support). With traffic capture on, it serves as the upstream of the MITM recording proxy (traffic is still recorded and then leaves through the proxy; both the intercept and pass-through paths use the upstream, so the source IP is not leaked); with capture off, it is injected directly into the agent's bash environment and WebFetch (`proxyEnv` adds `ALL_PROXY` to support socks5). The setting is stored in the settings KV table (no migration needed), and the frontend System settings page gains a "Global proxy" card, independent of the web search proxy and the LLM proxy.

### UI

#### Fixed

- Corrected the semantics of the intent status labels: `exhausted` "Exhausted" → "Out of budget" (it actually means the run was cut off midway on reaching its step / time budget and wrote back only partial results, not that the direction was fully explored), `blocked` "Blocked" → "Errored" (it actually means model / API / network failures used up the retries and the intent was barely explored, not that the target / a WAF blocked it), and added the previously missing `stopped` "Stopped" (a work stopped manually by the user).

### Dependencies

#### Changed

- Upgraded norma to v0.3.4: MCP tool output gains truncation and spilling to disk (see `651b961`); the later v0.3.6 provides in-place wrap-up on wall-clock timeouts.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model profiles gain an "Output limit", with a choice of which request field name carries it: the limit caps how many tokens a single reply may generate and is sent with every request; 0 (default) = the field is not sent and the server default applies. It is not the same thing as the "Context window": that is the model's total capacity, used only locally to compute the compaction threshold, and never appears in requests. The field name switch only matters for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, the only one most compatible gateways accept; OpenAI's own reasoning models (o series / GPT-5) conversely only accept `max_completion_tokens` and reject `max_tokens` outright with `unsupported_parameter`, so such endpoints need to be switched to the new field by hand. The other two formats each have a fixed field name (`max_tokens` for Anthropic, `max_output_tokens` for the Responses API), so for non-openai formats the switch is greyed out and cleared on save. This also wires up a path that had never been connected: none of the five places (planner / worker / chat / main Agent / goal decomposition) ever set an output limit, so the openai family never sent one and Anthropic fell back to the SDK's 8192; adding the setting without wiring this up would have meant an entered number had no effect. The value is now resolved per profile every turn, like "Streaming output", so after a failover switch it applies from the next turn. Old databases behave exactly the same after upgrading (new columns default to 0 and an empty field name).
- Clicking "Test" in the admin console now writes each HTTP attempt's status code and the gateway's raw response body to the server log (bodies trimmed to 4K), making it easier to diagnose 401s, quota messages, empty frames, HTML pages being returned and so on, instead of only seeing the ok / err the UI collapses it into. This logging does not depend on the "LLM recording" switch.

#### Changed

- A task's LLM profile chain can now be changed in any state, no longer limited to "running / paused / chain exhausted": after a task ends (done / failed / timeout) the main Agent conversation still uses this chain, and when a model on the chain had problems it could neither be changed nor could the interaction continue. Both terminal-state checks, in the backend HTTP handler and in the DB transaction, were removed, and the frontend dialog also allows editing and saving for tasks in a terminal state. Saving for a terminal-state task no longer reopens quota-blocked intents (those intents would be moved to open, with no worker to run them and no longer meeting the conditions for "Rerun intent"); to keep going, use rerun intent / add goal as before, which bring the task back to the running state.

### Agent

#### Changed

- "Message a running Worker to adjust its direction" now reuses the existing pause / resume + transcript resume mechanism and behaves the same as the main Agent conversation: it used to rely on a custom intervention persistence protocol that touched the scheduling barrier, the resume flow and more than ten activity query filters. Messages are now injected through the next turn's input, and the intent runs directly in a dedicated goroutine, not limited by the 3-slot worker pool, running as soon as a message is sent; the frontend keeps the message box and brings back the "Resume directly" button, with sending now carried over SSE. No DB schema changes. Trade-off: messages are kept only in memory with no crash recovery, and sending a message may momentarily put the task one worker over its concurrency limit (a rare case, acceptable).

### Tasks

#### Fixed

- Fixed out-of-memory (OOM) crashes when archiving large tasks: archiving now streams the snapshot as it writes instead of reading the whole task into memory at once; it also closes three recovery gaps on the cold archive path. On modern installs traffic lives only in SQLite, and a crash between the PostgreSQL commit and the SQLite commit used to leave an archived task's traffic stranded in hot storage with no way to recover it; a staging journal is now always written whether or not a legacy directory exists, and a missing `journal.json` is treated as discardable.
- Fixed the system stuttering when there are many tasks: loading conversations and loading pages had noticeable delays. Queries for the task list, task context and exploration records were optimised together, and the frontend dashboard, chat page and task detail page now make fewer duplicate requests.

### Assets

#### Changed

- Removed the "at most 256 rules" limit on a company's asset scope: companies whose scope is entered one IP / domain at a time easily hit that cap, and once they did they could only be split into several companies, even though a larger rule count does not make the scope itself any slower. The cap was removed in all three places (frontend, backend and demo mock); a single rule is still limited to 1024 characters, and the 2 MiB request body limit stays as a safety net (roughly forty to fifty thousand rules).

### Skills

#### Fixed

- Fixed uploading a skill archive failing with "Upload failed: zip: unsupported compression": the Go standard library only has two built-in decompressors, Store / Deflate, so bzip2 and Zstandard archives written by compression tools at non-default levels could never be extracted. Both decompressors are now added (pure Go, no new external dependencies); for methods that genuinely cannot be extracted, such as Deflate64 / LZMA / XZ / PPMd, and for encrypted archives, a clear message is now shown before extraction, naming exactly which file uses which compression method and how to repack it, instead of throwing the raw low-level error at the user.
- Fixed filename validation inside skills rejecting Chinese filenames as illegal: path validation used to be an ASCII allowlist of `[A-Za-z0-9-_./]`, so a single Chinese-named file in an archive (such as `参考/说明.md`) made the whole upload fail with "Archive contains an invalid path". It is now a Unicode denylist: filenames in any language and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |` and `..` / absolute paths / empty path segments are still rejected, so the directory traversal protection is unchanged. Skill names are relaxed too: ASCII is still limited to lowercase letters, digits and hyphens (the agentskills.io spec), while Chinese and other non-ASCII letters can be used directly in skill names, though spaces, dots and path separators are not accepted.
- Fixed archives with Chinese filenames made by Windows compression tools ending up with garbled filenames after extraction or being rejected outright: such zips do not set the UTF-8 flag and write filenames in GBK, so they are now decoded with GBK as a fallback before path validation. Quoted frontmatter such as `name: "中文技能"` now also yields the correct skill name.

### UI

#### Added

- The Findings page gains an "All findings" flat view, now the default, switchable with the original "By task" grouped view via tabs in the page header: in the grouped view you had to expand tasks one by one to see vulnerabilities, a detour when you just wanted to scan the list across tasks. The flat view is one big cross-task table (10/20/50/100 per page) that behaves exactly like the grouped view: select to export, expand a row to see the evidence and detailed report, edit name/category/severity inline, change triage status, dig deeper, delete. Both views share the stats cards and filter bar, switching keeps the filters, and the current view and filters are remembered locally; polling only hits the current view, and inline edits are written to both caches, so switching over never shows stale data.
- The Findings page gains a "By asset" view: an asset tree on the left (company → root domain / IP → subdomain → service → endpoint, with the company level appearing only when the assets actually belong to one) and, on the right, the findings under the selected node's whole subtree, sharing the same table and the same filters as the other two views. The tree only includes assets that have findings, filling in the ancestor chain as needed (when a finding hangs only on the deepest endpoint, the levels above are still assembled), and the counts on nodes are aggregated over the subtree and deduplicated by finding; findings whose asset was deleted or that never had a linked asset go into an "Unassigned assets" bucket. Unlike the other two views, the asset view does not poll: it queries only when you enter the view, change filters, add/edit/delete findings on the page, or click the refresh button on the tree, since the left-hand tree is a navigation structure with no need to be recomputed every 5 seconds. Each level of the tree shows only what it adds relative to the level above (subdomains drop the root domain suffix, services show `https :443`, endpoints show only the path), with full values in hover tooltips and the breadcrumb; when there are too many nodes, whole endpoint/service levels are dropped with a notice, and their counts still roll up into the level above.

#### Fixed

- Fixed the session transcript being squeezed and cut off in session details on mobile: on narrow screens the session list collapses, giving the width to the transcript itself.

### Dependencies

#### Changed

- Upgraded norma v0.3.2 → v0.3.3: adds `Config.MaxTokensField`, so the OpenAI Chat Completions output limit can be sent as `max_completion_tokens` instead (reasoning models only accept that key). The two keys are mutually exclusive and only one is sent; the default is still `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security alerts.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the "non-streaming" switch reverting to "streaming" when reopened after saving (#69): the profile list API's DTO was missing the `streaming` field, so the response never returned it and the frontend, reading `undefined`, always fell back to the streaming default; the actual write and DB storage were fine, the value just could not be read back. The DTO now includes the field again (without `omitempty`, since `false` must also appear in the response).
- The connection test now checks that the model actually replies, and tests using the profile's real send/receive mode (#65): it used to check only whether HTTP succeeded, so a request that went through with zero reply from the model (thinking burned the whole budget / the text was swallowed by a safety policy / a compatibility layer dropped `content`) still reported "Connection successful", at odds with the "no reply" seen in sessions; and the test always used streaming, so for non-streaming profiles it was really testing a different channel. Now an empty reply fails outright, a success shows the model's reply in the message, and the current streaming switch is carried into the test, so "test passed" and "sessions work" stay consistent.

### Agent

#### Changed

- `list_facts` now paginates + filters by keyword, so a single call no longer blows up the context when there are many facts (#74): it returns the latest 20 by default, supports `limit` (max 100) / a `before` cursor / `q` summary keywords, and returns `{facts, total, has_more, next_before}`; an overly long summary is truncated by length (the full text is still available via `node_detail`). The worker / planner prompts were updated to the pagination semantics. Old databases get a one-off migration that writes the new parameter schema into the tool catalogue table (`SeedTool` only inserts on first seed; otherwise the tool management page would show "No parameters").

### Tools

#### Added

- The "Tool runs" page gains tool call count stats (#72): the "Stats" button in the toolbar opens a dialog showing, per tool, the call count, share and failure count (sorted by count, descending); it reuses the list's task / keyword filters, covers the whole result set rather than the current page, and only fetches when the dialog opens.

### Dependencies

#### Changed

- Upgraded norma v0.3.1 → v0.3.2: fixes reasoning/refusal being silently dropped from OpenAI-family responses (Responses gains `reasoning_text`), deduplicates the three names of the reasoning field, adds bounded retries for empty responses (zero content blocks), and fixes gateway 400s caused by compaction boundaries splitting tool pairs (also repairing existing orphans already baked into transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM profile can switch between streaming and non-streaming (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, the complete JSON returned in one go), which works around some gateways' poor SSE implementations (empty frames, dropped thinking-field frames), at the cost of losing live progress and live token counts while running. worker / planner / mainagent / chat / goals all pick the value up dynamically from the currently active profile; the three Provider wrapper layers (`llmpool` / `llmrec` / the task runtime) all support non-streaming; `llm_profiles` gains a `streaming` column, added to old databases with `ALTER` (default `true`, so existing profiles notice nothing).
- Support for LLM profiles in the OpenAI Responses API format: each profile gains a third format, `openai-responses` (calling `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` is normalised and the default model is `gpt-5`; the `format` constraint on `llm_profiles` now includes `openai-responses`, with an idempotent migration for old databases; the frontend format dropdown gains "OpenAI (Responses API)". Dependency upgraded to norma v0.3.1 (including a fix for passing back `reasoning_content`).
- Records the raw HTTP of LLM requests/responses: the real wire body is captured at the HTTP transport layer, keeping the tool schemas, `tool_use` blocks and raw SSE frames that the normalised view cannot show; each attempt of norma's internal retries is kept separately; `llm_records` gains two columns, `raw_request` / `raw_response`, added to old databases with `ALTER`; the recordings page detail panel gains a "Raw" view toggle and request/response copy buttons (with an `execCommand` fallback for non-secure contexts).

### Chat

#### Changed

- Multi-select in the conversation list is now a "Select" mode toggle: the default list no longer shows a checkbox on every row (a cleaner look), and the header now shows "N total" + a "Select" button; clicking "Select" enters selection mode (checkboxes, select all, bulk delete), "Done" exits and clears the selection, and after a fully successful bulk delete it exits automatically (if anything failed it stays in selection mode so you can retry); renaming / pinning / deleting a single conversation still goes through each row's ⋯ menu.

### UI

#### Changed

- Improved the task management, conversation actions and traffic viewing experience (#57): task list column sort preferences are remembered persistently, inline rename is now triggered directly by an icon (no longer via a menu), and the drawer (sheet) interactions and traffic viewing details were polished.

#### Fixed

- Worker asset labels show only the domain and IP, and empty labels are handled correctly.

### Agent

#### Added

- The planner gains a "quantified acceptance check" when judging goals: when a goal has quantifiable acceptance criteria (asset test coverage reaching X%, capturing N flags, obtaining some privilege), it must check the measured values in `graph_overview` (`coverage.pct`, findings counts, etc.) before `prove_goal`; if the measurements fall short, `prove_goal` is forbidden and it must dispatch intents to close the gap instead, and it may not mark the goal met early on the grounds that "the main part is done / it is broadly achieved". This fixes a goal requiring 100% coverage being judged complete with only 40% measured.

#### Changed

- Rewrote the basis for "0 intents this round" in the planner prompt: the original text described 0 intents as "the most common, most important principle", which made the planner stop too early while the goal was unmet and untested surface remained in scope. It now says 0 intents should be produced only in two specific cases: ① every direction it can think of is already covered by open/running/recent_done intents; ② the next step depends on output from a work that is currently running and that output has not appeared yet (it should wait for the work to finish and plan at the next wake-up after the graph updates). A counter-constraint was also added: when there genuinely is a new direction that is not covered and does not depend on a running work, or the goal is unmet and untested surface remains, do not stop just because "0 intents is common".
- Strengthened the "evidence threshold for negative conclusions" in the worker prompt: for negative conclusions such as "not injectable / port closed / no login entry point" that could make the planner abandon a whole direction, it must exhaust the reasonable techniques within the intent (changing encoding / parameters / paths / methods) before concluding; if the techniques were not all tried or the evidence is weak, it must always mark `confidence=inferred`, so a careless `observed` negative does not weld the whole route shut (a wrong negative early in a task is especially likely to send the direction astray and hard to recover from).
- The planner / worker default prompt changes above are appended as new versions and made current through one-off migrations (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag); old versions are kept in the version history, and users who customised their prompts can restore them from the version records.

### Operations

#### Added

- New `reset-password.sh` to reset the admin password (the username is always `ARTEX`): supports both local / docker deployments, with connection details given explicitly or read automatically from `--dsn`/`$ARTEX_PG_DSN`/`config.json`; it uses `pgcrypto` in the database to generate a bcrypt hash compatible with the backend login and writes it back to `settings.auth.password_hash`, with no service restart needed afterwards. The password is passed in through an environment variable, never in the process argv, and is escaped to prevent injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After selecting several tasks in the task list you can change their category in bulk, and the target category can be "Uncategorised" to move tasks out of their current category; the whole batch is written in one transaction, and the only possible failures are tasks deleted after being selected.

#### Changed

- "Task category" in New task changed from a dropdown to a searchable input: typing filters the existing categories, a name not yet in the database is created and selected immediately on Enter (or by clicking "Create" in the dropdown), and the selection shows as a removable tag; still limited to a single category.

### Chat

#### Fixed

- Fixed sending a message being blocked with "LLM not configured, cannot chat" when the conversation had an LLM profile selected but there was no globally active profile. The pre-send check only looked at the globally active profile, while actual execution prefers the profile selected for the conversation, so the two resolution paths disagreed; they now share one logic (conversation/Agent binding first, global as the fallback).
- The message shown when chat is unavailable now depends on the state: with no profile at all it says "Add a profile", with profiles but none active it says "Activate one or pick a profile for this conversation", rather than always showing "Not configured", making the cause easier to pin down.

### Agent

#### Added

- For tasks whose goals are all met, intents the main Agent dispatches via `add_intent` can be claimed and run directly by a Worker, stopping once done: the Planner no longer runs in this case (a task with no open goals does not enter planning), so it cannot re-judge the goals as met and cancel the newly dispatched intent; once the frontier drains, the task goes back to completed. Before dispatching an intent, the main Agent asks the user, based on the intent's content, whether to register it as a formal goal; if it is registered, the task resumes regular autonomous planning.

#### Changed

- When `step_ids` for `get_worker_trace` / `get_task_worker_trace` exceeds the per-call limit (5), it no longer errors out; it returns the full content of the first 5 steps instead, and the result uses `returned_step_ids` / `omitted_step_ids` / `notice` to say which were fetched this time and which were not, with the hint "if this is already enough, there is no need to fetch more". Duplicate and invalid ids passed in are deduplicated and dropped before counting.

#### Fixed

- The task list now sorts by task id descending (newest first), replacing the old sort by creation time. Tasks created at the same moment had identical timestamps and an unstable order, which, combined with polling every 10 seconds and random map iteration in memory, made them keep swapping places in the list.

### Assets

#### Fixed

- Fixed company ownership recalculation being knocked over by a single dirty row: when `assets.ip` held a hostname (written by an Agent or the asset API), `a.ip::inet` threw `22P02`, making adding/editing a company's asset scope and deleting companies all fail and roll back. It now uses the safe conversion `try_inet()`, skipping invalid values instead of aborting the whole statement.
- Unparseable `ip` values are no longer skipped silently: after saving a company scope, a message states how many assets have an `ip` that is not a valid IP (with the specific ids and values), warning that these assets will not be attributed by IP/CIDR rules; the same information is also written to the server log, covering paths with no frontend response (deleting a company, scopesentry sync, Agent writes).
- IP/CIDR matching for the task test scope also switches to `try_inet()`, replacing the old regex guard that only checked the character set (hostnames made entirely of hex letters, such as `abc.def`, used to slip through and trigger the same error).

#### Changed

- The `ip` field of `ip`, `service` and `endpoint` assets no longer accepts hostnames: `insert_assets` and the asset API return a per-item error with its `index` and spell out the fix (use `type=subdomain` with `domain` instead, or resolve the A/AAAA records first), so the Agent can correct it and reinsert on its own. Other assets in the same batch are unaffected and saved as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- Running Workers can be paused, resumed and cancelled individually; pausing keeps the intent, facts and vulnerabilities, while cancelling transactionally cleans up the current intent and its direct outputs once execution exits.
- Added the `paused` intent status, an execution fence and named termination reasons, preventing late blackboard writes after a pause, cancellation or task deletion.
- Added an optional task concurrency limit; new tasks, resumes, vulnerability deep-dives and queue backfills all go through a persisted FIFO admission path.

#### Changed

- The default wall-clock duration of a single Worker run changed from 600 to 1200 seconds; after a timed-out task is revived, timing restarts at the next real execution.
- The real execution state of the Worker, Planner and main Agent now drives the task status badge, fixing it showing "Idle" while a Worker was running.
- Worker sessions keep per-item controls and the current session's token totals, and the model name moves to the right of the current session title.

#### Removed

- Removed the Worker multi-select, select-all and bulk pause/resume UI, along with the Worker bulk control API and Mock contract.
- Removed the token badge and tooltip from Worker list rows; the underlying full token ledger and task aggregation API remain.

### LLM

#### Added

- Tasks support an ordered LLM profile chain; when the provider's quota is clearly identified as insufficient, it switches to the next profile automatically and persists the current profile, the exhaustion state and an error summary.
- Running and paused tasks can edit the full profile chain, reorder it and manually switch the current profile, effective from the next LLM call.
- Automatic switches, manual switches and whole-chain exhaustion are written as structured system activities and pop up deduplicated reminders through the task activity feed.
- Added a task role model resolution API that resolves, in one place, the profile and model the Main Agent, Planner and Worker will use on their next call.
- Added an optional global LLM Pool, supporting a configurable call order, a fallback for when a specified model fails, health status, cooldown recovery and manual reset.
- Added an always-on LLM usage ledger that aggregates input, output and cache tokens by task, session, model and profile.

#### Changed

- Goal decomposition, the Planner, Workers and the main Agent share a task-level LLM runtime (see below for the resolution priority).
- Failover only reacts to clear quota, balance or billing errors; ordinary rate limiting, auth, network, server and context errors do not trigger a wrong switch.
- The LLM settings page now shows a list of profile cards edited in a right-hand drawer, and provides the Pool rotation order, priorities, exclusions, health status and recovery actions.
- The profile chain exhaustion message wraps on narrow screens; the current model moved from an icon in the session list to a text label next to the current session title, with a full tooltip.
- The order in which each in-task role resolves its LLM changed to "Agent binding → task profile chain → global": a role explicitly bound to a model always runs on that model, only unbound roles fall through to the task chain, and only when the task chain is empty do they fall through to global.
- An empty outbound proxy now means a direct connection, no longer falling back to the `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected); the proxy input accepts `socks5://user:pass@host:port` with a username and password.

#### Fixed

- Transient streaming failures before commit (before any output has been produced) are now safely retried with backoff on the same provider, markedly reducing cases of "stopped after only one or two tools, no summary, final state `model_error`"; quota exhaustion, context too long and deterministic 4xx errors are not retried and are still handled by failover and compaction recovery respectively.

#### Removed

- Removed the LLM icon marker from the session list to avoid confusion with the Worker status icons.

### UI

#### Added

- Tasks support creating, renaming, deleting and filtering by global categories; New task can select a category directly.
- Added global task template CRUD and a right-hand management drawer; New task can load a preset description and goals, or save the current content as a template.
- New task supports linking several source tasks and several company asset scopes; the company scope is entered in a single multi-line text box that automatically recognises domains, URLs, IPs, CIDRs, ICP filings and company keywords.
- Task test assets can be added and deleted live, recording their source (manual, company, inherited, discovered by an Agent, etc.); the current test assets and a source summary are shown next to the Worker session title.
- The Findings page groups by task with independent pagination within each group, and supports writing a description for a vulnerability and creating a high-priority deep-dive intent.
- Conversations can be renamed, pinned, unpinned and deleted; the task list supports bulk pause and resume for the current page.
- Traffic details and tool run details moved to a right-hand drawer; HTTP messages now include `Host` and highlight the request line, status code, headers, JSON and markup bodies.
- The task list actions column gains "Details" and "Pause/Resume" buttons; the session send key can be configured in System settings (`Enter` / `Cmd+Enter`, etc.), and the web search section gains a proxy input.
- The badge to the right of the session title now shows the LLM profile name, with the model ID moved to a hover tooltip; tasks can have an optional name, and the task list gains a name column and search (falling back to the description when empty).
- The Skills page adds stats for usage count, last used time and missing dependencies, making it easier to find skills that are not taking effect or not installed.

#### Changed

- Task titles are now focusable detail links; task assets, company assets, findings task groups and the vulnerabilities within groups use real server-side pagination, stable sorting and exact totals.
- Adding a company now uses a right-hand drawer, and scope entry is unified as multi-line text with live recognition, validation and type preview.
- The main Agent input box auto-grows, sends on `Enter` and adds a new line on `Shift+Enter`, and avoids accidental sends while a Chinese IME is composing.
- Agent previews, task reports and related details all use the shared Markdown renderer; delete confirmations, long errors and mobile drawer widths were fixed across the board.
- The task template picker displays and searches by template name and submits the template ID; the system's original font size was restored, and the app version is shown consistently as `0.3.3`.
- Task totals and current session tokens highlight only the input, cache read and output figures; the send button uses a simpler up-arrow icon.
- The test scope in the task overview shows company names instead of IDs.

#### Fixed

- Plain text containing dots is no longer mistaken for an ICP filing number.
- Fixed names in the variable catalogue clashing with global runtime variables (such as `{{.Now}}`), which made the Agent editor's variable list render duplicate keys.

#### Removed

- Removed the separate "Enter" button from task cards; click the task title to open the details instead.
- Removed the "New task" button from the Dashboard, the Logo URL input when adding a company, and the vulnerability count badges at the top of the Findings page and in task groups.
- Reverted the style that enlarged the global font by 10%, and removed the Worker checkboxes, model icons and per-row token stats from the session list.

### Agent

#### Added

- New tasks can link directly to several existing tasks, inheriting live and read-only the direct source tasks' goals, facts, vulnerabilities, completed intents, asset scope and blackboard context.
- Blackboard read tools can query source task nodes, facts, vulnerabilities and execution traces on demand; inherited nodes carry a source marker, and all write tools refuse to modify them.
- Company scope tools support domains, URLs, IPs, CIDRs, ICP filings and company keywords; keywords serve only as scope hints for the Agent and play no part in automatic asset attribution.
- The vulnerability deep-dive action creates a high-priority manual Worker intent in the original task, with asset anchors and a `derived_from` edge.
- The Overview gains a "Goal management" card for viewing, adding, editing and deleting goals by hand; adding or editing notifies the Planner and revives the task, while deleting hard-deletes the goal node (cascading to its edges and anchors) without reviving it.
- The main Agent gets the `steer_work` tool by default, letting it inject course-correction instructions into a running Worker in real time without interrupting it or losing progress (it still checks that the intent belongs to this task).

#### Changed

- Source task intents do not enter the new task's frontier; the new task keeps its own exploration, execution queue, working directory and conversation history.
- Deleting a running intent no longer destroys data: it now stops at `stopped`, requires a deletion reason, attaches the reason to the intent as a fact and writes it into the payload, and the planner is made aware via a `cancelled` trigger (keeping the intent content and reason).
- The main Agent conversation is now driven by the server-side activity feed, and the input can be restored when sending fails; opening a session scrolls to the bottom automatically and keeps the last reply visible after details lazy-load.
- Pausing a task terminates the current Main Agent, Planner and Worker calls, but does not stop the user from starting a new main Agent orchestration session while paused.
- Cancellation, shutdown and streaming interruptions all keep the real termination reason, the content generated so far, the number of turns run, elapsed time, tokens and tool calls that never returned.

#### Removed

- Removed the main Agent client's optimistic message echo, avoiding duplicate messages and streams crossing between sessions under pauses, failures or concurrent activity.

### Constraints

#### Added

- The goal decomposition stage extracts operation constraints (allow/deny) first and then decomposes goals, and the main Agent can add more at runtime; constraints are injected into the Planner and Worker system prompts at the highest priority, and after debiasing, diversity and surface-expanding exploration explicitly defer to the constraints.
- The Overview gains a "Constraint management" card and add/edit/delete APIs; where constraints are injected can be toggled separately for the Planner / Worker (both on by default, read every turn, effective immediately). The new table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, so old databases create it automatically.

### Intercept

#### Added

- Command intercept adds model fallback approval on top of regex/string rules: when no rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic verdict, with configurable failure fallback and approval timeout actions; the intercept page is split into two tabs, "Intercept rules / Model settings", and model verdicts are labelled with a `[Model]` prefix and the reasoning.

#### Fixed

- Hardened parsing of the judge's output so a correct `DENY` verdict is not mistaken for an allow.

### Traffic

#### Changed

- Traffic recording now stores each whole exchange in SQLite (large bodies spill into a blob bucket deduplicated by hash), with a trigram full-text index on text bodies supporting arbitrary substring and Chinese search; deletion becomes a single SQL transaction, dropping from hours to milliseconds without stalling recording in the meantime. Added streaming download for large bodies and a full-text parameter for `traffic_search`; data in the old file tree needs no migration and can still be read, searched and deleted.

### Tasks and assets

#### Added

- Task creation gains an "Asset coverage" switch (on by default): when off, coverage is not computed or shown, the situation graph shows only assets, `task_scope` is not accumulated automatically, and the related tools are removed from the Planner / main Agent; company links are not affected by this switch.
- `insert_assets` adds a per-asset `related` flag (default `true`): it only applies when coverage is on, and `false` puts the asset in the shared asset store only, without counting it toward this task's coverage (for example sibling sites or unrelated assets found along the way).
- Company scopes and task test assets both recognise domains, URLs, IPs, CIDRs, ICP filings and keywords in text; domains/IPs can create or reuse global assets, while CIDRs/ICP filings/keywords are kept as task scope context.

### Build

- `build.sh --release` builds Linux amd64/arm64, macOS amd64/arm64 and Windows amd64 in one go, producing zip release packages that include `skills/`, the example config and the README.
- Uses the Go linker to strip debug info and produces zip release packages; UPX is now explicitly opt-in, avoiding segfaults at startup of its self-extracting ELF in some Linux environments.
- The Release workflow runs a startup smoke test on the Linux amd64 binary and ships `SHA256SUMS` with the release packages.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New tasks can link to several existing tasks, inheriting live and read-only the direct source tasks' facts, vulnerabilities, completed intents, asset scope and blackboard context; the new task still uses its own execution queue, working directory and session history.
- Tasks support an ordered LLM profile chain. When the provider's quota is clearly identified as insufficient, it switches to the next profile automatically and persists the current profile, the exhaustion state and structured audit activities.
- Running and paused tasks can edit the LLM profile chain, reorder it and manually switch the current profile; automatic switches, manual switches and whole-chain exhaustion are shown on the task detail page.
- Running Workers can be paused, resumed and cancelled. Pausing keeps the blackboard data; cancelling transactionally cleans up the intent and the facts, vulnerabilities and execution records it directly produced once the Worker stops writing.
- Deleting a task can optionally also clean up the linked assets, traffic, vulnerabilities and the task's files on disk, with a new deletion barrier, concurrency protection and auditable deletion stats.
- Added an optional task concurrency limit; new tasks beyond the limit queue in FIFO order and start automatically once a running slot frees up.
- Added task asset pagination, company asset pagination, task category stats and a paginated intent Mock contract.
- Added the `build.sh` single-binary build script, supporting static frontend export, asset embedding, cross-platform targets and build version injection.

### Changed

- The explicit task-level LLM profile chain coexists with the existing global LLM Pool; an explicit chain keeps strict quota failover semantics, and without one the Agent binding and global profile rules apply as before.
- The main Agent input box supports auto-growing multi-line input, sending on `Enter` and a new line on `Shift+Enter`, and avoids accidental sends while a Chinese IME is composing.
- The Planner, Workers and main Agent share a task-level LLM runtime; the task chain uses the smallest context window among the candidate models as the safe compaction threshold.
- The task detail page shows running, idle and paused states based on the real LLM call state; facts, vulnerabilities, intents, asset references and graph nodes from source tasks are all marked with their source and kept read-only.
- Agent previews, task reports and related detail views all use the shared Markdown rendering component.
- LLM model profiles use cards and a drawer, and the model list scrolls independently within the drawer.
- Pausing a task no longer blocks the main Agent conversation: the main Agent orchestration session is independent of task pausing, and new messages can still be sent while paused (pausing only terminates the round currently in progress).
- The default wall-clock duration of a single Worker run changed from 600 to 1200 seconds.

### Fixed

- Removed the optimistic echo from the main Agent console in favour of rendering purely from server data, fixing messages getting jumbled or mixed into other sessions when pausing, failing to send and so on.
- Opening a main Agent session scrolls to the bottom by default and shows the last reply in full: once the last reply's full content lazy-loads and expands, the view sticks to the bottom automatically instead of the reply being pushed off screen.
- Fixed the main Agent session continuing to run after a task was paused, and the orchestration Agent not stopping the main Agent as well when pausing a task.
- Fixed the task status badge and action button states getting out of sync when a task completed or while the Planner, a Worker or the main Agent was actually running.
- Fixed Worker cancellation, task deletion and concurrent writes possibly producing late blackboard writes or leftover files.
- Fixed broken Markdown previews, long text overflowing delete confirmations, mobile widths and some misaligned task detail buttons.
- Added named termination reasons to every Agent cancellation path; activity details can show who cancelled, the final state, the number of turns run, elapsed time, token usage and tool calls that never returned.
- Fixed a race where the parent context could overwrite the named `shutdown` reason first when the backend shut down, and Chinese activity summaries being truncated by bytes and producing garbled text.
- Fixed cancellation events with partial streaming output losing the real termination reason, while keeping the content generated before the cancellation.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2
