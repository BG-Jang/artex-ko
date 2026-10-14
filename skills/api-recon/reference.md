# api-recon — Reference Manual

Grep recipes, `config.json` templates, and troubleshooting. All greps run against the `js/` directory. If the bundle is a single line, run `js-beautify` or `sed 's/}/}\n/g'` first; a raw grep with a context window is usually enough.

## Script Notes

Every file in `scripts/` is a **reference template** and must be adjusted for the target site before running. Typical changes:

| Script | Items commonly needing adjustment |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success value, stub match rules and body structure, routes source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether L3 is enabled, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` for destructive links, cookies, depth/max, same-domain filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field names |
| `config.json` | single entry point for all of the site-specific parameters above |

Place adjusted files in the task working directory (e.g. `recon/`), and state in the report the specific changes made relative to the reference scripts.

---

## A. Reverse-Engineering the Three Gates

### A1. Render Gate — "How does the app decide the user is logged in?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Trace the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` to determine the **storage key**, the **container** (Cookie vs. localStorage), and the **encoding**:

| Encoding | How to forge it in config |
|---|---|
| Plain string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | Unsigned / `alg:none` JWT, or sign with a key found in the bundle |
| Encrypted (SM2/AES/RSA) | Look for a hardcoded key; if the render gate only needs a decodable blob, forge it; otherwise fall back to static analysis |

→ Write into `cookies` / `localStorage`.

### A2. Interceptor Gate — "What triggers a redirect to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaiE '.{0,40}(未登录|请重新登录|登录已过期|登录失效|授权|로그인|인증|권한|세션.{0,6}만료|unauthorized|unauthenticated|forbidden|access denied|login required|please (log|sign) ?in|session expired|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: the **field name**, the **success value** (usually `0` or `200`), and the **failure value that triggers the redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ Write into `neutralize.fields` + `neutralize.success`.

