#!/usr/bin/env bash
# seed.sh - populate a local Bario stack with testable timeline data.
#
# Every write goes through the public HTTP APIs; this script never touches
# PostgreSQL directly. It is safe to rerun: existing accounts are logged in,
# follows are upserts, and posts are only created up to each user's target.
#
# Usage:
#   scripts/seed.sh
#   scripts/seed.sh --help
#
# Requires: bash, curl, jq.
#
# Environment:
#   AUTH_URL        auth service base URL     (default http://localhost:8081)
#   POSTS_URL       posts service base URL    (default http://localhost:8082)
#   FOLLOWS_URL     follows service base URL  (default http://localhost:8083)
#   ADMIN_USERNAME  reserved admin username   (default admin)
#   SEED_PASSWORD   password for seeded users (default local dev password)
#
# Start the four services first; see "Local development without Kubernetes"
# in README.md.

set -euo pipefail

AUTH_URL=${AUTH_URL:-http://localhost:8081}
POSTS_URL=${POSTS_URL:-http://localhost:8082}
FOLLOWS_URL=${FOLLOWS_URL:-http://localhost:8083}
ADMIN_USERNAME=${ADMIN_USERNAME:-admin}
SEED_PASSWORD=${SEED_PASSWORD:-bario-seed-dev}

AUTH_URL=${AUTH_URL%/}
POSTS_URL=${POSTS_URL%/}
FOLLOWS_URL=${FOLLOWS_URL%/}

# Nine regular accounts around a few hub personalities. Usernames must match
# the auth service rule: 3-32 letters, digits or underscores.
USERS=(
    ada_lovelace
    grace_hopper
    alan_turing
    katherine_j
    linus_t
    margaret_h
    dennis_r
    barbara_l
    ken_t
)

# follower -> followee. Hubs (ada, grace, alan) collect most of the followers;
# everyone else follows a handful of people, and a few follow the admin.
FOLLOW_EDGES=(
    "ada_lovelace alan_turing"
    "ada_lovelace grace_hopper"
    "ada_lovelace katherine_j"
    "ada_lovelace barbara_l"
    "grace_hopper ada_lovelace"
    "grace_hopper alan_turing"
    "grace_hopper dennis_r"
    "grace_hopper margaret_h"
    "alan_turing ada_lovelace"
    "alan_turing grace_hopper"
    "alan_turing ken_t"
    "katherine_j ada_lovelace"
    "katherine_j grace_hopper"
    "katherine_j margaret_h"
    "katherine_j alan_turing"
    "katherine_j admin"
    "linus_t dennis_r"
    "linus_t ken_t"
    "linus_t ada_lovelace"
    "linus_t admin"
    "margaret_h katherine_j"
    "margaret_h ada_lovelace"
    "margaret_h grace_hopper"
    "margaret_h barbara_l"
    "margaret_h admin"
    "dennis_r ken_t"
    "dennis_r alan_turing"
    "barbara_l ada_lovelace"
    "barbara_l grace_hopper"
    "barbara_l margaret_h"
    "barbara_l katherine_j"
    "barbara_l admin"
    "ken_t dennis_r"
    "ken_t linus_t"
    "ken_t ada_lovelace"
    "ken_t admin"
    "admin ada_lovelace"
    "admin grace_hopper"
    "admin alan_turing"
)

LOREM=(
    "Lorem ipsum dolor sit amet, consectetur adipiscing elit."
    "Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua."
    "Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris."
    "Duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore."
    "Excepteur sint occaecat cupidatat non proident, sunt in culpa qui officia deserunt."
    "Nulla facilisi, integer feugiat scelerisque varius morbi enim nunc faucibus."
    "Curabitur pretium tincidunt lacus, nulla gravida orci a odio."
    "Vestibulum ante ipsum primis in faucibus orci luctus et ultrices posuere cubilia."
    "Donec velit neque, auctor sit amet aliquam vel, ullamcorper sit amet ligula."
    "Pellentesque habitant morbi tristique senectus et netus et malesuada fames."
    "Aenean tellus metus, bibendum sed, posuere ac, mattis non, nunc."
    "Fusce vulputate eleifend sapien, vestibulum purus quam, scelerisque ut."
)

usage() {
    awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
}

case "${1:-}" in
    -h|--help)
        usage
        exit 0
        ;;
    "")
        ;;
    *)
        echo "seed: unknown argument: $1" >&2
        echo "seed: try --help" >&2
        exit 2
        ;;
esac

for dep in curl jq; do
    command -v "$dep" >/dev/null 2>&1 || {
        echo "seed: missing dependency: $dep" >&2
        exit 2
    }
