---
name: api-recon
description: Invoke this skill when collecting a website's API endpoints.
---

# API Recon (frontend API reconnaissance)

Provided you are **authorised**, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature triggers** (tabs, modals, table actions, etc.).

---

## Boundaries and prohibitions (must-read for agents; any violation is out of bounds)

This skill does **API / parameter surface recon only**; it is not the vulnerability hunting or exploitation phase.

### Task boundaries

| Scope | Allowed | Forbidden |
|---|---|---|
| **Goal** | Enumerate paths, methods, parameters, routes, UI triggers | SQLi/XSS/privilege escalation/brute-force/fuzzing for vulnerabilities, request-tampering attacks, destructive operations |
| **Auth** | Hook + stub/mock to bypass the **client-side** login gate | Asking the user for, or guessing, usernames and passwords; attempting real login form submissions |
| **Runtime** | With no credentials, hook the APIs and use mock responses to get the SPA into the post-login shell | Flows that can only continue with a real backend session |

### Credential-free dynamic analysis (Phase 3 default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** login, permission, menu and other bootstrap APIs;
2. For business query APIs, return mock bodies that are **structurally correct, carry a success business code, and may have empty data**;
3. Make the frontend render post-login pages even with no backend or in a 401 environment, so that it fires more XHR/fetch/WebSocket requests;
4. **Empty data, blank tables and placeholder UI are all expected**; do not switch to real login or vulnerability testing because of them.

**In one sentence**: use mocks to prop open the frontend routes and component mounting, and **record only outbound requests**; what the backend returns does not matter, what matters is **which other APIs the frontend will call**.

### Hard process prohibitions

| Forbidden | Do instead |
|---|---|
| grep/curl/Read the main entry `index-*.js` to extract API paths before Phase 1 is complete | Run `OUTDIR/harvest_static.py` |
| Hand-writing `extract_apis.py` or other scripts that replace harvest | Modify `OUTDIR/harvest_static.py` and rerun it |
| Repeating the same grep/command after it has failed ≥2 times | Change strategy: read tool_logs, modify harvest, check reference |
| Skipping Gate A/B and running the original `scripts/` directly | Copy to OUTDIR and adapt to the target |
| Real username/password, OTP, OAuth or other authentication | stub/mock (see above) |
| Skipping stubs on the grounds of "getting real data" to do privilege escalation/injection testing | Record outbound only; that is the recon boundary |
| Deleting, exporting sensitive data, bulk writes and other irreversible operations | The same applies to coverage clicks |
| Claiming all pages and APIs have been found without completing runtime + dynamic enumeration | See "Definition of done", or state the limitations |
| Claiming all parameters are known without completing the parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional from a single runtime sample | Multi-sample diff, or infer from validation rules/errors |

---

## Two-layer model + run modes

| Layer | Output | Limits |
|---|---|---|
| **Static** (JS bundle) | Full endpoint paths, draft routes, candidate fields at payload assembly points | No HTTP methods; parameters need Phase 1b; misses URLs concatenated at runtime |
| **Runtime** (live session) | Method + body + response + dynamic URLs + WS/SSE; multi-sample diff fills in parameters | Pages must actually render before they send requests; a single sample is not enough to decide required/optional |

| Run mode | Engine | Use for |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response bodies, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Clicking tabs/modals/tables for deeper feature coverage |
| **both** | depth first, then coverage | Most complete, takes the longest |

**Parameter methodology** (no general-purpose script): use harvest/regex for paths; for parameters use **anchor window expansion + UI binding chain + multi-sample diff + error-based inference** (grep recipes in section J of [reference.md](reference.md)).

---

## Definition of done

Only claim recon is complete when all of these are met:

- [ ] **Static**: Phase 1 harvest produced `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both requires **hooks in effect + the dynamic enumeration loop**
- [ ] **Into the shell**: visiting a business path does not land on `/login` (mind hash routing)
- [ ] **Parameters**: coverage/both completed the parameter trigger matrix + `param_samples.json`; Phase 5 merged `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 reconstructed the permission tree and reran, until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs complete (see the Phase 5 output table); `insert_assets` wrote the service and endpoint assets

---

## Scripts and gates

`scripts/` holds reference templates only; running the originals directly and treating that as the final result is **forbidden**.

**Rule**: read first → adapt to the target → write to `OUTDIR` (e.g. `recon/`) → record in `CHANGES.md`; if it does not fit, rewrite it following the methodology and borrow only the structure.

| Gate | When | Reference script → OUTDIR copy | Common required changes |
|---|---|---|---|
| **A (static)** | After Phase 0, before the **first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **For most sites the default regex runs as is**; only when the manifest/dialect does not match, change the endpoint regex, webpack/Vite `publicPath`, MPA exclude/cookie |
| **B (runtime)** | After Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success value, stubs, login regex, API prefix, hash/history |

