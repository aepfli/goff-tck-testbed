#!/usr/bin/env bash
# Smoke test for the image: every control-API operation, and the canonical set as served.
#
# Usage: ./smoke.sh [image]   (default goff-testbed:dev; CI passes testbed:smoke)
#
# Building is not evidence that the testbed works. An image that starts and then serves
# FLAG_NOT_FOUND for the whole canonical set would pass a build-only job and fail every adoption
# downstream, which reads as a broken provider rather than a broken image.
set -euo pipefail

IMAGE=${1:-goff-testbed:dev}
NAME=goff-smoke-$$
CANONICAL=$(dirname "$0")/flags/canonical-flags.json

docker run -d --name "$NAME" -P "$IMAGE" >/dev/null
trap 'docker logs "$NAME" 2>&1 | grep -v "█" | tail -40 || true; docker rm -f "$NAME" >/dev/null || true' EXIT

port() { docker port "$NAME" "$1/tcp" | head -1 | sed 's/^0\.0\.0\.0/127.0.0.1/'; }
PROXY=$(port 1031)
CONTROL=$(port 8080)

fail() { echo "::error::$*"; exit 1; }

# ofrep <key> [context-json] -> the OFREP response body (any status)
ofrep() {
  local ctx=${2:-'{"targetingKey":"smoke"}'}
  curl -s -X POST "http://$PROXY/ofrep/v1/evaluate/flags/$1" \
    -H 'Content-Type: application/json' -d "{\"context\":$ctx}"
}
ofrep_code() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "http://$PROXY/ofrep/v1/evaluate/flags/$1" \
    -H 'Content-Type: application/json' -d '{"context":{"targetingKey":"smoke"}}'
}
value() { ofrep changing-flag | jq -r .value; }

# /healthz reports the readiness of the CONTROL API, not of the backend -- the control API stays
# up while the backend is down, otherwise an outage scenario could never be ended. POST /start is
# the endpoint that carries the guarantee, so use it, and in doing so test it: nothing below
# retries or sleeps before evaluating.
for _ in $(seq 1 60); do
  curl -sf "http://$CONTROL/healthz" >/dev/null && break
  sleep 1
