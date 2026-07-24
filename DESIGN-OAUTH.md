# GlideMind OAuth — Design Addendum

**Status: LOCKED (2026-07-23).** All five open questions resolved (see Resolutions at the end),
with one scope change from the original proposal: the client-credentials grant is **in scope**
(Resolution 4 — every org instance enforces 2FA, so the service account's basic-auth path needs a
proper non-interactive replacement, not just a deferral note). Resolves the "Interactive OAuth
(PKCE)" item in [DESIGN.md](DESIGN.md) §Deferred, the `GLM_TOKEN`/`GLM_CLIENT_ID`/
`GLM_CLIENT_SECRET` promises in DESIGN.md §9, and the attribution recommendation in
[DESIGN-WRITES.md](DESIGN-WRITES.md) W9.

The driving constraint: the org enforces 2FA/MFA on all instances, which blocks personal basic
auth, so glm currently authenticates as the `svc.glm` service account. Two problems follow:

1. **Attribution (W9):** writes stamp `svc.glm`, not the real user. Interactive OAuth
   (Authorization Code + PKCE, through the IdP/MFA in a browser) is the path to acting-as-self.
   SmartWork's profile stays read-only until this lands.
2. **Service-account durability:** the service account's basic-auth password is itself a 2FA
   workaround. The client-credentials grant replaces it with a client_id + client_secret whose
   tokens run as a designated user on the registry entry — retiring the password entirely.

## Constraints this must honor (from the locked design)

- **Agent-first determinism.** glm is invoked hundreds of times per agent session. A data command
  must never block on a browser, prompt, or listener. Interactive auth happens in exactly one
  place — an explicit login command — and everything else either has a valid credential or fails
  fast with the corrective command (exit 2).
- **Secrets never in the config file** (Decision 6). Tokens and client secrets live in the OS
  keyring; the config holds only non-secret OAuth parameters (client_id, port).
- **Zero-config where possible** (Decision 8). OAuth's irreducible configuration is the
  instance-side Application Registry entry — glm cannot conjure a client_id. Everything else
  (endpoints, scope, redirect URI, token lifecycle) is derived or defaulted, overridable.
- **ServiceNow-generic.** Standard `/oauth_auth.do` + `/oauth_token.do` endpoints only; nothing
  instance- or app-specific.
- **Transport-agnostic core.** The OAuth flows and token store are `internal/` packages with no
  TTY or browser assumptions baked in; the CLI injects the interactive parts.

## Decision log (locked)

| # | Decision | Choice |
|---|----------|--------|
| O1 | Grant scope | Authorization Code + PKCE (S256) for humans; client-credentials for automation; password grant and device flow stay out |
| O2 | Profile model | `auth = "oauth"` or `"client_credentials"` + `client_id` in config (non-secret); secrets/tokens in keyring |
| O3 | Login surface | Explicit `glm profile login [name]` / `logout`; data commands never launch a browser |
| O4 | Redirect capture | Loopback HTTP listener, fixed default port **8456**, `http://localhost:8456/callback`, profile-overridable; auth URL always printed |
| O5 | Token storage | Tokens + expiry as one JSON blob per profile in the OS keyring, distinct from the password entry |
| O6 | Token lifecycle | Proactive refresh when expired; renew-once-and-retry on 401; unrecoverable PKCE refresh failure → exit 2 naming `glm profile login` |
| O7 | Client seam | `snow.Client` gains an authenticator interface (basic / static bearer / refreshing PKCE / client-credentials); transport otherwise unchanged |
| O8 | Headless/CI | `GLM_TOKEN` = static bearer overriding the keyring for any profile (mirrors `GLM_PASSWORD`); `GLM_CLIENT_ID`/`GLM_CLIENT_SECRET` drive client-credentials; env profile stays read-only (W1) |
| O9 | Confidential fallback | PKCE uses a public client by default; if an instance's entry demands a secret anyway, `--client-secret-stdin` stores it in the keyring |
| O10 | Identity | Login/first-token ends with an identity resolution call; the username is stored on the profile (feeds W7 identity-in-preview and the per-user schema cache key) |
| O11 | Security posture | S256 only, single-use random `state`, listener bound to loopback, exact redirect match, tokens never logged or printed |
| O12 | Exit codes | Auth misconfiguration → 1; every runtime auth failure → 2 with the corrective command; network during flow → 4 |
| O13 | Client-credentials | `auth = "client_credentials"`: secret in keyring, cached access token, self-healing re-mint on expiry/401; token runs as the registry entry's designated user |

---

## O1 — Grant scope

Two grants, two audiences:

- **Authorization Code + PKCE (S256)** — humans. The only grant that produces acting-as-self
  through MFA, which is the W9 attribution fix. PKCE method is `S256` only — no `plain` fallback.