### A3. Content Gate — "Where do menus/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common in enterprise back-offices):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | Route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | Sidebar menu rendering |
| `userRouteAuth` in bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` in bundle | `{ KEY: { name, link } }` | Alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): build a permissive payload → `stubs`.

**Full permission-tree reconstruction** (large sites where the sidebar or submodules remain empty): see **Section I**.

---

## B. config.json Template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field notes:
- `runtimeMode`: `depth` (Puppeteer), `coverage` (browser MCP), `both`
- `cookies[].value` prefixes: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal
- `forward: true` forwards real requests and rewrites the code field; `false` runs fully offline with stubs
- `mockTier`: preload enablement tier in coverage mode, e.g. `L1+L2`, `L1+L2+L3`
- `routes` comes from `routes.txt`; after forging the menu, the harness automatically appends `<a href>` entries
- `captureResponses` / `recordWs` are only effective in depth mode
- `waitUntil`: use `domcontentloaded` for large SPAs to avoid hanging on `networkidle2`
- `routeTimeout`: timeout for a single route's `page.goto` (milliseconds)
- `proxy`: Puppeteer `--proxy-server`; `HTTP_PROXY` / `HTTPS_PROXY` can also be set

### B1. Dual-Stub Template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

The outer field names (`response_code` / `code` / `data`) must match the interceptor gate from A2; `permissions` must cover every leaf code in the tree.

---

## C. Coverage Mode: Preload Configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before injection via CDP:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify: `window.__API_RECON_PRELOAD__ === true` and the pathname is stable.

Export the recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. Preload / Runtime Hook Capabilities

Browser hook capabilities built into preload (coverage) and runtime_harvest (depth), and their coverage:

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | Record request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Discover headers such as Authorization | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | Confirm session key names | ⚠️ Optional `observe.storageReads/cookieReads` |
| Vue route retrieval | Fill in frontendRoutes | ✅ `__API_RECON_ROUTES__` (loaded routes) |
| Vue route guard neutralization / login redirect blocking | Trigger module APIs | ✅ `neutralizeVueRouter` + native redirect neutralization |
| React route retrieval | Fill in routes | ⚠️ Static analysis + clicking; no dedicated hook |
| Page navigation blocking (login path) | Keep the page for analysis | ⚠️ Blocks only the login path, to avoid interfering with business navigation |
| Hook encryption libraries (CryptoJS/SM, etc.) | Encrypted params → plaintext API body | ❌ Must manually hook encryption function inputs; record conclusions in config |
| Anti-debugging bypass | Otherwise runtime cannot capture APIs | ❌ Must be handled manually; static analysis still works |

---

## E. Endpoint Extraction Regex (When Static Results Are Too Few)

Relax the regex in `extract_endpoints` in `harvest_static.py`, or run manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → Fix |
|---|---|
| Very few static APIs | Endpoint dialect mismatch → relax the regex (Section E) |
| Chunk count ≪ manifest | CSS-only or undeployed chunks; 404s already retried |
| Runtime still shows the login page | Render gate error → recheck A1: key name, container, encoding, domain |
| Shell loads but modules are blank | Content gate → forge the menu (A3); `routes` paths may be wrong |
| Each route only shows bootstrap/locale | Permission codes incomplete → Section I permission-tree reconstruction; check the dual stubs `role_permissions` + `permissions/all` |
| Sidebar items exist but subpages are blank | Tree missing intermediate nodes, or code does not match `userRouteAuth` |
| Every API redirects to login | Interceptor gate → verify `neutralize`; nested fields require extending the walk logic |
| WS frame count is 0 | Subscription happens only after user interaction; increase `perRouteMs` |
| Empty response bodies | Real responses only exist with `forward: true` |
| Chromium missing | Install chromium or set `config.chromium` / `CHROMIUM` |
| Many mocks still return to login | Hook injected too late, or missing `location.href` setter → use document-start + preload |
| Lists are all empty | Empty L3 arrays are normal; keep clicking tabs/settings/details |
| Redux actions mistaken for routes | Filter out internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | Preload not at document-start → change injection timing; or with `neutralizeVueRouter: false`, clear guards manually |
| URLs appear in responses but not in the log | Enable `extractUrlsFromResponse`; or extract manually from `__API_RECON_DETAIL__` |
| Unknown Authorization header name | Enable `observe.xhrHeaders` or inspect request headers in DevTools |
| Runtime extremely slow / timing out | Change to `waitUntil: domcontentloaded`; lower `routeTimeout`; do not use `networkidle2` |
| Proxy connection failure | Check `proxy` / environment variables; the Puppeteer and curl proxy ports must match |

---

## G. Hardened Targets

When the server progressively validates sessions (non-forgeable signed cookies, server-rendered menus that cannot be stubbed), the runtime gets stuck at the shell. Expected behavior:

- **Static analysis is sufficient for endpoint enumeration** — module paths are in the code
- If authorization permits, run the same harness with a **real session**: `forward: true`, no neutralize needed, capture real methods/params/responses

---

## H. Single-Task Checklist

1. Confirm the authorized scope
2. **Read** `scripts/harvest_static.py` → adjust for the target → run → review `api_static.txt` and `routes.txt`
3. **Phase 1b**: path-anchor window expansion + binding layer → `param_candidates.json` (Section J)
4. Reverse-engineer A1/A2/A3 → write the site-specific `config.json`
5. **Read and adjust** `runtime_harvest.js` / `preload.js` before running them
6. `runtimeMode=depth`: `npm install` → run the adjusted harvest script
7. `runtimeMode=coverage/both`: inject the adjusted preload at document-start → browser MCP dynamic enumeration + **parameter trigger matrix**
8. Module does not render → **Section I permission-tree reconstruction** → patch stubs → rerun
9. Multi-sample parameter diff + error back-inference → `params_merged.json`
10. Merge → `site_map.json` + `api_merged.txt`, honestly annotating coverage, gaps, and script changes

---

## I. Permission-Tree Reconstruction (Phase 4 Deepening)

Use this when the simple forged `menus: [{ path, show: true }]` does not work and submodules still fail to mount.

### I1. Locate the Auth Module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: the **permission API path**, the **response field names**, and the **consuming chunk filename**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# produces recon/route_map.json
```

If you see `[!] no routeMap pattern found`: relax the regex in `extract_route_map.py`, or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build the Permission Tree + Stubs

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases such as `He=o.DASHBOARD`)
2. Resolve aliases to real paths using `route_map.json`
3. Infer the parent from the code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, automatically write the `stubs` and extend `routes` in `config.json`

