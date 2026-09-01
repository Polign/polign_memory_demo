#!/bin/sh

# Records two isolated dstack Compose projects recovering one memory collection
# from a shared S3-backed Polign store. Machine A is killed and all of its local
# volumes are deleted before Machine B starts.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ENV_FILE="${DSTACK_DEMO_ENV:-$SCRIPT_DIR/.env}"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yaml"
LOCAL_FILE="$SCRIPT_DIR/docker-compose.local.yaml"

if [ ! -f "$ENV_FILE" ]; then
    echo "missing $ENV_FILE; configure OpenAI and S3 before recording" >&2
    exit 1
fi

set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

: "${OPENAI_API_KEY:?OPENAI_API_KEY must be set in dstack/.env}"
: "${POLIGN_STORE:?POLIGN_STORE must be an s3:// URI in dstack/.env}"
: "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID must provide the source AWS identity}"
: "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY must provide the source AWS identity}"
STORE_REGION="${POLIGN_STORE_REGION:-${AWS_REGION:-}}"
: "${STORE_REGION:?POLIGN_STORE_REGION (or AWS_REGION for direct access) must be set in dstack/.env}"
case "$POLIGN_STORE" in
    s3://*) ;;
    *) echo "POLIGN_STORE must begin with s3:// for this recording" >&2; exit 1 ;;
esac

MODEL_PROVIDER="openai"
MODEL="${MODEL:-gpt-5}"
DSTACK_USERNAME="${DSTACK_USERNAME:-polign}"
DSTACK_PASSWORD="${DSTACK_PASSWORD:?DSTACK_PASSWORD must be set in dstack/.env}"
MEMORY_DEMO_PORT="${MEMORY_DEMO_PORT:-18080}"
COMPOSE_PROGRESS="plain"
export MODEL_PROVIDER MODEL DSTACK_USERNAME DSTACK_PASSWORD MEMORY_DEMO_PORT
export POLIGN_STORE AWS_REGION POLIGN_STORE_REGION COMPOSE_PROGRESS

compose() {
    project="$1"
    shift
    docker compose --env-file "$ENV_FILE" \
        -p "$project" \
        -f "$COMPOSE_FILE" -f "$LOCAL_FILE" "$@"
}

narrate() {
    printf '\n\033[1;36m# %s\033[0m\n' "$1"
    sleep 4
}

wait_for_gateway() {
    attempt=0
    until curl -fsS "http://127.0.0.1:${MEMORY_DEMO_PORT}/healthz" >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "$attempt" -gt 120 ]; then
            echo "gateway did not become ready" >&2
            exit 1
        fi
        sleep 2
    done
}

chat() {
    prompt="$1"
    printf '\n\033[1;33myou>\033[0m %s\n' "$prompt"
    payload="$(jq -cn --arg message "$prompt" '{message: $message}')"
    response="$(curl -fsS \
        -u "${DSTACK_USERNAME}:${DSTACK_PASSWORD}" \
        -H 'Content-Type: application/json' \
        --data "$payload" \
        "http://127.0.0.1:${MEMORY_DEMO_PORT}/api/chat")"
    printf '\033[1;32mgpt>\033[0m %s\n' "$(printf '%s' "$response" | jq -r '.reply')"
    sleep 3
}

show_memories() {
    MEMORY_PAGE="$(curl -fsS -u "${DSTACK_USERNAME}:${DSTACK_PASSWORD}" \
        "http://127.0.0.1:${MEMORY_DEMO_PORT}/memories/")" || {
        echo "memory inspector is unavailable" >&2
        return 1
    }
    printf '%s' "$MEMORY_PAGE" |
        sed -n '/<td/s/<[^>]*>/ /gp' |
        awk '
            {
                gsub(/^[[:space:]]+|[[:space:]]+$/, "")
                cell[++count] = $0
            }
            count == 6 {
                printf "  %-11s %-8s %-20s %-18s status=%s\n", \
                    cell[1], cell[2], cell[3], cell[4], cell[6]
                count = 0
            }
        '
    sleep 6
}