- **Client-credentials** — automation. The app authenticates as itself; ServiceNow runs the
  token as the sys_user designated on the registry entry (`oauth_entity.user`). This is the
  proper replacement for `svc.glm`'s basic-auth password on 2FA-enforced instances: same
  identity, no password. Attribution for automation writes is the designated user — correct,
  since automation *should* stamp the service identity.

Explicitly out: the password grant (defeats MFA, deprecated upstream) and device-code flow
(ServiceNow does not offer it). Basic auth remains supported for dev/PDI instances where it
still works.

## O2 — Profile model

```toml
[profiles.smartwork]                    # a human, acting as self
instance = "https://smartwork.service-now.com"
auth = "oauth"
client_id = "3ae4b0..."                 # non-secret by definition (public client)
username = "tcurtsinger"                # written by `profile login`, informational
# redirect_port = 8456                  # only when the default is taken

[profiles.smartwork-svc]                # automation, acting as the designated user
instance = "https://smartwork.service-now.com"
auth = "client_credentials"
client_id = "5031d9..."
username = "svc.glm"                    # written on first token, informational
```

Setup: `glm profile add <name> --instance … --auth oauth --client-id <id>` (no password
prompt; login discovers the username), or `--auth client-credentials --client-id <id>
--client-secret-stdin` (secret goes straight to the keyring). `resolveProfile` accepts both
methods; anything else still gets the existing "not supported" error.

## O3 — Login surface

- **`glm profile login [name]`** — the one interactive command: opens the system browser to the
  instance's `/oauth_auth.do` (printing the URL too, in case auto-open fails), captures the
  callback on the loopback listener, exchanges the code, stores tokens, resolves identity (O10),
  and prints a `whoami`-style confirmation. Times out after 5 minutes with exit 2. On a
  client-credentials profile, `login` simply mints and stores a token (no browser) — useful as a
  setup check.
- **`glm profile logout [name]`** — deletes the stored tokens (keyring only; the profile and any
  client secret stay).
- **Data commands never go interactive.** No valid access token, no renewal possible → exit 2:
  `profile "smartwork": OAuth session expired — run: glm profile login smartwork`. An agent
  surfaces that line to the human, who logs in once; deterministic for everyone (Resolution 5 —
  no TTY special-casing).
- `profile remove` also deletes tokens and client secret; `profile test` works unchanged through
  the new client.

## O4 — Redirect capture

A transient HTTP listener on loopback, **default port 8456** ("glm" on a phone keypad → 456).
ServiceNow's registry matches the redirect URI exactly (no RFC 8252 any-port loopback), so the
port must be fixed and match the Application Registry entry: `http://localhost:8456/callback`.
Override with `redirect_port` on the profile (for a port conflict or a differently-registered
entry). The listener binds loopback only, accepts a single callback, validates `state`, shows a
minimal "you can close this tab" page, and shuts down. Port already in use → exit 1 naming the
override. Remote/SSH sessions can't complete a loopback flow — `GLM_TOKEN` or a
client-credentials profile is the fallback.

## O5 — Token storage

One keyring entry per profile, distinct from the basic-auth password entry (e.g. key
`<profile>:oauth`), holding a small JSON blob: access token, refresh token (PKCE only),
absolute expiry with a ~60s early-refresh skew. A client-credentials profile additionally has a
`<profile>:secret` entry for the client secret. ServiceNow tokens are short opaque strings — the
blob sits far under the Windows Credential Manager ~2.5 KB blob limit. Concurrent glm processes
may race on renewal; ServiceNow does not rotate refresh tokens, so both obtain valid tokens and
last-write-wins is safe.

## O6 — Token lifecycle