done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/bario-seed.XXXXXX")
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
BODY_FILE=$WORK/body

HTTP_CODE=000
HTTP_BODY=""

warn() { echo "seed: warning: $*" >&2; }
info() { echo "seed: $*"; }
die() { echo "seed: $*" >&2; exit 1; }

# api METHOD URL [TOKEN] [JSON_BODY]
# Leaves the response body in HTTP_BODY and the status in HTTP_CODE.
api() {
    local method=$1 url=$2 token=${3:-} data=${4:-}
    local args=(-sS --max-time 20 -o "$BODY_FILE" -w '%{http_code}' -X "$method")
    args+=(-H 'Accept: application/json')
    if [[ -n "$token" ]]; then
        args+=(-H "Authorization: Bearer $token")
    fi
    if [[ -n "$data" ]]; then
        args+=(-H 'Content-Type: application/json' --data-binary "$data")
    fi

    HTTP_CODE=000
    if ! HTTP_CODE=$(curl "${args[@]}" "$url" 2>/dev/null); then
        HTTP_CODE=000
    fi
    HTTP_BODY=$(cat "$BODY_FILE")
}

api_error() {
    if [[ -n "$HTTP_BODY" ]]; then
        jq -r '.error // empty' <<<"$HTTP_BODY" 2>/dev/null || true
    fi
}

check_service() {
    local name=$1 url=$2
    if ! curl -fsS --max-time 5 "$url/healthz" >/dev/null 2>&1; then
        die "$name is not reachable at $url (start the local stack first, see README)"
    fi
}

declare -A USER_ID=()
declare -A USER_TOKEN=()
declare -A USER_ADMIN=()

FAILURES=0
POSTS_CREATED=0
POSTS_FAILED=0
POSTS_AT_TARGET=0
FOLLOWS_OK=0
FOLLOWS_FAILED=0

# ensure_account USERNAME EMAIL
# Registers the account or logs in when it already exists. Populates the
# USER_* maps. Returns non-zero for accounts that cannot be used.
ensure_account() {
    local username=$1 email=$2 register_payload login_payload token id is_admin

    register_payload=$(jq -nc \
        --arg username "$username" \
        --arg password "$SEED_PASSWORD" \
        --arg email "$email" \
        '{username: $username, password: $password, email: $email}')
    login_payload=$(jq -nc \
        --arg username "$username" \
        --arg password "$SEED_PASSWORD" \
        '{username: $username, password: $password}')

    api POST "$AUTH_URL/register" "" "$register_payload"
    case "$HTTP_CODE" in
        201)
            id=$(jq -r '.id // empty' <<<"$HTTP_BODY")
            is_admin=$(jq -r '.is_admin // false' <<<"$HTTP_BODY")
            info "created account \"$username\" (id ${id:-?}, admin=$is_admin)"
            ;;
        409)
            info "account \"$username\" already exists"
            ;;
        *)
            warn "could not register \"$username\": HTTP $HTTP_CODE $(api_error)"
            return 1
            ;;
    esac

    api POST "$AUTH_URL/login" "" "$login_payload"
    if [[ "$HTTP_CODE" != "200" ]]; then
        warn "could not log in as \"$username\": HTTP $HTTP_CODE $(api_error)"
        warn "  if this account predates the seed, set SEED_PASSWORD to its password"
        return 1
    fi

    token=$(jq -r '.token // empty' <<<"$HTTP_BODY")
    id=$(jq -r '.user.id // empty' <<<"$HTTP_BODY")
    is_admin=$(jq -r '.user.is_admin // false' <<<"$HTTP_BODY")
    if [[ -z "$token" || -z "$id" ]]; then
        warn "login for \"$username\" returned no token"
        return 1
    fi

    USER_ID[$username]=$id
    USER_TOKEN[$username]=$token
    USER_ADMIN[$username]=$is_admin
    return 0
}

post_target() {
    case "$1" in
        admin) echo 4 ;;
        ada_lovelace) echo 8 ;;
        grace_hopper) echo 7 ;;
        alan_turing) echo 6 ;;
        katherine_j) echo 5 ;;
        linus_t) echo 3 ;;
        margaret_h) echo 6 ;;
        dennis_r) echo 4 ;;
        barbara_l) echo 5 ;;
        ken_t) echo 7 ;;
        *) echo 3 ;;
    esac
}