**SPA mandatory order** (must not be swapped; the Phase numbering takes precedence over "explore first, script later"):

| Step | Must | Forbidden |
|---|---|---|
| After Phase 0 | The next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | Copy the script → small changes as needed → **run it immediately** | Extracting APIs by hand first, then deciding whether to harvest |
| Before Phase 1 is complete | Check the outputs with `wc -l`; on 404, fix harvest and retry | Hand-written extract scripts; repeatedly grepping URLs that were never downloaded |
| From Phase 1b onwards | grep only `OUTDIR/js/*.js` | Using the main bundle instead of harvest |

- ✅ Copy `harvest_static.py` → (optional) change the regex → **run immediately**
- ❌ curl the main bundle → grep several times → write a temporary extract → only harvest at the end
- **MPA**: after Phase 0 the next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and output constraints

| Constraint | Details |
|---|---|
| Large files | **Do not** Read/grep `index-*.js` files >100KB into context; batch-process them with OUTDIR scripts |
| grep output | Must use `\| head -20` or `-m 5`; keep only path summaries in the conversation, do not paste bundle snippets |
| Verification | Use `wc -l`, `ls \| wc -l`; do not Read whole directories |
| Initial regex probe | Optional, ≤1 time, only on small chunks ≤50KB or on HTML; for the real static pass, harvest is authoritative |
| reference | Recipes/templates/troubleshooting are in [reference.md](reference.md); do not repeat the full text inline |

---

## Execution roadmap

```
Phase 0 classify + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter reverse engineering
  → Phase 2 three auth gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (if needed) → rerun Phase 3
  → Phase 5 merge and report + insert_assets to bulk-insert every discovered service and endpoint API asset; under no circumstances may an already discovered asset be left out when inserting
```

Tick them off in order; **do not enter the next Phase until the previous item is complete**.

