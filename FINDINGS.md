# Findings

**runtime** = observed against `gofeatureflag/go-feature-flag:v1.57.0` (and this image built on
it), WSL Docker, 2026-10-20. **source** = read from code, pre-registered in
the GOFF TCK plan (§7, kept outside this repo) *before* any adoption runs, so "the suite found
things" stays a prediction rather than a post-hoc story. Nothing marked *source* has been
reproduced through a provider yet.

## Backend (GOFF relay proxy)

### 1. A failed start exits with status 0 — *runtime*

Missing, invalid or empty flag file with `startWithRetrieverError: false` (the default): GOFF logs
`impossible to start GO Feature Flag` and exits **0**. A supervisor cannot tell a failed start from
a clean shutdown by exit status. The launchpad's probe watches for the process exiting instead.

### 2. `startWithRetrieverError: true` makes `/health` lie — *runtime*

The proxy starts, answers `/health` `200 {"initialized":true}`, and serves `404 FLAG_NOT_FOUND`
for every flag while it retries the retriever. A readiness probe on `/health` would wave this
through. The testbed keeps the default (`false`).

### 3. Disabled flags lose their value and their variant on the wire — *runtime*

`disable: true` → OFREP `200 {"value":null,"reason":"DISABLED","variant":"SdkDefault"}` for a
single flag, but `"variant":""` in the bulk response. The configured value is not sent at all.
Correct outcome for a provider is the caller's default with reason `DISABLED`; a provider that
type-checks `value` before looking at `reason` will report `TYPE_MISMATCH` (or `GENERAL`) instead.
That is what the `disabled-*` scenarios will show.

### 4. `10.0` becomes `10` everywhere — *runtime*

GOFF decodes flag config into `any`, so `integral-float-flag` is `float64(10)` internally and `10`
on OFREP *and* on `/v1/flag/configuration` (what the in-process providers pull). The lossless half
of `@numeric-coercion` is `notApplicable` against this backend: not a provider failure, and an
adoption should not count it as one. The generator writes the literal `10.0`; nothing downstream
keeps it.

### 5. OFREP never type-checks — *runtime*

The OFREP request carries no type, so the relay proxy returns the stored value (`wrong-flag` →
`"uno"`, `float-flag` → `0.5`). `TYPE_MISMATCH` is decided entirely in the client. The core's
float→int narrowing (#9) is therefore unreachable through OFREP, by construction.

### 6. Admin refresh is synchronous, but `refreshed` is not a change signal — *runtime*

`POST /admin/v1/retriever/refresh`: 0 stale reads in 100 rewrite→refresh→evaluate cycles, ~10ms.
But `{"refreshed":true}` comes back when nothing changed too, and `{"refreshed":false}` when the
file is unreadable — after which the last good set keeps being served. The launchpad treats
`false` as an error and relies on its probe, not on the body, for "served".

### 7. `pollingInterval` is clamped to 1000ms silently — *runtime*

100 / 500 / 1000 all pick up a file change in 930–1010ms; no warning is logged. Irrelevant to the
testbed (it never waits on polling), relevant to anyone tuning for faster change detection.

### 8. No early-ready window — *runtime, a non-finding*

GOFF loads the retriever before binding the port. Over 5 cold starts the first evaluation never
trailed the first `/health` 200. Not a flagd#2047 analogue. `/start` still probes, because the
contract is "served", not "healthy" — and because of #1.

## Providers (predicted, *source*)

9. **Core narrows any float to int.** `modules/core/evaluation/evaluation.go`:
   `case float64: if expectedType == "int" { convertedValue = int(value) }`. `float-flag` as an
   integer → `0`, reason `STATIC`, no error. Reachable in-process (Go module, WASM) only — see #5.
10. **In-process and remote disagree about type strictness** as a direct consequence of #5 and #9:
    same provider, two modes, two answers. Same shape as go-sdk-contrib#939.
11. **Go in-process emits `PROVIDER_CONFIGURATION_CHANGED` during `Init()`**, before `READY`
    (`handleRefreshResult(changed, nil)`).
12. **Go in-process recovers to `READY` only on a *not-modified* refresh.** `/stop` → `/change` →
    `/restart` yields `CONFIGURATION_CHANGED` and the provider stays `STALE`. This testbed can
    drive exactly that: `/change` while stopped updates the flag file and `/restart` serves it.
    No current scenario does.
13. **Go remote never fails `Init()`** — returns `nil` immediately with the cache off, swallows the
    fetch error with it on. `@unavailable` is unsatisfiable; the provider is `READY` against a
    dead backend.
14. **Remote caches by default and rewrites the reason to `CACHED`.** Every OFREP response
    carries `metadata.gofeatureflag_cacheable: true` (*runtime*), so repeat evaluations hit it.
    Adoptions must disable the cache.
15. **Three hosts, one engine.** Go evaluates in-process via the Go module, Java and JS via a WASM
    build of the same core. Divergence between them is the highest-value thing this testbed can
    surface.

## Testbed notes

- **No connection parameters needed.** No evaluation keys are configured, so a provider needs host
  and port only — unlike Flagsmith (flagsmith-tck-testbed FINDINGS #1). The admin key
  (`provider-tck-admin-key`) is internal to the launchpad.
- **Reasons are GOFF's, not normalised.** `targeting-key-flag` resolves `TARGETING_MATCH` on a hit
  and `DEFAULT` on a miss (it has a rule, so not `STATIC`); everything else is `STATIC`.
- **Op latencies** (in-container, warm): `/start` ~75ms (process restart), `/restart?seconds=0`
  ~60ms, `/change` and `/reset` 2–4ms, `/stop` ~5ms. 50 `/change` and 30 `/change`+`/start` cycles,
  each followed by an immediate evaluation: 0 stale reads.