done
[ "$(curl -sf "http://$CONTROL/healthz" | jq -r .status)" = ok ] || fail "/healthz is not {\"status\":\"ok\"}"
curl -sf -X POST "http://$CONTROL/start?config=default" >/dev/null || fail "/start failed"

echo "::group::canonical flag set (OFREP bulk)"
curl -s -X POST "http://$PROXY/ofrep/v1/evaluate/flags" -H 'Content-Type: application/json' \
  -d '{"context":{"targetingKey":"smoke"}}' | jq -S .
echo "::endgroup::"

# The key list is DERIVED from the canonical set, never hardcoded: a hardcoded list keeps passing
# when the spec adds a flag the image does not serve -- the exact drift this check exists to catch.
keys=$(jq -r '.flags | keys[]' "$CANONICAL")
echo "checking $(echo "$keys" | wc -l) canonical keys"
for key in $keys; do
  def=$(jq -c --arg k "$key" '.flags[$k]' "$CANONICAL")
  got=$(ofrep "$key")
  [ "$(ofrep_code "$key")" = 200 ] || fail "$key: $got"
  if [ "$(echo "$def" | jq -r .state)" = DISABLED ]; then
    # GOFF serves a disabled flag as reason DISABLED with value null; the configured value is not
    # on the wire. FINDINGS.md #3.
    [ "$(echo "$got" | jq -r .reason)" = DISABLED ] || fail "$key: want reason DISABLED, got $got"
    continue
  fi
  want_variant=$(echo "$def" | jq -r .defaultVariant)
  [ "$(echo "$got" | jq -r .variant)" = "$want_variant" ] || fail "$key: want variant $want_variant, got $got"
  # jq compares numbers numerically, so integral-float-flag's 10.0 matches GOFF's 10 here. That
  # collapse is real and recorded (FINDINGS.md #4); this check is about values, not number syntax.
  echo "$got" | jq -e --argjson d "$def" '.value == $d.variants[$d.defaultVariant]' >/dev/null \
    || fail "$key: value differs from canonical: $got"
  if [ "$(echo "$def" | jq 'has("targeting")')" = false ]; then
    [ "$(echo "$got" | jq -r .reason)" = STATIC ] || fail "$key: want reason STATIC, got $got"
  fi
done

# Absence is what the FLAG_NOT_FOUND scenario rests on.
[ "$(ofrep_code missing-flag)" = 404 ] || fail "missing-flag is not 404"
[ "$(ofrep missing-flag | jq -r .errorCode)" = FLAG_NOT_FOUND ] || fail "missing-flag is not FLAG_NOT_FOUND"

# 2^53-1 must survive exactly. Checked on the raw bytes: jq may round-trip through a double.
ofrep huge-integer-flag | grep -q '"value":9007199254740991[,}]' || fail "huge-integer-flag lost precision"

# The in-process providers do not use OFREP: they pull the whole configuration and evaluate it
# themselves (Go module, WASM). That endpoint must carry the full canonical set too.
cfg_keys=$(curl -s -X POST "http://$PROXY/v1/flag/configuration" -H 'Content-Type: application/json' \
  -d '{}' | jq -r '.flags | keys[]')
[ "$cfg_keys" = "$keys" ] || fail "/v1/flag/configuration keys differ from the canonical set"

# Targeting, all three directions: a backend that always served the targeted value would pass the
# matching case alone.
hit_id=$(jq -r '.flags["targeting-key-flag"].targeting.if[0]["=="][1]' "$CANONICAL")
[ "$(ofrep targeting-key-flag "{\"targetingKey\":\"$hit_id\"}" | jq -r .value)" = hit ] || fail "targeted key is not hit"
[ "$(ofrep targeting-key-flag '{"targetingKey":"f20bd32d-703b-48b6-bc8e-79d53c85134a"}' | jq -r .value)" = miss ] \
  || fail "non-targeted key is not miss"
[ "$(ofrep targeting-key-flag '{}' | jq -r .value)" = miss ] || fail "no targeting key is not miss"

# POST /change must not return until the new value is served. No sleep between the two.
before=$(value)
curl -sf -X POST "http://$CONTROL/change" >/dev/null || fail "/change failed"
after=$(value)
[ "$before" != "$after" ] || fail "/change did not change the served value"
curl -sf -X POST "http://$CONTROL/change" >/dev/null || fail "second /change failed"
[ "$(value)" = "$before" ] || fail "/change does not toggle between two values"

# POST /reset restores the baseline without an outage.
curl -sf -X POST "http://$CONTROL/change" >/dev/null
pid_before=$(docker exec "$NAME" pidof go-feature-flag)
curl -sf -X POST "http://$CONTROL/reset" >/dev/null || fail "/reset failed"
[ "$(value)" = "$before" ] || fail "/reset did not restore the baseline"
[ "$(docker exec "$NAME" pidof go-feature-flag)" = "$pid_before" ] || fail "/reset restarted the relay proxy"

# POST /restart preserves flag state, including a /change.
curl -sf -X POST "http://$CONTROL/change" >/dev/null
curl -sf -X POST "http://$CONTROL/restart?seconds=1" >/dev/null || fail "/restart failed"
[ "$(docker exec "$NAME" pidof go-feature-flag)" != "$pid_before" ] || fail "/restart did not restart the relay proxy"
[ "$(value)" = "$after" ] || fail "/restart lost the /change"

# POST /start reseeds: the toggle must not survive it. flagd-testbed leaks here.
curl -sf -X POST "http://$CONTROL/start?config=default" >/dev/null
[ "$(value)" = "$before" ] || fail "/start did not reset flag state"

# Unknown configurations are a 400, not a 500 and not a silent default.
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://$CONTROL/start?config=no-such-config")
[ "$code" = 400 ] || fail "/start?config=no-such-config returned $code, want 400"
[ "$(value)" = "$before" ] || fail "a rejected /start disturbed the running backend"

# POST /stop makes the backend unreachable without stopping the container.
curl -sf -X POST "http://$CONTROL/stop" >/dev/null || fail "/stop failed"
curl -s -m 5 "http://$PROXY/health" >/dev/null 2>&1 && fail "backend still reachable after /stop"
[ "$(docker inspect -f '{{.State.Running}}' "$NAME")" = true ] || fail "/stop killed the container"
curl -sf "http://$CONTROL/healthz" >/dev/null || fail "control API died with the backend"
curl -sf -X POST "http://$CONTROL/stop" >/dev/null || fail "/stop while stopped is not idempotent"

# A /change during an outage is kept and served after /restart (FINDINGS.md #12)...
curl -sf -X POST "http://$CONTROL/change" >/dev/null || fail "/change while stopped failed"
curl -sf -X POST "http://$CONTROL/restart?seconds=0" >/dev/null || fail "/restart after /stop failed"
[ "$(value)" = "$after" ] || fail "/change during the outage was not served after /restart"

# ...and /stop then /start ends an outage on the baseline, as the TCK's reconnect scenario does.
curl -sf -X POST "http://$CONTROL/stop" >/dev/null
curl -sf -X POST "http://$CONTROL/start" >/dev/null || fail "/start after /stop failed"
[ "$(value)" = "$before" ] || fail "/stop + /start did not serve the baseline"

echo "smoke test passed"
