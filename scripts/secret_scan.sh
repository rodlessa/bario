#!/bin/sh
# secret_scan.sh - deterministic, offline secret scanner.
#
# Usage:
#   scripts/secret_scan.sh [--staged]
#   scripts/secret_scan.sh [--] FILE [FILE...]
#
# With no arguments (or --staged) it scans the lines added by
# `git diff --cached`. With file arguments it scans those files in full.
#
# No network, no LLM, no external services: grep and awk only.
# Exit codes: 0 clean, 1 potential secrets found, 2 usage/environment error.
#
# A line containing the marker `secret-scan:allow` is skipped. Use it only
# for deliberate fixtures, never for real credentials.

set -eu

# Byte-oriented, locale-independent matching: identical results everywhere.
LC_ALL=C
export LC_ALL

PROG=${0##*/}
TAB=$(printf '\t')

usage() {
    cat <<EOF
Usage: $PROG [--staged] [--] [FILE...]

  --staged     scan staged changes with 'git diff --cached' (default)
  FILE...      scan the given files in full
  -h, --help   show this help

Detects provider keys, private keys, .env files with values, secret-like
assignments, and high-entropy tokens in assignment or bearer contexts.
Exit status is 1 when a potential secret is found.
EOF
}

case "${1:-}" in
    -h|--help)
        usage
        exit 0
        ;;
    --staged)
        MODE=staged
        shift
        [ "$#" -eq 0 ] || { echo "$PROG: --staged does not take file arguments" >&2; exit 2; }
        ;;
    --)
        shift
        MODE=files
        ;;
    -*)
        echo "$PROG: unknown option: $1" >&2
        usage >&2
        exit 2
        ;;
    "")
        MODE=staged
        ;;
    *)
        MODE=files
        ;;
esac

if [ "$MODE" = "staged" ]; then
    git rev-parse --is-inside-work-tree >/dev/null 2>&1 || {
        echo "$PROG: not inside a git work tree" >&2
        exit 2
    }
    # Diff paths are interpreted from the repository root.
    cd "$(git rev-parse --show-toplevel)"
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/secret_scan.XXXXXX") || {
    echo "$PROG: cannot create temporary directory" >&2
    exit 2
}
trap 'rm -rf "$WORK"' EXIT HUP INT TERM

INPUT=$WORK/input
FINDINGS=$WORK/findings
: >"$INPUT"
: >"$FINDINGS"

# ---------------------------------------------------------------------------
# Input collection: one record per line, "path <TAB> lineno <TAB> content".
# ---------------------------------------------------------------------------

append_file() {
    awk -v f="$1" '{ printf "%s\t%d\t%s\n", f, NR, $0 }' "$1"
}