# make_content USER SEQ - one to three lorem sentences, at most 280 chars.
make_content() {
    local user=$1 seq=$2 count=${#LOREM[@]}
    local i1=$(( (seq + ${#user}) % count ))
    local i2=$(( (seq * 5 + 7) % count ))
    local i3=$(( (seq * 3 + 11) % count ))
    local content="${LOREM[$i1]} ${LOREM[$i2]}"

    if (( seq % 3 == 0 )); then
        content+=" ${LOREM[$i3]}"
    fi
    if (( ${#content} > 280 )); then
        content="${LOREM[$i1]} ${LOREM[$i2]}"
    fi
    if (( ${#content} > 280 )); then
        content="${LOREM[$i1]}"
    fi
    printf '%s' "$content"
}

info "auth=$AUTH_URL posts=$POSTS_URL follows=$FOLLOWS_URL"
check_service auth "$AUTH_URL"
check_service posts "$POSTS_URL"
check_service follows "$FOLLOWS_URL"

# --- Accounts -------------------------------------------------------------

if ensure_account "$ADMIN_USERNAME" "${ADMIN_USERNAME}@bario.local"; then
    if [[ "${USER_ADMIN[$ADMIN_USERNAME]}" == "true" ]]; then
        info "admin \"$ADMIN_USERNAME\" ready (id ${USER_ID[$ADMIN_USERNAME]})"
    else
        warn "\"$ADMIN_USERNAME\" is not flagged as admin; check the auth service ADMIN_USERNAME"
    fi
else
    FAILURES=$((FAILURES + 1))
fi

for user in "${USERS[@]}"; do
    if ! ensure_account "$user" "${user}@bario.local"; then
        FAILURES=$((FAILURES + 1))
    fi
done

# --- Follow graph ---------------------------------------------------------

for edge in "${FOLLOW_EDGES[@]}"; do
    read -r follower followee <<<"$edge"
    follower_token=${USER_TOKEN[$follower]:-}
    followee_id=${USER_ID[$followee]:-}

    if [[ -z "$follower_token" || -z "$followee_id" ]]; then
        warn "follow $follower -> $followee skipped (account unavailable)"
        FOLLOWS_FAILED=$((FOLLOWS_FAILED + 1))
        continue
    fi

    api POST "$FOLLOWS_URL/users/$followee_id/follow" "$follower_token"
    case "$HTTP_CODE" in
        201|200|204)
            FOLLOWS_OK=$((FOLLOWS_OK + 1))
            ;;
        *)
            warn "follow $follower -> $followee failed: HTTP $HTTP_CODE $(api_error)"
            FOLLOWS_FAILED=$((FOLLOWS_FAILED + 1))
            ;;
    esac
done

info "follows: $FOLLOWS_OK applied, $FOLLOWS_FAILED failed"

# --- Posts ----------------------------------------------------------------

for user in "$ADMIN_USERNAME" "${USERS[@]}"; do
    token=${USER_TOKEN[$user]:-}
    user_id=${USER_ID[$user]:-}
    if [[ -z "$token" || -z "$user_id" ]]; then
        continue
    fi

    target=$(post_target "$user")
    api GET "$POSTS_URL/users/$user_id/posts?limit=10"
    if [[ "$HTTP_CODE" == "200" ]]; then
        existing=$(jq -r '(.posts // []) | length' <<<"$HTTP_BODY")
    else
        warn "could not list posts for \"$user\": HTTP $HTTP_CODE $(api_error)"
        POSTS_FAILED=$((POSTS_FAILED + 1))
        continue
    fi

    if (( existing >= target )); then
        info "posts: \"$user\" already has $existing (target $target)"
        POSTS_AT_TARGET=$((POSTS_AT_TARGET + 1))
        continue
    fi

    need=$((target - existing))
    info "posts: creating $need for \"$user\" (existing $existing, target $target)"
    for ((i = 0; i < need; i++)); do
        content=$(make_content "$user" "$((existing + i))")
        payload=$(jq -nc --arg content "$content" '{content: $content}')
        api POST "$POSTS_URL/posts" "$token" "$payload"
        if [[ "$HTTP_CODE" == "201" ]]; then
            POSTS_CREATED=$((POSTS_CREATED + 1))
        else
            warn "post for \"$user\" failed: HTTP $HTTP_CODE $(api_error)"
            POSTS_FAILED=$((POSTS_FAILED + 1))
        fi
    done
done

info "posts: $POSTS_CREATED created, $POSTS_AT_TARGET accounts already at target, $POSTS_FAILED failed"

if (( FAILURES > 0 || FOLLOWS_FAILED > 0 || POSTS_FAILED > 0 )); then
    die "finished with errors (accounts=$FAILURES follows=$FOLLOWS_FAILED posts=$POSTS_FAILED)"
fi

info "done"
