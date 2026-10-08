# `/btw` validation record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Used a separate PostgreSQL test database and data directory; real model credentials were injected only into the separate test environment, were not written into code or this record, and the product's default model was not changed. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

The real model conversations, returned objects, engineering assertions and Qwen's raw review text are saved in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Deep copy of structured messages and tool arguments | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summary / compaction requests do not overwrite, complete replies and terminal states are published, half replies are excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model pool member identity | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-exchange replay, budget trimming and over-limit errors | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main and side running in parallel, cancellation isolated in both directions | Pass | Blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, usage already incurred kept on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript / activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subtests |
| Persistence, paging, idempotency, partial answers kept across restart | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear vs late write races, parent resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archive and restore, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| Three parent endpoints, authentication, resource ownership, Worker logical deletion | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Side questions on a busy main session, independent SSE reconnect / disconnect, cancel, clear | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| Concurrency of 1 per parent session / 4 globally | Pass | Two `TestSideHTTP…` cases |
| Snapshot persisted before submission, follow-up after restart, old sessions cannot forge a snapshot | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Refuses to continue after the cached config is deleted or the model changes | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancels before archiving and waits for the final answer and usage to persist | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Usage and side question attribution recorded only once when a streaming consumer cancels early | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| Worker / deadline run contexts restored automatically after a restart keep publishing new snapshots | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race checks on related packages | Pass | Commands below |
| TypeScript and production build | Pass | `npx tsc --noEmit`, `npm run build` |
| Biome on new frontend modules | Pass | `biome check`, 3 new modules |

After pointing `ARTEX_PG_DSN` at a separate disposable database, the automated checks can be reproduced (do not point it at a production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green: two existing tests in the `server` package fail during temp directory cleanup, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

Exporting the source from the unmodified baseline above and rerunning the `server` package in the same isolated environment reproduces both cleanup failures as well. The baseline run also showed a goal node count assertion failure in `TestCoreTaskLifecyclePG`; the final modified `server` regression does not have that assertion failure. The other packages pass, and the side question cases and race checks for this change pass. Baseline problems were not counted as passing for this acceptance, and no existing assertions were changed to hide problems.

The Next.js build prints the existing multiple lockfile / workspace root inference warning; the build completes and all pages are generated successfully.

## Browser checks

Used the Codex In-app Browser, connected to a separate local Go service and the Next.js dev server. The following manual/automated operations were completed on desktop and on a 390 × 844 narrow screen, checking screenshots and browser logs:

- Typing `/btw` while a regular chat is running shows the main content and the side question together; the desktop sidebar works correctly.
- Consecutive follow-up questions; after a side question is stopped the generated part is kept; the main flow continues.
- The request continues when the panel is closed, and the completed answer is restored on reopening; after a page refresh, an empty `/btw` restores the history.
- Input, buttons, history and close in the narrow-screen Drawer work correctly, with no horizontal overflow.
- Clearing uses a confirmation dialog; after clearing the history disappears, while the main transcript and snapshot are kept.
- Asking questions from the task MainAgent and two Workers separately and switching between them: Agent labels and histories do not get mixed up.
- A blocking local model fixture keeps the Worker running; after submitting `/btw` from the Worker's main input box and stopping the side question, the Worker still shows live running status and its own pause button, and the side question saves its partial answer.
- Browser error / warning logs are empty.

The controllable fixture is used to verify concurrency timing precisely, without depending on the real model's output speed. During debugging, two checks with running Workers did not produce a valid concurrency window (the task had already finished / the answer ended early); the fixture was fixed and the checks were redone and passed; those initial runs are not counted as valid passes.

## Real model conversations

First probed `grok-4.6` on the OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, model name `grok-4.6` and `READY` in 2.82 seconds. The first choice was available, so neither the Tingly `glm` nor the Zhipu `glm-5.3` fallback chain was enabled; these two fallback services were not verified this time.

| Scenario | Actual result |
| --- | --- |
| Asked about the asset, goal and marker while the main session was running | Returned `redhaze.top`, reading the home page and summarising the goal, and `BTW-REAL-0910`; side question completed in 16.97 seconds |
| Asked for the tool evidence after the main session finished reading the home page | Correctly cited WebFetch 200, the curl redirects 301 → 302 → 200 and the page title; 7.24 seconds |
| Side question asked Bash to create a test file | Refused to run; the target file was not created; 7.74 seconds |
| A completed side question does not change the main context | Main transcript SHA-256 and main activity records unchanged; side question tool executions: 0 |
| Follow-up after actually stopping / restarting the Go service | Kept the earlier 3 side question history entries and answered the asset, marker and title directly from the persisted snapshot, without rerunning the main Agent |
| New session using a Grok non-streaming config | Correctly answered the asset and `ATOMIC-0910`; usage returned and saved: input 11734, output 138, cache_read 11520 |

In the asset case, the main session used WebFetch and Bash/curl to read the public home page; the landing page was `https://id.redhaze.top/home`, titled "RedHaze Technology RedHaze Group · Global Conglomerate Portal". Bash stored the response temporarily in a local test file; nothing was written to the remote end. This fact was verified separately from "the side question did not run any tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming responses did not return usage. A separate direct request with `stream_options.include_usage=true` returned HTTP 200, 12 data frames and 0 usage frames. So the 0 in the streaming tests means the endpoint provided no usage, and must not be read as no billing. Non-streaming usage and the fixture's failure / cancellation usage are all saved correctly.

## Qwen review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. It was given the first three real side question conversations, the main session's tool evidence and the engineering assertions; it returned `verdict: accept` and `concerns: []`, judging the answers consistent with the asset, marker and page read evidence, and the side question's tool refusal compliant with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review did not cover the service restart and non-streaming tests added later. Qwen's summary of "no writes" was too broad: the main session's curl did create a local temporary response file, as recorded explicitly above. Concurrency, zero tool executions and transcript isolation are judged by the engineering assertions; the model review only helps assess answer quality.