Instance defaults: access token 30 min, refresh token 100 days (per-registry-entry values exist
on `oauth_entity`; glm trusts the `expires_in` it is handed, not assumptions). The client renews
**proactively** when the stored expiry has passed, and **reactively** on an HTTP 401 — renew
once, retry the request once. The 401 retry is safe for writes: a 401 is rejected before
processing, so the send-once contract (`api`'s "exactly once on the wire") holds. Renewal means:
refresh-token exchange (PKCE) or a fresh client-credentials mint (no refresh token in that
grant — the secret *is* the long-lived credential, so a CC profile never dead-ends while its
secret is valid). PKCE refresh failure (refresh token expired/revoked) → exit 2 naming
`glm profile login <name>`. A 403 is never retried — that's authorization, not authentication.

## O7 — Client seam

`snow.Client` currently hardcodes `req.SetBasicAuth` in three places (`getJSON`, `Raw`,
`DownloadAttachment`). It gains one small internal interface:

```go
type authenticator interface {
    apply(req *http.Request) error   // set Authorization
    retryAuth(ctx) bool              // after a 401: true = credentials renewed, retry once
}
```

Four implementations: **basic** (current behavior, `retryAuth` always false), **static bearer**
(`GLM_TOKEN`, no renewal), **refreshing PKCE** (keyring-backed, single-flight refresh within the
process), **client-credentials** (keyring-cached token, re-mints from the secret). Constructors:
`NewBasic` unchanged; `NewBearer` and `NewOAuth` added. Everything else — retries, redirect
policy, error mapping, body caps — is untouched. The seam is what makes the whole feature
testable: unit tests inject a fake authenticator; flow tests point the token endpoint at
`httptest`.

## O8 — Headless/CI env vars

- **`GLM_TOKEN`** supplies a static bearer and **overrides the keyring for any profile**
  (Resolution 2) — exactly the precedence rule `GLM_PASSWORD` established: the profile picks the
  instance, env may supply the credential. If both are set, `GLM_TOKEN` wins (the more specific
  claim). No renewal: when the token dies, exit 2 and CI re-mints.
- **`GLM_CLIENT_ID` + `GLM_CLIENT_SECRET`** supply client-credentials material. On a named
  `client_credentials` profile, `GLM_CLIENT_SECRET` overrides the keyring secret (the
  `GLM_PASSWORD` rule again). They never change a profile's auth *method* — only `GLM_TOKEN`
  carries method+credential in one, and that is deliberate.
- **The synthetic env profile** (`GLM_INSTANCE`) infers its method from what is present, most
  specific first: `GLM_TOKEN` → bearer; else `GLM_CLIENT_ID`+`GLM_CLIENT_SECRET` →
  client-credentials; else `GLM_USERNAME`+`GLM_PASSWORD` → basic. It remains read-only per W1,
  unchanged — this is about who supplies the credential, never writability.
- The schema cache is keyed per user (dictionary reads are ACL-filtered); under `GLM_TOKEN` a
  stored profile username is never trusted for that key (the token may be a different account).
  `GLM_USERNAME` supplies the identity explicitly, else the cache keys on a short digest of the
  token itself — distinct per credential, stable for its lifetime.

## O9 — Confidential-client fallback (PKCE)

Yokohama supports public clients (`public_client` on the registry entry) and that is the
recommended, default setup for the PKCE entry — a CLI cannot keep a secret, so a "confidential"
CLI client is security theater. But if an instance's entry is created confidential anyway, glm
cooperates: `glm profile add … --client-secret-stdin` stores the secret in the keyring (never in
config), and the token exchange includes it. Zero new config for the recommended path.
(Client-credentials entries are confidential by nature — O13 — and reuse the same keyring slot.)

## O10 — Identity resolution

The last step of `profile login` — and the first successful client-credentials mint — resolves
who the token belongs to (one GET on `sys_user` with `sys_id=javascript:gs.getUserID()`), stores
the username on the profile, and prints it. That feeds: the W7 identity line
(`as tcurtsinger @ smartwork…` — now the real user, which is the entire point), `whoami` (works
unchanged), and the per-user schema cache key (`Client.Username()`). If the instance later maps
the token to a different user (account changes, registry `user` reassigned), `whoami` shows the
truth; the stored name is informational, not authoritative.

## O11 — Security posture

- PKCE `S256` with a fresh 43+ char verifier per login; single-use cryptographically random
  `state` checked on callback; listener accepts exactly one callback then closes.
- Listener binds loopback only; the callback page carries no token material (the code arrives as
  a query param, is exchanged immediately, and never printed).
- Tokens and client secrets never appear in config, logs (`--verbose` logs URLs via
  `Redacted()`, never headers or POST bodies), audit lines (W6 already excludes values), or
  error messages.
- The existing transport protections apply unchanged: https-only (loopback exempt), same-origin
  redirect policy, no credential in instance URLs.

## O12 — Exit codes

Reuse the contract: auth misconfiguration (missing client_id, bad port, missing secret) → 1;
every runtime auth failure — denied consent, login timeout, expired/revoked refresh token,
rejected client secret, 401 after renewal → 2, always naming the corrective command; network
failures during a flow → 4.

## O13 — Client-credentials grant

The non-interactive counterpart, replacing `svc.glm`'s basic-auth password on 2FA-enforced
instances:

- **Registry side:** a confidential entry with `default_grant_type = client_credentials` and a
  designated run-as user (`oauth_entity.user` → e.g. `svc.glm`). Tokens act as that user — same
  identity the automation has today, minus the password.
- **glm side:** `auth = "client_credentials"` profile; client_id in config, secret in the
  keyring (`--client-secret-stdin`), minted access tokens cached in the keyring like PKCE
  tokens. No refresh token exists in this grant; renewal is a fresh mint from the secret, so the
  flow is fully self-healing — no browser, no dead ends while the secret is valid. A rejected
  secret → exit 2 naming `glm profile add <name> --client-secret-stdin` (rotation).
- **Write gating unchanged:** a client-credentials profile can be write-enabled like any named
  profile (W1's two gates still apply); the W7 identity line shows the designated user, which is
  the honest answer for automation writes.

## Instance facts (verified on ven07100, 2026-07-23)

- **The org's instances already run this exact flow in production.** The team's SmartWork MCP
  (Cloud Run) is pure per-user OAuth: the MCP client (Claude) performs authorization-code + PKCE
  (S256) directly against the instance's `/oauth_auth.do` + `/oauth_token.do`, with
  `offline_access` refresh tokens, and the server forwards the user's bearer token — every write
  already lands as the real user there. glm's PKCE flow is the same protocol with a
  `localhost:8456` callback instead of Claude's. This also settles the "IdP/SSO pass-through"
  verification item: users authenticate through it today, dev through prod (ven03765).
- Registry setup may be lighter than a new entry: ServiceNow accepts multiple comma-separated
  redirect URLs per entry, so `http://localhost:8456/callback` could be appended to the existing
  Claude-connector entry. A dedicated "GlideMind CLI" entry is still the recommendation
  (independent revocation, per-client token lifespans, cleaner audit), but both work.
- Release: **Yokohama patch 13** (`glide.war`).
- `oauth_entity` schema confirms everything both grants need: `use_pkce` and `public_client`
  booleans (PKCE public client), `default_grant_type` choice + `user` sys_user reference +
  `client_secret` (client-credentials with a designated run-as user).
- `svc.glm` can read `oauth_entity` (16 entries today). Registry entries for glm must be created
  manually (admin UI) per instance — two entries where both grants are used: "GlideMind CLI"
  (public, PKCE required, redirect `http://localhost:8456/callback`, scope `useraccount`) and
  "GlideMind Automation" (confidential, client_credentials, user = the service account). Setup
  steps land in the README with this feature. (A `glm api`-driven bootstrap of the entries is
  possible but deferred — one manual admin step per instance is acceptable.)
- Still to verify at implementation time, on the live instance: the exact token response shape
  (`expires_in`, `scope`), whether the PKCE entry demands a secret despite `public_client`,
  client-credentials grant availability on the org's instances (the field existing proves the
  release supports it, not that a given instance permits it), and IdP/SSO pass-through on the
  SmartWork instance (dev may not exercise the real IdP).

## Testability

Same house rule as writes: `term.IsTerminal` is never true under `go test`, and no test opens a
real browser. The `internal/oauth` package takes injected endpoints (auth URL, token URL →
`httptest`), an injected browser-opener (`func(url) error`), and an injected token store — the
full login flow, both renewal paths, 401-retry, state mismatch, timeout, and port-conflict cases
all run headless. The CLI layer stays a thin wiring shim, like `matchesRecordKey` before it.

## Resolutions (locked 2026-07-23)

1. **Login verb: `glm profile login` / `profile logout`.** The profile is the object; slots
   beside `add`/`test`/`use`; adds no top-level line to `glm prime`.
2. **`GLM_TOKEN` overrides any profile**, mirroring `GLM_PASSWORD`: the profile picks the
   instance, env may supply the credential. `GLM_TOKEN` beats `GLM_PASSWORD` when both are set.
3. **Port 8456, URI `http://localhost:8456/callback`** — baked into every instance's registry
   entry; `redirect_port` profile override for conflicts.
4. **Client-credentials is IN scope** (changed from the proposal): every org instance enforces
   2FA, so the service account needs a passwordless non-interactive path, not a deferral. O13.
5. **Expired session always errors** — exit 2 naming `glm profile login <name>`, identical for
   humans and agents. No TTY-conditional prompt in the auth path.

## Rollout

1. **Authenticator seam + `GLM_TOKEN`** (1 PR): the O7 interface, `NewBearer`, env wiring,
   env-profile method inference (bearer leg only). Small, fully unit-tested, gives CI a story
   immediately.
2. **OAuth core** (1 PR): `internal/oauth` — PKCE flow, loopback listener, code exchange,
   refresh, client-credentials mint, keyring token store. All endpoint-injected tests, no CLI
   surface yet.
3. **CLI wiring** (1 PR): `profile add --auth oauth|client-credentials --client-id
   [--client-secret-stdin]`, `profile login`/`logout`, `resolveProfile`/`clientForResolved`
   branches, identity resolution, renew-on-401, `GLM_CLIENT_ID`/`GLM_CLIENT_SECRET`.
4. **Live verification**: registry entries on ven07100, end-to-end login + `whoami` + a
   client-credentials mint, then the SmartWork profile — and `write-enable` there only after
   acting-as-self is confirmed (W9).