1. [ ] **Phase 0**: initial probe for SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0-classification)
2. [ ] **Gate A + Phase 1**: copy the script → harvest **immediately** → verify with `wc -l` → [Phase 1](#phase-1-static-analysis)
3. [ ] **Phase 1b**: anchor window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b-parameter-reverse-engineering)
4. [ ] **Phase 2**: three auth gates → `config.json` → [Phase 2](#phase-2-the-three-auth-gates)
5. [ ] **Gate B**: adapt the runtime scripts → [Phase 3](#phase-3-runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm you got into the shell; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4-permission-tree-reconstruction)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5-merge-and-report)

---

## Phase 0: Classification

Fetch the entry HTML and **create `OUTDIR`** (do not modify `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunks → Phases 1-5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Produces `forms.txt`, `links.txt`, `api_inline.txt`. If forms ≈ 0, treat it as an SPA → switch to Phase 1.

---

## Phase 1: Static analysis

Follow [Scripts and gates](#scripts-and-gates) and [Tool and output constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse the HTML scripts → webpack/Vite manifest → download every lazy chunk → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- Chunk count vs manifest: on 404, fix harvest and retry; do not curl chunks one by one by hand
- Too few entries in `api_static.txt` → loosen the endpoint regex in OUTDIR and rerun (see reference)

### Phase 1b: Parameter reverse engineering

Paths come from Phase 1; parameter fields need their own recon. grep rules are in [Tool and output constraints](#tool-and-output-constraints).

**Completion criteria**: for each important API you can state the field names, where they are sent, inferred types, whether they are required, sample values and confidence.

#### 1b.0: Transport shape

| Shape | Where the parameters are | Look at first (static) |
|---|---|---|
| REST JSON | body + query | Next to the path anchor: `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameters | `/user/:id` | Route table + `useParams` / `$route.params` |
| Encryption/signing | Wrapped into `sign`/`data` | Hook the encryption function's input arguments (reference section D) |

Output: tag each API with `transport: query|json|form|graphql|encrypted`.

#### 1b.1: Anchor window expansion

Using a known path as the anchor, widen the window to find the payload object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clues |
|---|---|
| axios instance | `data` / `params` |
| Unified request | Interceptors inject global fields |
| OpenAPI client | Generated method signatures |
| React Query / SWR | The hook's second argument |
| Vue composable | The composable's input arguments |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2: Binding layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| Form submit | Follow submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | Global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

In the DevTools call stack, trace up from `fetch`/`XHR.send` to the function that builds the payload.

#### 1b.3: The three payload questions (≠ the three auth gates of Phase 2)

| Question | What to answer |
|---|---|
| **Assembly** | Where the payload is built, traces of transforms |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / header |

At the interceptor gate (Phase 2), also read the globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4: Handoff to Phase 3

Candidate fields come from static analysis/the binding layer; **required/optional/conditional dependencies** need the Phase 3 parameter matrix + diff + Phase 5 error-based inference.

---

## Phase 2: The three auth gates

grep in `OUTDIR/js/` (with `head`) and write `config.json` (recipes in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How does it decide the user is logged in? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers the redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not assume a localStorage key name is the credential; confirm it from the chunks/request chain.

**Exit = Gate B**: put the conclusions into `config.json`, and modify `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b: API observation (optional)

Use the `preload.js` in OUTDIR to confirm session key names, Authorization and nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | Header observation |
| `extractUrlsFromResponse: true` | Sub-APIs inside responses |
| `observe.storageReads/cookieReads: true` | Feed back into config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Export after every coverage round: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3: Runtime

Gate B must already be passed; follow [Boundaries and prohibitions](#boundaries-and-prohibitions-must-read-for-agents-any-violation-is-out-of-bounds) and the credential-free mock strategy.

Set `"runtimeMode": "depth" | "coverage" | "both"` in `config.json` (template in reference).

### Hooks and stubs (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 precise | auth/permission/bootstrap stubs | Get past first-screen auth |
| L2 negative correction | All JSON responses | Not-logged-in code → success |
| L3 fallback | `/api` etc. not matched by L1 | Empty success body to prop open the UI |

- **depth**: fake auth + `forward` rewriting business codes + `stubs`; walk the `routes` (hash/history); produces `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or a userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; business paths do not bounce back to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b: Coverage dynamic enumeration (mandatory)

1. Main navigation/sidebar: click each item, wait 1-3s for the network
2. Tabs: `role=tab`, `.ant-tabs-tab`
3. Tables: view/edit/details on the first row
4. Toolbar: export, filter, create (**avoid irreversible deletes**)
5. On entering each module: merge APIs/routes
6. SPA: controlled `pushState` to the paths in `routes.txt` not yet covered (forbidden for MPA)

**Parameter trigger matrix** (mandatory): for each module, record once per operation type and **diff the samples**:

| Operation | Parameters it usually adds |
|---|---|
| List first screen | Pagination + default filters |
| Click search | keyword, filter |
| Advanced filter | More optional ones |
| Create/edit | Full entity |
| Bulk/export/sort | `ids[]`, `exportType`, `sortField` |

**Under stubs, outbound bodies/headers are still real**: the request is the source of truth. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4: Permission tree reconstruction

**Trigger**: module pages are blank / each route only shows bootstrap (e.g. locale) → the content gate has not been passed.

| Symptom | Meaning |
|---|---|
| Got into the shell | Render gate + interceptor gate passed |
| Sidebar items missing / clicks show blank pages | Stub shape or permission codes incomplete |
| Every route has the same, very few APIs | `v-if permission` not passing |
| `routes.txt` has far fewer than the bundle | Fill them in from the auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

Stub checks: the outer `response_code` matches the interceptor gate; the flat codes line up with the tree; `routes` covers every link in `route_map`.

After updating `config.json`, **rerun Phase 3**. For large SPAs you can tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5: Merge and report

### Output table

| File | Phase | Contents |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundles and paths |
| `param_candidates.json` | 1b | Static parameter field candidates |
| `config.json` | 2 | Three gates + runtime config |
| `runtime_api.json` | 3a | Detailed depth recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multiple samples, click log, detail |
| `route_map.json` etc. | 4 | Permission tree intermediate files (if run) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | Routes, APIs, params, features, limitations |
| **insert_assets** | 5 | Write every service and endpoint asset to the asset store |

### 5b: Parameter merge

Diff from `param_samples.json`; **there is no general-purpose merge script**. The confidence rules are in reference J7 (High/Medium/Low/Pending trigger).

### 5c: Error-based inference

Within the authorised scope you may send incomplete requests and read the 400s (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Watch for the `data` wrapper, `variables`, and `bizData` before encryption.

The report must state: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, and a summary of `CHANGES.md` relative to the reference scripts.

Suggested `site_map.json` structure:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

More fields and grep recipes are in [reference.md](reference.md).

---

## General notes

- **Framework-agnostic**: webpack/Vite/Angular lazy loading is handled the same way
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetches can be recorded; RSC/Server Actions cannot be fully enumerated
- **Blind spots**: JSVMP, WASM, strict HMAC/mTLS validation → static + note the limitations
- **Parameter blind spots**: conditionally linked fields, hidden params, payloads assembled in WASM → "Pending trigger" / "Unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate the endpoints

---

## Additional resources

- Grep recipes, the `config.json` template, troubleshooting, hooks, section J on parameter reverse engineering, the site_map template: **[reference.md](reference.md)**
- Reference script paths are in the [Scripts and gates](#scripts-and-gates) table
