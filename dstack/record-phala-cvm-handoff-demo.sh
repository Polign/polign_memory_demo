#!/bin/sh

# Record a real Phala CVM handoff between two pre-provisioned instances. The
# browser transcript is intentionally not transferred; both agents continue
# from the same typed memory collection in the S3-backed Polign store.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ENV_FILE="${DSTACK_DEMO_ENV:-$SCRIPT_DIR/.env}"
PHALA_BIN="${PHALA_BIN:-phala}"

INSTANCE_A="${PHALA_HANDOFF_A_NAME:-polign-memory-handoff-a}"
INSTANCE_B="${PHALA_HANDOFF_B_NAME:-polign-memory-handoff-b}"
ENDPOINT_A="${PHALA_HANDOFF_A_ENDPOINT:-https://74ac5dde571d1fb69b695624c148f225b4f144a0-18080.dstack-pha-prod9.phala.network}"
ENDPOINT_B="${PHALA_HANDOFF_B_ENDPOINT:-https://e70aec24f95eb30f804c1be73992215799fc1947-18080.dstack-pha-prod5.phala.network}"

if [ ! -f "$ENV_FILE" ]; then
    echo "missing $ENV_FILE" >&2
    exit 1
fi

set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

: "${DSTACK_USERNAME:?DSTACK_USERNAME must be set in dstack/.env}"
: "${DSTACK_PASSWORD:?DSTACK_PASSWORD must be set in dstack/.env}"

narrate() {
    printf '\n\033[1;36m# %s\033[0m\n' "$1"
    sleep 3
}

cvm_status() {
    "$PHALA_BIN" cvms get "$1" --json 2>/dev/null | jq -r '.status // "unknown"'
}

wait_for_status() {
    name="$1"
    wanted="$2"
    attempt=0
    while [ "$(cvm_status "$name")" != "$wanted" ]; do
        attempt=$((attempt + 1))
        if [ "$attempt" -gt 90 ]; then
            echo "$name did not reach $wanted" >&2
            return 1
        fi
        sleep 2
    done
}

wait_for_health() {
    endpoint="$1"
    attempt=0
    until curl -fsS --max-time 5 "$endpoint/healthz" >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "$attempt" -gt 90 ]; then
            echo "$endpoint did not become healthy" >&2
            return 1
        fi
        sleep 2
    done
}

reset_chat() {
    endpoint="$1"
    curl -fsS --max-time 30 \
        -u "${DSTACK_USERNAME}:${DSTACK_PASSWORD}" \
        -X POST "$endpoint/api/reset" >/dev/null
}

chat_response() {
    endpoint="$1"
    prompt="$2"
    payload="$(jq -cn --arg message "$prompt" '{message: $message}')"
    curl -fsS --max-time 150 \
        -u "${DSTACK_USERNAME}:${DSTACK_PASSWORD}" \
        -H 'Content-Type: application/json' \
        --data "$payload" \
        "$endpoint/api/chat"
}

chat() {
    endpoint="$1"
    prompt="$2"
    printf '\n\033[1;33myou>\033[0m %s\n' "$prompt"
    response="$(chat_response "$endpoint" "$prompt")"
    printf '\033[1;32mgpt>\033[0m %s\n' "$(printf '%s' "$response" | jq -r '.reply')"
    printf '%s' "$response" | jq -r '.retrieved_from[]?' | while IFS= read -r source; do
        printf '\033[2;32mRetrieved from %s\033[0m\n' "$source"
    done
    sleep 3
}

stop_cvm() {
    "$PHALA_BIN" cvms stop "$1" --json >/dev/null
    wait_for_status "$1" stopped
}

start_cvm() {
    "$PHALA_BIN" cvms start "$1" --json >/dev/null
    wait_for_status "$1" running
}

