# goff-tck-testbed

A provider-TCK backend for **GO Feature Flag**, built on the GOFF **relay proxy**.

> **Fake the config source, never the evaluation.** The relay proxy is GOFF's own binary, copied
> unmodified from `gofeatureflag/go-feature-flag:v1.57.0`. Only its configuration source is
> synthetic: a launchpad generates a flag file from the canonical set and points GOFF's file
> retriever at it.

Scratch repo, no permanent home and no published image yet.

## Ports

| Port | What |
| --- | --- |
| `1031` | relay proxy — OFREP (`/ofrep/v1/...`), in-process config pull (`/v1/flag/configuration`), change poll, streams |
| `8080` | control API — `/start /stop /restart /change /reset /healthz` |

Both are mapped dynamically (`docker-compose.yaml`, service `backend`). No API key is needed to
evaluate.

## Control API

| Op | Does | Returns when |
| --- | --- | --- |
| `POST /start?config=default` | regenerate flags from the canonical set, restart the relay proxy | the baseline is served |
| `POST /stop` | kill the relay proxy; container and control API stay up | process reaped |
| `POST /restart?seconds=5` | kill, wait, start on the **current** flag file (mutations kept) | current state is served |
| `POST /change` | toggle `changing-flag` foo ↔ bar, admin refresh | the new value is served |
| `POST /reset` | rewrite the baseline, admin refresh, **no restart** | the baseline is served |
| `GET /healthz` | `{"status":"ok"}` — the control API, not the backend | — |

"Served" means a real OFREP evaluation returned the expected value; nothing sleeps. `/change` and
`/reset` during an outage update the flag file and return without probing; `/restart` then serves
it (FINDINGS.md #12), `/start` discards it.

Other configurations: drop `<name>.json` (canonical flagd format) into `launchpad/configs/`.

## Flag mapping

Canonical (flagd format) → GOFF flag file:

- `variants` → `variations`, `defaultVariant` → `defaultRule.variation` (names unchanged)
- `state: DISABLED` → `disable: true`
- `targeting-key-flag`'s rule → `targeting: [{query: 'targetingKey eq "<uuid>"', variation: hit}]`
- `missing-flag` stays absent

What GOFF does with it — `10.0` → `10`, disabled flags served with `value: null` — is in
[FINDINGS.md](FINDINGS.md).

## Run it

```bash
docker compose up -d --build
```

```bash
./smoke.sh goff-testbed:dev
```

`smoke.sh` is what CI runs: every control op, the canonical set as served (keys derived from
`flags/canonical-flags.json`, vendored from spec `provider-tck/v0.1.0`), and targeting in all three
directions.
