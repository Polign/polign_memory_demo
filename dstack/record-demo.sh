#!/bin/sh

# Records the dstack deployment path with a real OpenAI-backed agent. The
# recording shows the private Polign service, authenticated public gateway,
# typed writes, hard container restarts, durable recall, and supersession.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ENV_FILE="${DSTACK_DEMO_ENV:-$SCRIPT_DIR/.env}"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yaml"
LOCAL_FILE="$SCRIPT_DIR/docker-compose.local.yaml"

if [ ! -f "$ENV_FILE" ]; then
    echo "missing $ENV_FILE; copy .env.example to .env and add OPENAI_API_KEY" >&2
    exit 1
fi

# This file is user-owned and intentionally ignored by Git. Export its values
# to both Compose and the recording driver without ever printing them.
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

: "${OPENAI_API_KEY:?OPENAI_API_KEY must be set in dstack/.env}"
MODEL_PROVIDER="openai"
MODEL="${MODEL:-gpt-5}"
DSTACK_USERNAME="${DSTACK_USERNAME:-polign}"
DSTACK_PASSWORD="${DSTACK_PASSWORD:?DSTACK_PASSWORD must be set in dstack/.env}"
MEMORY_DEMO_PORT="${MEMORY_DEMO_PORT:-18080}"
COMPOSE_PROGRESS="plain"
export MODEL_PROVIDER MODEL DSTACK_USERNAME DSTACK_PASSWORD MEMORY_DEMO_PORT COMPOSE_PROGRESS

compose() {
    docker compose --env-file "$ENV_FILE" \
        -p "$DSTACK_DEMO_PROJECT" \
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
        if [ "$attempt" -gt 90 ]; then
            echo "gateway did not become ready" >&2
            compose ps
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
    narrate "The authenticated inspector exposes typed state and supersession history."
    curl -fsS -u "${DSTACK_USERNAME}:${DSTACK_PASSWORD}" \
        "http://127.0.0.1:${MEMORY_DEMO_PORT}/memories/" |
        sed -n '/<td/s/<[^>]*>/ /gp' |
        awk '
            {
                gsub(/^[[:space:]]+|[[:space:]]+$/, "")
                cell[++count] = $0
            }
            count == 6 {
                printf "  %-11s %-8s %-20s %-10s confidence=%-4s status=%s\n", \
                    cell[1], cell[2], cell[3], cell[4], cell[5], cell[6]
                count = 0
            }
        '
    sleep 5
}

run_demo() {
    cleaned=false
    cleanup() {
        if [ "$cleaned" = false ]; then
            compose down -v --remove-orphans >/dev/null 2>&1 || true
        fi
    }
    trap cleanup EXIT INT TERM

    narrate "Scene 1/5 — The dstack security boundary"
    printf 'This locally executes the exact Compose workload deployed to a dstack confidential VM.\n'
    printf 'provider: OpenAI (%s); API credential injected but never printed\n' "$MODEL"
    printf 'boundary: only the authenticated gateway is published; Polign stays private\n'
    sleep 4

    narrate "Start a fresh isolated copy of the prebuilt workload."
    compose up -d --wait
    compose ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'
    sleep 5

    narrate "Only the gateway publishes a port, and it rejects anonymous access."
    anonymous_status="$(curl -sS -o /dev/null -w '%{http_code}' \
        "http://127.0.0.1:${MEMORY_DEMO_PORT}/")"
    printf 'anonymous GET / -> HTTP %s (expected 401)\n' "$anonymous_status"
    [ "$anonymous_status" = "401" ]
    sleep 4

    narrate "Scene 2/5 — OpenAI writes typed memories through private Polign"
    chat "I use Vim as my editor."
    chat "My daily step goal is 9000."
    chat "What editor do I use, and what is my daily step goal?"
    show_memories

    narrate "Scene 3/5 — Stop the agent and database; retain only the dstack volume"
    printf 'BEFORE RESTART\n'
    compose ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'
    sleep 4
    compose stop -t 0 memory-demo polign
    printf 'Both application processes are now stopped. Acknowledged memory remains on the named volume.\n'
    sleep 5
    compose start polign
    attempt=0
    until compose exec -T polign wget -q -O - http://127.0.0.1:23000/healthz >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        [ "$attempt" -gt 60 ] && { echo "Polign did not restart" >&2; exit 1; }
        sleep 1
    done
    compose start memory-demo
    wait_for_gateway
    printf '\nAFTER RESTART\n'
    compose ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'
    printf 'New agent process + new Polign process: healthy. The chat context is gone.\n'
    sleep 6

    narrate "Scene 4/5 — A new conversation recalls state recovered by Polign"
    chat "After that restart, what editor do I use and is my step goal above 8000?"

    narrate "Scene 5/5 — Single-valued state is superseded, not deleted"
    chat "I switched to Neovim."
    chat "What editors have I used over time?"
    show_memories

    narrate "Done: authenticated ingress, private storage, crash recovery, and typed history."
    printf 'On dstack, this same pinned Compose workload runs inside an attestable confidential VM.\n'
    sleep 8

    compose down -v --remove-orphans >/dev/null 2>&1
    cleaned=true
}

if [ "${1:-}" = "--run" ]; then
    DSTACK_DEMO_PROJECT="${DSTACK_DEMO_PROJECT:-polign-memory-recording-$$}"
    export DSTACK_DEMO_PROJECT
    run_demo
    exit 0
fi

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
command -v asciinema >/dev/null 2>&1 || { echo "asciinema is required" >&2; exit 1; }
command -v agg >/dev/null 2>&1 || { echo "agg is required" >&2; exit 1; }

OUTPUT_BASE="${1:-$SCRIPT_DIR/dstack-openai-demo}"
CAST_FILE="${OUTPUT_BASE}.cast"
GIF_FILE="${OUTPUT_BASE}.gif"
DSTACK_DEMO_PROJECT="polign-memory-recording-$$"
export DSTACK_DEMO_PROJECT

printf 'Preparing the application image before recording...\n'
compose build --quiet >/dev/null 2>&1
printf 'Image ready. Starting the paced recording...\n'

asciinema record --overwrite --return --idle-time-limit 6 \
    --window-size 118x36 \
    --title "Polign memory on dstack with OpenAI" \
    --command "$SCRIPT_DIR/record-demo.sh --run" \
    "$CAST_FILE"
agg --quiet --idle-time-limit 6 --last-frame-duration 8 \
    --theme github-dark "$CAST_FILE" "$GIF_FILE"
printf 'recording: %s\n' "$CAST_FILE"
printf 'shareable GIF: %s\n' "$GIF_FILE"