run_recorded_demo() {
    narrate "Two pre-provisioned confidential VMs, one durable S3-backed memory"
    printf 'Instance A: prod9 / US-WEST-1 / running\n'
    printf 'Instance B: prod5 / US-WEST-1 / stopped and ready\n'
    printf 'Shared memory collection: phala_handoff_demo\n'
    printf 'The chat transcript stays local. Typed memory is durable in Polign on S3.\n'
    sleep 5

    reset_chat "$ENDPOINT_A"
    narrate "Instance A — start the conversation and store two typed records"
    chat "$ENDPOINT_A" "I'm working on Project Lighthouse for the Phala CVM handoff, and I own a demo token named TDX-42."
    chat "$ENDPOINT_A" "Before we switch machines, remind me which project I'm working on and which demo token I own."

    narrate "Switch machines — stop A, then start the already-provisioned B"
    started_at="$(date +%s)"
    printf 'Stopping %s ... ' "$INSTANCE_A"
    stop_cvm "$INSTANCE_A"
    printf 'stopped.\n'
    printf 'Starting %s ... ' "$INSTANCE_B"
    start_cvm "$INSTANCE_B"
    wait_for_health "$ENDPOINT_B"
    elapsed=$(( $(date +%s) - started_at ))
    printf 'healthy. Handoff completed in %s seconds.\n' "$elapsed"
    printf 'No image build or deployment occurred during the switch.\n'
    sleep 5

    reset_chat "$ENDPOINT_B"
    narrate "Instance B — a fresh conversation continues from the same durable memory"
    chat "$ENDPOINT_B" "This is a new CVM and a fresh conversation. Continue where I left off: which project am I working on and which demo token do I own?"
    chat "$ENDPOINT_B" "Now use Wikipedia to tell me who wrote Pride and Prejudice in one sentence and cite the article."

    narrate "The process and transcript changed; the typed memory did not"
    printf 'Instance B recovered Project Lighthouse and TDX-42 from S3.\n'
    printf 'Wikipedia remained a separate read-only collection with visible provenance.\n'
    sleep 8
}

restore_initial_state() {
    set +e
    if [ "$(cvm_status "$INSTANCE_B")" != "stopped" ]; then
        "$PHALA_BIN" cvms stop "$INSTANCE_B" --json >/dev/null 2>&1
        wait_for_status "$INSTANCE_B" stopped >/dev/null 2>&1
    fi
    if [ "$(cvm_status "$INSTANCE_A")" != "running" ]; then
        "$PHALA_BIN" cvms start "$INSTANCE_A" --json >/dev/null 2>&1
        wait_for_status "$INSTANCE_A" running >/dev/null 2>&1
    fi
    wait_for_health "$ENDPOINT_A" >/dev/null 2>&1
    set -e
}

cleanup_demo_records() {
    endpoint="$1"
    reset_chat "$endpoint"
    chat_response "$endpoint" "Forget that I own the TDX-42 demo token." >/dev/null
    chat_response "$endpoint" "Forget that I'm working on Project Lighthouse for the Phala CVM handoff." >/dev/null
    reset_chat "$endpoint"
    records="$(curl -fsS --max-time 30 \
        -u "${DSTACK_USERNAME}:${DSTACK_PASSWORD}" \
        "$endpoint/memories/")"
    printf '%s' "$records" | grep -q '0 records'
}

if [ "${1:-}" = "--run" ]; then
    run_recorded_demo
    exit 0
fi

for command in "$PHALA_BIN" curl jq asciinema agg; do
    command -v "$command" >/dev/null 2>&1 || {
        echo "missing required command: $command" >&2
        exit 1
    }
done

if [ "$(cvm_status "$INSTANCE_A")" != "running" ]; then
    echo "$INSTANCE_A must be running before recording" >&2
    exit 1
fi
if [ "$(cvm_status "$INSTANCE_B")" != "stopped" ]; then
    echo "$INSTANCE_B must be stopped before recording" >&2
    exit 1
fi
wait_for_health "$ENDPOINT_A"

OUTPUT_BASE="${1:-$SCRIPT_DIR/phala-two-cvm-handoff-demo}"
CAST_FILE="${OUTPUT_BASE}.cast"
GIF_FILE="${OUTPUT_BASE}.gif"

trap restore_initial_state EXIT INT TERM

asciinema record --overwrite --return --idle-time-limit 7 \
    --window-size 120x36 \
    --title "Polign memory: conversation handoff between two Phala CVMs" \
    --command "$SCRIPT_DIR/record-phala-cvm-handoff-demo.sh --run" \
    "$CAST_FILE"

# B is active at the end of the captured handoff. Remove only the two isolated
# demo records, then return the pair to its initial A-running/B-stopped state.
cleanup_demo_records "$ENDPOINT_B"
restore_initial_state
trap - EXIT INT TERM

agg --quiet --idle-time-limit 7 --last-frame-duration 8 \
    --theme github-dark "$CAST_FILE" "$GIF_FILE"

printf 'recording: %s\n' "$CAST_FILE"
printf 'shareable GIF: %s\n' "$GIF_FILE"
