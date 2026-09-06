#!/usr/bin/env sh
# Smoke-тест живого API (по умолчанию http://localhost:8080).
# Использование: ./scripts/smoke.sh [BASE_URL] [PAIR]
set -u

BASE="${1:-http://localhost:8080}"
PAIR="${2:-EUR/MXN}"
failures=0

check() {
    if [ "$2" = "1" ]; then
        echo "PASS  $1"
    else
        echo "FAIL  $1"
        failures=$((failures + 1))
    fi
}

code() {
    curl -s -o /dev/null -w "%{http_code}" "$@"
}

field() {
    sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"
}

# --- служебные эндпоинты ---
[ "$(code "$BASE/healthz")" = "200" ] && check "healthz" 1 || check "healthz" 0
curl -s "$BASE/version" | grep -q '"version":' && check "version" 1 || check "version" 0

# --- happy path: асинхронное обновление ---
RESP=$(curl -s -X POST "$BASE/quotes" -H "Content-Type: application/json" -d "{\"pair\":\"$PAIR\"}")
ID=$(printf '%s' "$RESP" | field id)
[ -n "$ID" ] && check "POST /quotes -> id" 1 || check "POST /quotes -> id" 0

STATUS=pending
i=0
while [ "$STATUS" = "pending" ] && [ "$i" -lt 15 ]; do
    sleep 2
    STATUS=$(curl -s "$BASE/quotes/requests/$ID" | field status)
    i=$((i + 1))
done
[ "$STATUS" = "completed" ] && check "update completed" 1 || check "update completed" 0

PRICE=$(curl -s "$BASE/quotes?pair=$PAIR")
printf '%s' "$PRICE" | grep -q '"price":[0-9]' && check "GET /quotes -> price" 1 || check "GET /quotes -> price" 0

# --- идемпотентность по ключу ---
KEY=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || echo "key-$RANDOM")
A=$(curl -s -X POST "$BASE/quotes" -H "Content-Type: application/json" -H "Idempotency-Key: $KEY" -d "{\"pair\":\"$PAIR\"}" | field id)
B=$(curl -s -X POST "$BASE/quotes" -H "Content-Type: application/json" -H "Idempotency-Key: $KEY" -d "{\"pair\":\"$PAIR\"}" | field id)
[ -n "$A" ] && [ "$A" = "$B" ] && check "same Idempotency-Key -> same id" 1 || check "same Idempotency-Key -> same id" 0

# --- негативные кейсы ---
[ "$(code "$BASE/quotes?pair=NOPE")" = "400" ] && check "invalid pair -> 400" 1 || check "invalid pair -> 400" 0
[ "$(code "$BASE/quotes/requests/not-a-uuid")" = "400" ] && check "invalid id -> 400" 1 || check "invalid id -> 400" 0
[ "$(code "$BASE/quotes/requests/00000000-0000-0000-0000-0000000000ff")" = "404" ] && check "unknown id -> 404" 1 || check "unknown id -> 404" 0
[ "$(code -X POST -H "Content-Type: application/json" -d '{"pair":"XXX/YYY"}' "$BASE/quotes")" = "422" ] && check "unsupported pair -> 422" 1 || check "unsupported pair -> 422" 0

# --- итог ---
echo
if [ "$failures" -gt 0 ]; then
    echo "SMOKE FAILED: $failures"
    exit 1
fi
echo "SMOKE OK"