**Adjust for the target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes without a prefix relationship

### I4. Verify Stub Consistency

```bash
# permissions count should be ≈ number of userRouteAuth entries
wc -l recon/perm_codes_all.txt
# routes should cover all links in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun Runtime and Compare

```bash
node recon/runtime_harvest.js recon/config.json
# Compare the runtime_api.json entry count before and after forging; check whether module APIs such as /attack and /asset now appear
```

| Before forging | After forging (success) |
|---|---|
| Same 3–5 bootstrap entries on every route | Different routes trigger different module APIs |
| Only `/api/locale/language` | Module endpoints such as `/api/web/...` appear |
| Single-digit routes in `routes.txt` | `routes` 80–110+ from route_map |

### I6. If Still Failing

- **Coverage mode**: click the sidebar and tabs; permission gating may only request data after interaction
- **Stub fields**: compare the real API (curl + real session) against the nesting of the stub
- **Additional guards**: grep for `hasPermission|checkRole|func.` and similar button-level checks, then extend `role_permissions.permissions`
- **Static fallback**: module API paths remain in `api_static.txt`; runtime only supplements METHOD/body; keep parameters in `param_candidates.json` + recorded samples

---

## J. Parameter Reverse-Engineering (Phases 1b / 5b / 5c)

**A methodology, not a generic script.** Use regex to find paths; use anchor window expansion, the UI binding chain, multi-sample diffs, and error back-inference to find parameters.

### J1. Anchor Window Expansion — Finding the Request-Building Object from a Path

```bash
# Use a path known from Phase 1 as the anchor
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper Layers and Transport Shapes

```bash
# axios / unified request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# Path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation Gates — Required / Format / Enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding Layer — Form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Runtime supplement: DevTools → Network → request → **Initiator** (call stack); trace upward from `fetch`/`send` to the function that builds the payload.

### J5. Encrypted Parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Do not guess fields on ciphertext.** Hook the **inputs** of the encryption function and record the plaintext payload before encryption; record conclusions in `config.json` / `param_candidates.json`.

### J6. Parameter Trigger Matrix (Required in Phase 3)

For each module, record one request per operation and diff the request body/query:

| Operation | Focus |
|---|---|
| Initial list load | Default pagination values |
| Search | keyword, filters |
| Advanced filter | optional fields |
| Create/edit | Complete entity |
| Batch/export | `ids[]`, `exportType` |
| Sort/paginate | `sortField`, `order` |

Produce `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence Rules

| Confidence | Condition |
|---|---|
| **High** | Static callsite + runtime ≥2 samples consistent |
| **Medium** | Static only, or only 1 runtime capture |
| **Low** | Inferred from responses/errors, not re-verified |
| **Pending trigger** | Static fields known, but UI/permissions did not reach them |

### J8. Scenario Quick Reference

| Scenario | Order |
|---|---|
| REST list page | J1 request-building object → J6 four-way diff → J3 rules |
| Create/edit form | J3 Form name → J4 submit chain → runtime submit + deliberately leave a field empty to see the 400 |
| GraphQL | J2 variables declaration → record variables for each operation at runtime |
| Encrypted body | J5 hook inputs → fields before encryption are the real params |

### J9. Mapping to api-recon Phases

| api-recon | Parameter recon |
|---|---|
| Phase 1 static | J1 anchor window expansion |
| Phase 2 A2 interceptor | Globally injected fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | Different module forms differ → full fields only trigger with sufficient permissions |
| Phase 5 merge | `params_merged.json` + confidence; do not determine required fields from a single sample |

### J10. Troubleshooting

| Symptom | Handling |
|---|---|
| Static has field names but runtime never shows them | Mark as "pending trigger"; complete the permission tree / click advanced filter / iterate each option of linked selects |
| Same path with different body shapes | Normal — record separately by `action`; do not force a merged schema |
| Stub response looks fake but you want the params | **Inspect the outbound request** body/headers; do not infer from the stub response |
| 400 reports a nested field | Watch for outer wrappers such as `data`/`bizData`/`variables` |
| GraphQL shows only operation names | Expand the `variables` JSON; find `$var: Type` statically |

---
