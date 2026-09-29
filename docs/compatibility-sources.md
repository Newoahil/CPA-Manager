# Quota compatibility sources and contract

Research/implementation anchor: 2026-09-29. Validation owner: main agent.
This document records source evidence, not live upstream success. All package
fixtures are synthesized from source contracts; none are production golden
captures. No production requests, credentials, configuration downloads, or
notification delivery are part of package validation.

## Frozen package boundary

`internal/quota` is a pure builder/parser, importing only the standard library
and `internal/domain`. It performs no network or filesystem operations.
`Build(Context)` returns the upstream request, not the CPA envelope.
The transport lane adds `auth_index`, authenticates to CPA, checks the outer
status, unwraps `status_code` and string `body`, and calls `Parse` with the
**upstream** status. `status <= 0` represents a transport failure or timeout.

Providers: `codex`, `claude`, `gemini-cli`, `antigravity`. Profile defaults to
`current`; only Antigravity also supports `legacy`. Unknown profiles/providers
return `ErrUnsupported`. Required missing/unsafe context returns
`ErrMissingContext`; errors never contain supplied context values.

The builder has fixed HTTPS endpoints. Only the Authorization header contains
`Bearer $TOKEN$`; tokens are not accepted by this package. Account/project data
cannot contain the placeholder or control characters. No endpoint fallback,
default project, credential download, or optional subscription/profile query is
performed. Codex requires account ID even though newer frontends allow omission.

## Official management documentation

Official website references:
- <https://help.router-for-me/management/api>
- <https://help.router-for-me/management/apiv8>