append_diff() {
    git -c core.quotePath=false diff --cached --no-color --no-ext-diff \
        --unified=0 --diff-filter=ACMR |
        awk '
            /^diff --git / { file = ""; next }
            /^\+\+\+ / {
                p = substr($0, 5)
                if (p == "/dev/null") { file = ""; next }
                sub(/^b\//, "", p)
                file = p
                next
            }
            /^@@ / {
                if (match($0, /\+[0-9]+/))
                    line = substr($0, RSTART + 1, RLENGTH - 1) + 0
                next
            }
            {
                if (file == "") next
                if (substr($0, 1, 1) == "+") {
                    print file "\t" line "\t" substr($0, 2)
                    line++
                } else if (substr($0, 1, 1) == " ") {
                    line++
                }
            }
        '
}

if [ "$MODE" = "staged" ]; then
    append_diff >>"$INPUT"
    # A .env file should never be committed at all: inspect the full staged
    # blob, not only the added lines, so an existing value cannot slip in
    # when the file is modified elsewhere.
    git -c core.quotePath=false diff --cached --name-only --diff-filter=ACMR |
        while IFS= read -r name; do
            [ -n "$name" ] || continue
            base=${name##*/}
            case "$base" in
                .env|.env.*)
                    git show ":$name" 2>/dev/null | awk -v f="$name" '{ printf "%s\t%d\t%s\n", f, NR, $0 }' >>"$INPUT"
                    ;;
            esac
        done
    if [ ! -s "$INPUT" ]; then
        echo "$PROG: no staged changes to scan"
        exit 0
    fi
else
    nfiles=0
    for f in "$@"; do
        if [ ! -e "$f" ]; then
            echo "$PROG: skipping missing path: $f" >&2
            continue
        fi
        if [ ! -f "$f" ]; then
            echo "$PROG: skipping non-regular path: $f" >&2
            continue
        fi
        # grep -I treats files with NUL bytes as binary; skip them so no
        # binary content is ever fed to the text scanners. An empty file is
        # still a valid (clean) input.
        if [ -s "$f" ] && ! grep -Iq '' "$f" 2>/dev/null; then
            continue
        fi
        append_file "$f" >>"$INPUT"
        nfiles=$((nfiles + 1))
    done
    if [ "$nfiles" -eq 0 ]; then
        echo "$PROG: no readable text files to scan" >&2
        exit 2
    fi
fi

# Honour the per-line allow marker before any rule runs.
grep -v 'secret-scan:allow' "$INPUT" >"$WORK/input.filtered" || :
if [ -s "$WORK/input.filtered" ]; then
    mv "$WORK/input.filtered" "$INPUT"
else
    : >"$INPUT"
fi

# ---------------------------------------------------------------------------
# Simple pattern rules.
#
# Table format: id@@label@@flags@@extended-regex
# flags is "i" for case-insensitive or "-" for case-sensitive.
# ---------------------------------------------------------------------------

scan_patterns() {
    while IFS= read -r rule; do
        [ -n "$rule" ] || continue
        id=${rule%%@@*}
        rest=${rule#*@@}
        label=${rest%%@@*}
        rest=${rest#*@@}
        flags=${rest%%@@*}
        regex=${rest#*@@}

        if [ "$flags" = "i" ]; then
            matches=$(grep -niE -- "$regex" "$INPUT" 2>/dev/null || :)
        else
            matches=$(grep -nE -- "$regex" "$INPUT" 2>/dev/null || :)
        fi
        [ -n "$matches" ] || continue

        printf '%s\n' "$matches" |
            sed 's/^[0-9][0-9]*://' |
            cut -f1,2 |
            while IFS="$TAB" read -r file line; do
                [ -n "$file" ] || continue
                printf '%s\t%s\t%s\t%s\n' "$file" "$line" "$id" "$label" >>"$FINDINGS"
            done
    done <<'RULES'
aws-access-key@@AWS access key ID@@-@@(AKIA|ASIA)[0-9A-Z]{16}
github-token@@GitHub token@@-@@(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})
openai-key@@OpenAI API key@@-@@sk-[A-Za-z0-9_-]{20,}
opencode-key@@opencode API key@@-@@oc_sk_[A-Za-z0-9_-]{10,}
slack-token@@Slack token@@-@@xox[baprs]-[A-Za-z0-9-]{10,}
stripe-key@@Stripe secret key@@-@@sk_(live|test)_[A-Za-z0-9]{10,}
google-api-key@@Google API key@@-@@AIza[0-9A-Za-z_-]{30,}
private-key@@Private key block (PEM or OpenSSH)@@-@@-----BEGIN ([A-Z0-9]+ )?PRIVATE KEY-----
RULES
}

# Secret-like assignments: key[, key] <sep> value, with a non-empty literal
# value. The validator drops variable references and placeholders.
cat >"$WORK/assign.awk" <<'AWK'
BEGIN { FS = "\t" }

function suppressed(v,   lv) {
    gsub(/^[ \t\r]+|[ \t\r]+$/, "", v)
    if (v == "") return 1
    lv = tolower(v)
    if (lv == "true" || lv == "false" || lv == "null" || lv == "nil" ||
        lv == "none" || lv == "undefined") return 1
    if (v ~ /^\$/ || v ~ /[(){}$<>%]/) return 1
    if (lv == "string" || lv == "number" || lv == "boolean" ||
        lv == "integer" || lv == "required" || lv == "optional") return 1
    if (v ~ /^\*+$/ || v ~ /^x+$/ || v ~ /^\.+$/ || v ~ /^-+$/) return 1
    return 0
}

{
    path = $1
    lineno = $2
    content = $0
    sub(/^[^\t]*\t[^\t]*\t/, "", content)

    lc = tolower(content)
    re = "(^|[^a-z0-9])(api[_-]?key|apikey|secret|token|password|passwd|pwd|access[_-]?key|client[_-]?secret|auth[_-]?token)[\"']?[ \t]*[=:][ \t]*"
    if (!match(lc, re)) next

    keyraw = substr(content, RSTART, RLENGTH)
    quote = ""
    value = substr(content, RSTART + RLENGTH)
    if (substr(value, 1, 1) == "=") next
    first = substr(value, 1, 1)
    if (first == "\"" || first == "'" || first == "`") {
        quote = first
        rest = substr(value, 2)
        close_at = index(rest, quote)
        if (close_at == 0) next
        value = substr(rest, 1, close_at - 1)
    } else {
        sub(/[ \t;,#`].*$/, "", value)
    }

    if (suppressed(value)) next

    # Ignore references where the value is just a variable named after the key,
    # e.g. an object field copied into a local: that is not a credential.
    key = substr(lc, RSTART, RLENGTH)
    sub(/["']?[ \t]*[=:][ \t]*$/, "", key)
    sub(/^[^a-z0-9]/, "", key)
    if (tolower(value) == key) next

    print path "\t" lineno "\tsecret-assignment\tSecret-like assignment with a literal value"
}
AWK

# ---------------------------------------------------------------------------
# High-entropy strings (base64/hex, 32+ chars) in assignment or bearer
# contexts. Shannon entropy filters out repeated or word-like runs.
# ---------------------------------------------------------------------------

cat >"$WORK/entropy.awk" <<'AWK'
function shannon(t,   i, n, c, k, p, h) {
    n = length(t)
    for (k in c) delete c[k]
    for (i = 1; i <= n; i++) c[substr(t, i, 1)]++
    h = 0
    for (k in c) {
        p = c[k] / n
        h -= p * log(p) / log(2)
    }
    return h
}

BEGIN { FS = "\t" }

{
    path = $1
    lineno = $2
    content = $0
    sub(/^[^\t]*\t[^\t]*\t/, "", content)

    pos = 1
    s = content
    while (match(s, /[A-Za-z0-9+\/=_.-]{32,}/)) {
        tok = substr(s, RSTART, RLENGTH)
        at = pos + RSTART - 1
        pos = at + RLENGTH
        s = substr(content, pos)

        pre = substr(content, 1, at - 1)
        if (length(pre) > 80) pre = substr(pre, length(pre) - 79)
        pl = tolower(pre)

        in_context = 0
        if (pl ~ /bearer[ \t"']*$/)
            in_context = 1
        else if (pl ~ /(key|secret|token|password|passwd|apikey|api_key|api-key|auth|credential|private|session)[^=:]*[=:][ \t"']*$/)
            in_context = 1
        if (!in_context) continue

        if (tok ~ /^[0-9a-fA-F]+$/) {
            if (shannon(tok) < 3.0) continue
        } else {
            if (tok ~ /^[a-z.]+$/) continue
            if (tok !~ /[0-9A-Z]/) continue
            if (shannon(tok) < 3.5) continue
        }

        print path "\t" lineno "\thigh-entropy\tHigh-entropy token in an assignment or bearer context"
    }
}
AWK

# ---------------------------------------------------------------------------
# .env files: any file named .env or .env.* that carries a value.
# ---------------------------------------------------------------------------

cat >"$WORK/dotenv.awk" <<'AWK'
BEGIN { FS = "\t" }

function suppressed(v,   q) {
    gsub(/^[ \t\r]+|[ \t\r]+$/, "", v)
    if (v == "") return 1
    q = substr(v, 1, 1)
    if ((q == "\"" || q == "'") && length(v) >= 2) {
        v = substr(v, 2)
        if (substr(v, length(v), 1) == q) v = substr(v, 1, length(v) - 1)
    }
    if (v == "") return 1
    if (v ~ /^\$/) return 1
    if (v ~ /^\{\{.*\}\}$/ || v ~ /^<.*>$/) return 1
    if (v ~ /^\*+$/ || v ~ /^\.+$/ || v ~ /^-+$/) return 1
    return 0
}

{
    path = $1
    lineno = $2
    content = $0
    sub(/^[^\t]*\t[^\t]*\t/, "", content)

    n = split(path, parts, "/")
    base = parts[n]
    if (base != ".env" && base !~ /^\.env\./) next

    line = content
    sub(/^[ \t]+/, "", line)
    sub(/[ \t\r]+$/, "", line)
    if (line == "" || substr(line, 1, 1) == "#") next
    sub(/^export[ \t]+/, "", line)
    if (line !~ /^[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*/) next

    value = line
    sub(/^[^=]*=[ \t]*/, "", value)
    sub(/[ \t]+$/, "", value)
    if (suppressed(value)) next

    print path "\t" lineno "\tdotenv-values\t.env file carrying values"
}
AWK

scan_patterns
awk -f "$WORK/assign.awk" "$INPUT" >>"$FINDINGS"
awk -f "$WORK/entropy.awk" "$INPUT" >>"$FINDINGS"
awk -f "$WORK/dotenv.awk" "$INPUT" >>"$FINDINGS"

# ---------------------------------------------------------------------------
# Report. Secret values are never printed.
# ---------------------------------------------------------------------------

sort -t "$TAB" -k1,1 -k2,2n -k3,3 "$FINDINGS" |
    awk -F '\t' '!seen[$0]++' >"$WORK/findings.sorted"

count=$(wc -l <"$WORK/findings.sorted" | tr -d ' ')

if [ "$count" -eq 0 ]; then
    if [ "$MODE" = "staged" ]; then
        echo "$PROG: clean (staged changes)"
    else
        echo "$PROG: clean ($nfiles file(s))"
    fi
    exit 0
fi

echo "$PROG: $count potential secret(s) found"
echo
awk -F '\t' '{
    printf "  %s:%s  [%s] %s\n", $1, $2, $3, $4
}' "$WORK/findings.sorted"
echo
echo "Remove the secret, rotate it, and load it from the environment or a"
echo "secret manager. For a deliberate fixture, append: secret-scan:allow"
exit 1
