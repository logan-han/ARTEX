# Long side conversations and context budget

Fixed on 2026-09-11. The original implementation treated the character count of the request JSON directly as tokens and inherited the main task's 32K output reservation, so it rejected normal side questions early when there was a lot of HTML, JS or tool results.

## Open source implementation review

- [Grok CLI side context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts snippets from recent user and assistant text, with a character budget of about 2000 and at most 400 characters taken from each entry. This path does not maintain a continuous side question history.
- [Grok CLI independent request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): separate cancellation signal; an output limit of 2048 tokens when the model supports it; no tools provided.
- [Grok CLI main session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, keeps recent content, updates the old summary with new content, and handles truncation across turns.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full request estimation, output/buffer reservation, recent content plus a rolling summary, and summary requests without tools; that implementation defaults to a recent budget of 8000 and a summary output limit of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): only attempts overflow recovery when assistant output has not started yet, and the call after recovery does not enter the same overflow recovery path again.

ARTEX borrows the separate output budget, recent content plus a rolling summary, and bounded recovery. It keeps norma v0.3.6's structured messages and tool pairing, does not copy Grok's text excerpts, and does not write OpenCode's main session compaction events into the ARTEX main transcript.

## Request budget and execution

- Messages reuse norma's per-content-block UTF-8 byte estimate with its 4/3 margin; system prompts, tool schemas and message framing overhead are counted as well. The estimate is not an exact model token count.
- Side question output defaults to at most 8192 tokens and never exceeds the output limit already set in the main config. The service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` can set a limit from 256 to 32768; it does not change the product's default model or the main task's parameters.
- The input budget is the context window minus the output limit and a safety margin; an unknown window uses the platform default of 200K. The safety margin is 5% of the window, at least 128 and at most 8192 tokens.
- Successful exchanges are loaded by increasing ordinal, at most 20 per batch. At most 20 exchanges are kept verbatim, with a token budget of at most 1/4 of the input budget and no more than 16K.
- Exchanges over the limit are folded into the rolling summary. The summary records the history source and context time; past assistant answers are not the same as new tool evidence, and on conflict the latest main snapshot takes precedence.
- When the main context is still too long, only the older messages in the copy are summarised, keeping at most 8K tokens of recent content; the cut point never separates a tool call from its result. An oversized single group goes into the summary as a whole.
- Summary input is split into UTF-8-safe chunks based on the actual remaining window, with an output limit of 2048 tokens; an empty summary, truncation, tool calls or exceeding the summary budget are never written to the cache. A single side question makes at most 12 summary calls and is subject to the same 120-second timeout; hitting the limit fails explicitly instead of looping forever.
- If the model's first response reports a context overflow and no text or tool call has been output yet, the request is reduced further and retried at most once; if the estimated size did not drop, recovery stops immediately. Other model errors and partial streamed output do not trigger this recovery.
- All usage obtained, including summaries, failed attempts and usage at cancellation, is added to the same side question request. When the Provider returns no usage, only zero can be recorded; an estimate must not be passed off as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores the summary of older exchanges, the ordinal it covers, and main context summaries cached by snapshot identity. `side_question_requests.context_info` stores the preparation phase, the actual number of replayed exchanges, summary usage and budget estimates.

A summary is saved only while the original request is still running and the clear version matches; clearing also clears the cache, and late writes cannot bring back cleared data. A new snapshot does not reuse an old snapshot's summary. Summary fields are saved with v3 task archives; restoring an old v3 archive that lacks these fields fills in empty objects, and v1/v2 remain compatible.

POST first admits and returns the request; preparation and compaction run in the background without holding the admission lock or a database transaction. SSE/history show the preparing, organising exchanges, compacting copy and answering phases; a compaction failure is saved as that side question request's failed terminal state. The frontend keeps the error and restores the failed question as a draft, without covering the input box with a floating notification; history polling no longer clears submission errors.

## Validation record

- Replay of 19/20/21/50 exchanges, retention of old conclusions across 20 exchanges, reuse of the summary cache after restart: automated tests pass.
- Very long Chinese answers and code context, chunked request budgets, tool pairing, unchanged snapshots, cache invalidation on a new snapshot: automated tests pass.
- Summary failure/cancellation/truncation/oversize/tool return, clear races, the call cap, only one overflow recovery and no retry of partial streams: automated tests pass.
- Separate PostgreSQL: pagination, restart, v1/v2/v3 archives, archive restore of the summary cache and budget metadata, old v3 archives missing the new fields, and 20 parent sessions sharing four concurrency slots: pass.
- Go candidate service build, frontend TypeScript check, Biome check of the modified components, and a Next.js production build in a separate directory: pass.
- The built-in browser used a separate UI fixture at 1280×720 and 390×844 to verify the organising phase, the summary scope hint, draft restore after failure, no floating error notification, no horizontal overflow and no console errors. The temporary fixture has been removed.
- Read-only replay of existing local Worker snapshots passed the new budget checks; for example, Worker #3's 293085-character snapshot is no longer wrongly rejected by local character counting. This item made no external model calls.
- The real conversation test that would have sent a private Worker snapshot to Grok was rejected by the automatic approval review and was not run, so it does not count as a pass.
- 2026-09-11 00:37: restarted the local backend at the user's request, reusing the original database, data directory and login config. The running file matches the candidate binary's SHA-256; `/api/health` returns OK through both the backend and the frontend proxy.

Validation commands (using only a separate test database):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses a separate copy to avoid overwriting the current preview's `.next`. The candidate service is at `/private/tmp/artex-btw-budget-candidate`, and has been copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.