Website TLS retrieval failed during research. The official documentation source
was read at `53943c24483c8be5e1e4039e12c7621d09262786` (2026-09-28):
- [v0 document](https://raw.githubusercontent.com/router-for-me/CLIProxyAPIDocs/53943c24483c8be5e1e4039e12c7621d09262786/docs/en/management/api.md)
- [v8 document](https://raw.githubusercontent.com/router-for-me/CLIProxyAPIDocs/53943c24483c8be5e1e4039e12c7621d09262786/docs/en/management/apiv8.md)
- [website routing](https://raw.githubusercontent.com/router-for-me/CLIProxyAPIDocs/53943c24483c8be5e1e4039e12c7621d09262786/docs/.vitepress/config.mts)

The documents evolve; they do not define every historical release. The website's
currently deployed revision was not verified.

| Transport | Credential list | Generic upstream proxy |
| --- | --- | --- |
| Old fork/v0 | `GET /v0/management/auth-files` | `POST /v0/management/api-call` |
| Official v8.0.3 | `GET /v8/management/credentials` | `POST /v8/management/requests/api-call` |

Common proxy request: `auth_index` string, `method`, absolute `url`, singular
`header` string map, optional `data` **string**. Common response: outer HTTP 200
can contain upstream failure; inspect `status_code`, `header` string-array map,
and JSON-decode `body` string. Management and upstream authentication differ.

Old fork anchor: `Leejhua/CLIProxyAPI` main resolved to
`3bc34e2c5e940b401a3883cdbb6aa6f032f35e21` (2026-05-11):
- [routes](https://raw.githubusercontent.com/Leejhua/CLIProxyAPI/3bc34e2c5e940b401a3883cdbb6aa6f032f35e21/internal/api/server.go)
- [proxy handler](https://raw.githubusercontent.com/Leejhua/CLIProxyAPI/3bc34e2c5e940b401a3883cdbb6aa6f032f35e21/internal/api/handlers/management/api_tools.go)
- [credential metadata](https://raw.githubusercontent.com/Leejhua/CLIProxyAPI/3bc34e2c5e940b401a3883cdbb6aa6f032f35e21/internal/api/handlers/management/auth_files.go)

Official v8.0.3: `acdace936fa7df2905500c7f5e0a97d683138dea` (2026-09-28):
- [v8 routes](https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v8.0.3/internal/api/server_management_v8.go)
- [proxy handler](https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v8.0.3/internal/api/handlers/management/api_tools.go)
- [quota handler](https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v8.0.3/internal/api/handlers/management/plugin_quota.go)
- [quota registration](https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v8.0.3/internal/pluginhost/quota_provider.go)
- [normalized types](https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v8.0.3/sdk/pluginapi/types.go)

`/credentials/quota/providers` enumerates active plugin QuotaProvider capabilities,
not guaranteed built-in Codex/Claude/Gemini CLI/Antigravity implementations.
`/credentials/quota/fetch` dispatches to a plugin or an already-configured
credential `quota_probe`, otherwise returns 501. Therefore plugins are not
strictly necessary when a probe already exists, but a v8 version alone does not
guarantee quota support. The generic v8 proxy can be selected in advance when
normalized capability is absent. This package does not parse the normalized v8
response or install/configure plugins or probes.

v8.0.3's actual request `proxy_url` overrides credential/global proxies, contrary
to the documentation's stated priority. This package never sets that field.
The old fork replaces header placeholders only; v8.0.3 also replaces body
placeholders. The common contract deliberately uses header substitution only.
No behaviour is inferred from go.mod's version or from a missing filename.

## Upstream profiles and safe context

Frontend old anchor: `v1.7.41`, `b45639aa0169de8441bc964fb765f2405c10ccf4`.
Frontend current anchor: `v1.24.2`, `4530da271ba2e89810d4dccebc57f3091afa590a`.

- [old URLs/headers](https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/v1.7.41/src/utils/quota/constants.ts)
- [old requests/parsers](https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/v1.7.41/src/components/quota/quotaConfigs.ts)
- [old model quota builder](https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/v1.7.41/src/utils/quota/builders.ts)
- [current URLs/headers](https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/v1.24.2/src/utils/quota/constants.ts)
- [current Codex](https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/v1.24.2/src/features/quota/providers/codex/data.ts)
- [current Claude](https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/v1.24.2/src/features/quota/providers/claude/data.ts)
- [current Antigravity](https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/v1.24.2/src/features/quota/providers/antigravity/data.ts)
- [current grouped parser](https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/v1.24.2/src/utils/quota/builders.ts)

| Provider | Fixed request | Parsed quota schema |
| --- | --- | --- |
| Codex | GET `https://chatgpt.com/backend-api/wham/usage`; required `Chatgpt-Account-Id`, pinned codex-tui UA | `rate_limit.primary_window` required; optional secondary, code-review and additional rate-limit windows; `used_percent` in 0..100 |
| Claude | GET `https://api.anthropic.com/api/oauth/usage`; `anthropic-beta: oauth-2025-04-20` | Known named windows' `utilization`; current Fable/Fable 5 `weekly_scoped` limits' `percent`, in 0..100 |
| Gemini CLI | POST `https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota`; JSON `{project}` | `buckets[].modelId/tokenType/remainingFraction/resetTime` |
| Antigravity current | POST `https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary`; JSON `{project}`, pinned antigravity/cli UA | `groups[].buckets[].remainingFraction/resetTime`, preserving groups/buckets |
| Antigravity legacy | POST `https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels`; JSON `{project}`, pinned old antigravity UA | `models[id].quotaInfo.remainingFraction/resetTime` |

Antigravity deliberately chooses the first daily endpoint in the pinned official
frontend, one endpoint per profile. Sandbox/production alternates are not tried.
Gemini CLI and Antigravity are distinct providers, scopes and schemas. An old CPA
may use a current upstream profile; management API and upstream profiles are
independent. These quota endpoints are private/unofficial upstream contracts;
source support is not an availability or compatibility guarantee from upstream.

Google Gemini CLI first-party anchor `fe6350238c1862dade66a9dea9080c6508475bec`
(2026-09-28) still uses retrieveUserQuota:
- [server](https://raw.githubusercontent.com/google-gemini/gemini-cli/fe6350238c1862dade66a9dea9080c6508475bec/packages/core/src/code_assist/server.ts)
- [types](https://raw.githubusercontent.com/google-gemini/gemini-cli/fe6350238c1862dade66a9dea9080c6508475bec/packages/core/src/code_assist/types.ts)

Transport must obtain Codex account ID from the credential list's sanitized
`id_token.chatgpt_account_id` claims object (or an explicitly verified equivalent
metadata field), never by downloading raw credentials. Do not confuse auth index,
filename, email, JWT subject and account ID. For the old fork Gemini CLI's
`account` can carry `email (project)`; newer list entries expose `project_id`.
Antigravity requires an explicit project; absent or conflicting context is unknown,
not a reason to use the historical frontend's hardcoded project or file download.

## Parsing and errors

- Percent fields stay in 0..100: `0.95` means **0.95 percent used**, not 95.
- Fraction fields must be numeric JSON in 0..1; convert with `(1-fraction)*100`.
  Zero is valid. No guessing, clamping, numeric-string coercion or `%` parsing.
- Amount-only, reset-only, empty and unrecognized primary shapes fail.
  A malformed declared numeric bucket fails the entire result and discards partial
  windows. Optional null windows in Claude/Codex mean not offered, not zero.
  Legacy model entries without any quotaInfo are catalogue entries and skipped;
  a declared null/malformed quotaInfo fails. At least one quota window is required.
- Named Claude windows and recognized Fable limits are preserved independently;
  unknown future limit kinds are not interpreted. No account-wide averaging or
  cross-model minimum/maximum aggregation occurs in this package.
- Names are schema paths with escaped model/group/window identities and occurrence
  suffixes to distinguish duplicate identities; model-map keys are sorted. Unique
  identities survive array reordering. Truly duplicate identities remain ordered
  occurrences because the upstream supplies no distinguishing stable ID.
- Codex resets use integer Unix **seconds** or integer relative seconds anchored
  to supplied `now`. Absolute reset takes precedence. Relative derivation remains
  visible in ResetText. Other resets are RFC3339/RFC3339Nano strings.
  Missing/null reset means unreported; malformed provided reset fails.
- Exact numeric semantics use ConfidenceReported, even when the reset timestamp is
  relatively derived. This confidence does not certify endpoint availability.
- Upstream 401: FailureAuth, fixed credential-rejected message. 403:
  FailureTransport with fixed access-forbidden message (no forbidden enum exists;
  do not mark expired). 429, 408, 5xx, timeout and network failures: FailureTransport.
  No error code/text is currently promoted to FailureQuota. Errors contain no body
  excerpts. Missing context at Parse is FailureParse; unsupported profile/provider
  is FailureUnsupported. No Plan/Balance/Windows survive failure.

## Transport safety requirements and remaining runtime unknowns

Use explicit auto/v0/v8 selection. Auto may investigate v0 after a v8 404, but must
require a successful schema-valid list before choosing it; v0 support does not
prove a v7 server. 404 can also mean disabled management, Home mode, or wrong proxy
path. 401/403/timeouts/5xx never trigger version downgrade. A 501 normalized quota
failure is unsupported for that attempt, not a trigger for an automatic retry via
another API. Management version headers are hints; latest-version is not the
running version. These rules belong to the separate transport implementation.

Quota fetches can refresh OAuth credentials: the old fork synchronously handles
Gemini CLI and Antigravity; v8.0.3's generic proxy handles Antigravity among these
four providers, not a guaranteed Gemini CLI refresh. Plugins may have their own
effects. Thus future quota fetches are not strictly zero-state-change operations.

Never probe/pop v0 `/usage-queue` or v8 `/observability/usage/queue`: even GET
consumes records. Request-token telemetry is not account remaining quota. Never
call cooldown/quota reset, Codex reset-credit consume, or raw credential/config
download as part of quota collection.

Remaining integration checks: deployed build/route availability; OAuth scopes,
project/account binding and token refresh; plugin capability/schema; live endpoint
and UA acceptance; current/legacy Antigravity availability; proxy policy. No live
success is claimed. Package tests exercise synthesized contracts only.
