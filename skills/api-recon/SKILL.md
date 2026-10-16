---
name: api-recon
description: Use this skill when collecting a website's API endpoints.
---

# API Recon (Frontend API Reconnaissance)

With **authorization** in place, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature trigger points** (tabs, dialogs, table actions, etc.).

---

## Boundaries and Prohibitions (Agent must read · violation = out of scope)

This skill **only performs API / parameter-surface reconnaissance**. It is not a vulnerability-hunting or penetration-exploitation stage.

### Task Boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate paths, methods, parameters, routes, UI trigger points | SQLi/XSS/privilege escalation/brute force/fuzzing for vulnerabilities, packet-tampering attacks, destructive operations |
| **Authentication** | Hook + stub/mock to bypass the **client-side** login gate | Asking the user for or guessing account credentials; attempting real login form submission |
| **Runtime** | Hook endpoints without credentials; use mock responses to get the SPA into the post-login shell | Flows that depend on a real backend session to proceed |

### Credential-Free Dynamic Analysis (Phase 3 Default)

1. Through `preload.js` / `runtime_harvest.js`, **intercept and stub** bootstrap endpoints such as login, permissions, and menus;
2. Return mock bodies with **correct structure, a success business code, and possibly empty data** for business query endpoints;
3. Let the frontend render post-login pages even without a backend or under a 401 environment, thereby triggering more XHR/fetch/WebSocket calls;
4. **Empty data, blank tables, and placeholder UI are all expected** — do not switch to real login or vulnerability testing because of them.

**In short**: use mocks to open up frontend routes and component mounting, and **record only outbound requests**. What the backend returns does not matter; what matters is which endpoints the frontend **still goes on to call**.

### Hard Process Prohibitions

| Prohibited | Alternative |
|---|---|
| Before Phase 1 is complete, grep/curl/Read the main entry `index-*.js` to extract API paths | Run `OUTDIR/harvest_static.py` |
| Hand-writing `extract_apis.py` or other scripts that replace harvest | Modify `OUTDIR/harvest_static.py` and re-run it |
| Repeating the same grep/command after it has failed ≥2 times | Change strategy: read tool_logs, modify harvest, consult the reference |
| Skipping gates A/B and running the original `scripts/` versions directly | Copy to OUTDIR and adapt to the target |
| Real usernames/passwords, OTP, OAuth or other authentication | stub/mock (see above) |
| Skipping stubs in order to get real data, and running privilege-escalation/injection tests | Record outbound only; this is within the recon boundary |
| Deleting, exporting sensitive data, bulk writes, and other irreversible operations | Same applies to coverage clicks |
| Claiming all pages and APIs were obtained without completing runtime + dynamic enumeration | See "Definition of Done" or state the limitation |
| Claiming all parameters are known without completing the parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional fields from a single runtime sample | Multi-sample diff, or validation rules/error-based inference |

---

## Two-Layer Model + Run Modes

| Layer | Output | Limit |
|---|---|---|
| **Static** (JS bundle) | Full endpoint paths, draft routes, candidate fields at request-building points | No HTTP methods; parameters require Phase 1b; misses URLs assembled at runtime |
| **Runtime** (live session) | Method + body + response + dynamic URLs + WS/SSE; multi-sample diff to complete parameters | Pages must actually render to send requests; a single sample is insufficient to determine required/optional |

| Run mode | Engine | Use case |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response bodies, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Clicking tabs/dialogs/tables for deeper feature coverage |
| **both** | depth first, then coverage | Most complete, longest runtime |

**Parameter methodology** (no generic script): derive paths via harvest/regex; derive parameters via **anchor-window expansion + UI binding chain + multi-sample diff + error-based inference** (grep recipes in section J of [reference.md](reference.md)).

---

## Definition of Done

Only claim recon is complete when all of the following are satisfied:

- [ ] **Static**: Phase 1 harvest produces `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both require **hooks in effect + the dynamic enumeration loop**
- [ ] **Enter shell**: when accessing business paths, the URL is not `/login` (watch for hash routing)
- [ ] **Parameters**: coverage/both complete the parameter trigger matrix + `param_samples.json`; Phase 5 merges into `params_merged.json`
- [ ] **Depth** (if module pages are blank): restore the permission tree in Phase 4 and re-run until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs are complete (see the Phase 5 output table); `insert_assets` writes service and endpoint assets

---

## Scripts and Gates

`scripts/` contains reference templates only. **Do not** run the originals directly and treat their output as final results.

**Rule**: read first → adapt to the target → write to `OUTDIR` (e.g. `recon/`) → log in `CHANGES.md`; if it does not fit, rewrite following the methodology and borrow only the structure.

| Gate | When | Reference script → OUTDIR copy | Common required changes |
|---|---|---|---|
| **A (static)** | After Phase 0, **before the first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **The default regex works directly on most sites**; only change the endpoint regex, webpack/Vite `publicPath`, or MPA exclude/cookie when the manifest/dialect does not match |
| **B (runtime)** | After Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success value, stubs, login regex, api prefix, hash/history |

**Mandatory SPA order** (cannot be reordered; phase numbering takes precedence over "explore first, then script"):

| Step | Required | Prohibited |
|---|---|---|
| After Phase 0 completes | Next Bash command = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | Copy script → make minor adjustments if needed → **run immediately** | Manually extracting APIs before deciding whether to harvest |
| Before Phase 1 completes | Verify outputs with `wc -l`; on 404, fix harvest and retry | Hand-writing extract scripts; repeatedly grepping URLs that were not downloaded |
| From Phase 1b onward | grep only `OUTDIR/js/*.js` | Using the main bundle instead of harvest output |

- ✅ Copy `harvest_static.py` → (optional) adjust regex → **run immediately**
- ❌ curl main bundle → grep multiple times → write temporary extract → harvest last
- **MPA**: After Phase 0, the next Bash command = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and Output Constraints

| Constraint | Description |
|---|---|
| Large files | `index-*.js` over 100KB **must not** be Read/grepped into context; batch-process with OUTDIR scripts |
| grep output | Must use `\| head -20` or `-m 5`; keep only path summaries in the conversation, do not paste bundle fragments |
| Verification | Use `wc -l`, `ls \| wc -l`; do not Read whole directories |
| Initial regex probing | Optional, ≤1 time, only on chunks ≤50KB or HTML; formal static analysis is based on harvest |
| reference | Recipes/templates/troubleshooting are in [reference.md](reference.md); do not repeat its full text inline |

---

## Execution Roadmap

```
Phase 0 Classification + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b Parameter reverse engineering
  → Phase 2 Auth three gates → config.json
  → Gate B → Phase 3 Runtime + parameter matrix
  → Phase 4 Permission tree (if needed) → re-run Phase 3
  → Phase 5 Merge report + insert_assets to batch-insert all discovered services and endpoint API assets; under no circumstances may discovered assets be omitted during insertion
```

Check off in order; **do not enter the next Phase until the previous item is complete**.

1. [ ] **Phase 0**: Initial probe of SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy script → **immediately** harvest → verify with `wc -l` → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor-window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: auth three gates → `config.json` → [Phase 2](#phase-2--auth-three-gates)
5. [ ] **Gate B**: adjust runtime scripts → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm shell entry; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → re-run Phase 3 → [Phase 4](#phase-4--permission-tree-restoration)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch the entry HTML and **create `OUTDIR`** (do not modify `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Outputs `forms.txt`, `links.txt`, `api_inline.txt`. If an SPA yields forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — Static

Follow [Scripts and Gates](#scripts-and-gates) · [Tool and Output Constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML scripts → webpack/Vite manifest → download all lazy chunks → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- Chunk count vs manifest: on 404, fix harvest and retry; do not manually curl chunks one by one
- If `api_static.txt` is too small → loosen the endpoint regex in OUTDIR and re-run (see reference)

### Phase 1b — Parameter Reverse Engineering

Paths come from Phase 1; parameter fields must be recon'd separately. For grep rules see [Tool and Output Constraints](#tool-and-output-constraints).

**Completion criteria**: for each important endpoint, you can answer: field name, transport location, type inference, whether required, sample values, and confidence.

#### 1b.0 — Transport Forms

| Form | Where parameters are | Static priority check |
|---|---|---|
| REST JSON | body + query | next to the path anchor, `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameters | `/user/:id` | route table + `useParams` / `$route.params` |
| Encrypted/signed | wrapped in `sign`/`data` | Hook encryption function inputs (reference section D) |

Output: annotate each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor-Window Expansion

Using a known path as the anchor, expand the window to find the request-building object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clues |
|---|---|
| axios instance | `data` / `params` |
| Unified request | Interceptor injects global fields |
| OpenAPI client | Generated method signatures |
| React Query / SWR | Second hook argument |
| Vue composable | composable input arguments |

Type residue: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding Layer

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

Use the DevTools call stack to trace the request-building function upward from `fetch`/`XHR.send`.

#### 1b.3 — Request-Building Three Questions (≠ Phase 2 Auth Three Gates)

| Question | What to answer |
|---|---|
| **Assembly** | Where the payload is built; traces of transformation |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

While reviewing the interceptor gate (Phase 2), also read the globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Hand-off to Phase 3

Candidate fields come from the static/binding layers; **required / optional / conditional dependencies** require the Phase 3 parameter matrix + diff + Phase 5 error-based inference.

---

## Phase 2 — Auth Three Gates

Grep `OUTDIR/js/` (with `head`) and write `config.json` (recipes in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How is logged-in state determined? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers a redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat localStorage key names as credentials — confirm them from the chunk/request chain.

**Exit = Gate B**: put the conclusions into `config.json` and modify `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API Observation (Optional)

Use `preload.js` in OUTDIR to confirm session key names, Authorization, and nested API URLs:

| Setting | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers observation |
| `extractUrlsFromResponse: true` | Sub-APIs in responses |
| `observe.storageReads/cookieReads: true` | Backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Each coverage round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Gate B must already be passed; follow [Boundaries and Prohibitions](#boundaries-and-prohibitions-agent-must-read--violation--out-of-scope) · the credential-free mock strategy.

Set `"runtimeMode": "depth" | "coverage" | "both"` in `config.json` (template in reference).

### Hook and Stub (Shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 Precise | auth/permission/bootstrap stubs | Pass first-screen auth |
| L2 Negative correction | All JSON responses | Not-logged-in code → success |
| L3 Fallback | `/api` etc. not matched by L1 | Empty success body to open up the UI |

- **depth**: fake auth + `forward` rewrites business codes + `stubs`; traverse `routes` (hash/history); produce `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or Userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; business paths do not return to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — Coverage Dynamic Enumeration (Required)

1. Main navigation/sidebar — click each item, wait 1–3s for network
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — first-row view/edit/details
4. Toolbar — export, filter, create (**avoid irreversible deletion**)
5. On entering each module — merge APIs/routes
6. SPA — for paths in `routes.txt` not yet covered, controlled `pushState` (prohibited for MPA)

**Parameter trigger matrix** (required): record each module once per operation type, and **diff across samples**:

| Operation | Parameters usually added |
|---|---|
| List first screen | Pagination + default filters |
| Click search | keyword, filter |
| Advanced filter | More optional |
| Create/edit | Full entity |
| Bulk/export/sort | `ids[]`, `exportType`, `sortField` |

**Under stubs, outbound body/headers remain real** — rely on the requests themselves. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — Permission Tree Restoration

**Trigger**: module pages are blank / each route only has bootstrap (e.g. locale) → content gate not passed.

| Symptom | Meaning |
|---|---|
| Shell entry succeeds | Render gate + interceptor gate passed |
| Sidebar items missing / clicks blank | Stub shape or permission codes incomplete |
| Each route has the same, very few APIs | `v-if permission` check not passed |
| `routes.txt` far fewer than bundle | Must complete from the auth module |

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

Stub check: outer `response_code` matches the interceptor gate; flat codes align with the tree; `routes` covers all links in `route_map`.

After updating `config.json`, **re-run Phase 3**. For large SPAs, tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — Merge and Report

### Output Table

| File | Phase | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundle and paths |
| `param_candidates.json` | 1b | Static parameter field candidates |
| `config.json` | 2 | Three gates + runtime config |
| `runtime_api.json` | 3a | depth detailed recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multi-sample, click logs, detail |
| `route_map.json` etc. | 4 | Permission-tree intermediate files (if executed) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | Routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | Write all services and endpoint assets into the asset store |

### 5b — Parameter Merge

Diff from `param_samples.json`; **there is no generic merge script**. For confidence rules see reference J7 (high/medium/low/pending trigger).

### 5c — Error-Based Inference

Within the authorized scope, send incomplete requests and read the 400 responses (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Watch for the `data` wrapper, `variables`, and `bizData` before encryption.

The report must state: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, and a `CHANGES.md` summary relative to the reference scripts.

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

For more fields and grep recipes, see [reference.md](reference.md).

---

## General Notes

- **Framework-agnostic**: the webpack/Vite/Angular lazy-load approach is the same
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions cannot be fully enumerated
- **Blind spots**: JSVMP, WASM, strong HMAC/mTLS verification → static analysis + note the limitation
- **Parameter blind spots**: conditional linkage, hidden params, WASM request building → "pending trigger" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static analysis can still enumerate endpoints

---

## Additional Resources

- Grep recipes, `config.json` template, troubleshooting, hooks, parameter reverse-engineering section J, site_map template: **[reference.md](reference.md)**
- Reference script paths are in the [Scripts and Gates](#scripts-and-gates) table