run_demo() {
    machine_a="$DSTACK_S3_PROJECT_A"
    machine_b="$DSTACK_S3_PROJECT_B"
    cleaned_a=false
    cleaned_b=false
    cleanup() {
        if [ "$cleaned_a" = false ]; then
            compose "$machine_a" down -v --remove-orphans >/dev/null 2>&1 || true
        fi
        if [ "$cleaned_b" = false ]; then
            compose "$machine_b" down -v --remove-orphans >/dev/null 2>&1 || true
        fi
    }
    trap cleanup EXIT INT TERM

    narrate "Scene 1/6 — Two machines, one S3-backed source of truth"
    printf 'Machine A and Machine B use different containers, caches, and local volumes.\n'
    printf 'Shared backend: Amazon S3 (bucket and prefix hidden from the recording).\n'
    if [ -n "${POLIGN_STORE_ROLE_ARN:-}" ]; then
        printf 'AWS access: sealed source principal -> STS AssumeRole -> short-lived store credentials.\n'
    else
        printf 'AWS access: sealed source principal with direct S3 permissions (role assumption is preferred).\n'
    fi
    printf 'Collection: %s\n' "$POLIGN_COLLECTION"
    printf 'Only one machine runs at a time; no local state crosses the handoff.\n'
    sleep 6

    narrate "Scene 2/6 — Boot Machine A and create a richer user profile"
    compose "$machine_a" up -d --wait
    compose "$machine_a" ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'
    sleep 5

    chat "I live in Seattle, work at Northstar Labs, and my primary programming language is Go."
    chat "I'm working on Project Atlas. I like espresso and trail running, and I use dark mode."
    chat "Summarize what you know about my location, work, project, and preferences."

    narrate "Machine A's inspector shows the typed records acknowledged to the S3-backed store."
    show_memories

    narrate "Scene 3/6 — Machine A dies without a graceful database shutdown"
    compose "$machine_a" kill -s SIGKILL gateway memory-demo polign
    printf 'Machine A is dead. Deleting every container, cache, and local volume it owned...\n'
    compose "$machine_a" down -v --remove-orphans >/dev/null 2>&1
    cleaned_a=true
    printf 'Machine A local state: removed. S3 is the only remaining database state.\n'
    sleep 7

    narrate "Scene 4/6 — Cold-start Machine B with fresh local volumes and the same S3 prefix"
    compose "$machine_b" up -d --wait
    wait_for_gateway
    compose "$machine_b" ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'
    printf 'Machine B is healthy after replaying the durable S3 write log.\n'
    sleep 7

    narrate "Scene 5/6 — A new OpenAI conversation continues from durable memory"
    chat "This is a new machine and a new conversation. What do you remember about my work and preferences?"
    chat "I moved to Portland and switched my primary programming language to Rust."
    chat "Give me a current handoff summary, and include where I lived and what language I used before."

    narrate "Scene 6/6 — Machine B exposes both current state and cross-machine history"
    show_memories
    for expected in Seattle Portland Go Rust superseded; do
        case "$MEMORY_PAGE" in
            *"$expected"*) ;;
            *) echo "final memory history is missing $expected" >&2; exit 1 ;;
        esac
    done
    printf 'Seattle and Go remain as superseded history; Portland and Rust are active.\n'
    printf 'The conversation process died. The S3-backed typed memory continued.\n'
    sleep 10

    compose "$machine_b" down -v --remove-orphans >/dev/null 2>&1
    cleaned_b=true
}

if [ "${1:-}" = "--run" ]; then
    run_demo
    exit 0
fi

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
command -v asciinema >/dev/null 2>&1 || { echo "asciinema is required" >&2; exit 1; }
command -v agg >/dev/null 2>&1 || { echo "agg is required" >&2; exit 1; }

run_id="$(date -u +%Y%m%d%H%M%S)-$$"
POLIGN_COLLECTION="s3_failover_${run_id}"
DSTACK_S3_PROJECT_A="node-a"
DSTACK_S3_PROJECT_B="node-b"
export POLIGN_COLLECTION DSTACK_S3_PROJECT_A DSTACK_S3_PROJECT_B

for project in "$DSTACK_S3_PROJECT_A" "$DSTACK_S3_PROJECT_B"; do
    if [ -n "$(compose "$project" ps -aq)" ]; then
        echo "Compose project '$project' already exists; remove it before recording" >&2
        exit 1
    fi
done

OUTPUT_BASE="${1:-$SCRIPT_DIR/dstack-s3-failover-demo}"
CAST_FILE="${OUTPUT_BASE}.cast"
GIF_FILE="${OUTPUT_BASE}.gif"

printf 'Preparing the application image before recording...\n'
compose "$DSTACK_S3_PROJECT_A" build --quiet >/dev/null 2>&1
printf 'Image ready. Starting the S3 failover recording...\n'

asciinema record --overwrite --return --idle-time-limit 7 \
    --window-size 118x36 \
    --title "Polign memory: dstack machine failover through S3" \
    --command "$SCRIPT_DIR/record-s3-failover-demo.sh --run" \
    "$CAST_FILE"
agg --quiet --idle-time-limit 7 --last-frame-duration 10 \
    --theme github-dark "$CAST_FILE" "$GIF_FILE"
printf 'recording: %s\n' "$CAST_FILE"
printf 'shareable GIF: %s\n' "$GIF_FILE"
