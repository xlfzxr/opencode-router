#!/data/data/com.termux/files/usr/bin/bash
# Probe actual model IDs, never the routing alias (which could hide failures).
set -euo pipefail
umask 077
for tool in curl jq; do
  command -v "$tool" >/dev/null || { echo "Install dependency: pkg install curl jq" >&2; exit 2; }
done
BASE="${OCR_BASE_URL:-http://127.0.0.1:3002}"
BASE="${BASE%/}"
ALIAS="${OCR_ALIAS:-ocr}"
TIMEOUT="${OCR_PROBE_TIMEOUT:-90}"
DELAY="${OCR_PROBE_DELAY:-2}"
OUT="${1:-$HOME/opencode-model-tests/$(date +%Y%m%d-%H%M%S)-$$}"
mkdir -p "$OUT"
# Do not clobber results of an earlier run.
if [[ -e "$OUT/results.jsonl" ]]; then
  echo "Results already exist: $OUT/results.jsonl; choose a new directory." >&2
  exit 2
fi
curl --silent --show-error --connect-timeout 10 --max-time "$TIMEOUT" --fail \
  "$BASE/v1/models" -o "$OUT/catalog.json"
jq -e '.data | type == "array"' "$OUT/catalog.json" >/dev/null
jq -r --arg alias "$ALIAS" '.data[] | .id | select(type == "string" and . != $alias)' \
  "$OUT/catalog.json" | sort -u > "$OUT/models.txt"
if [[ ! -s "$OUT/models.txt" ]]; then
  echo 'No actual model IDs found in bridge catalog.' >&2
  exit 2
fi
: > "$OUT/results.jsonl"
printf 'Testing model IDs via %s/v1/messages (non-streaming)\n' "$BASE"
printf 'Results: %s\n' "$OUT"
index=0
failed=0
while IFS= read -r model; do
  index=$((index + 1))
  body="$OUT/$index.response.json"
  request="$OUT/$index.request.json"
  jq -n --arg model "$model" '{model:$model,max_tokens:32,stream:false,messages:[{role:"user",content:"Reply with the single word OK."}]}' > "$request"
  rc=0
  metrics="$(curl --silent --show-error --connect-timeout 10 --max-time "$TIMEOUT" \
    -H 'Content-Type: application/json' --data-binary "@$request" \
    -o "$body" -w '%{http_code} %{time_total}' "$BASE/v1/messages" 2> "$OUT/$index.curl-error.txt")" || rc=$?
  http="${metrics%% *}"
  elapsed="${metrics#* }"
  result=error
  if [[ "$rc" == 28 ]]; then
    result=timeout
  elif [[ "$rc" != 0 ]]; then
    result=transport_error
  elif [[ "$http" == 200 ]]; then
    if jq -e '.type == "message" and (.content | type == "array")' "$body" >/dev/null 2>&1; then
      if jq -e '[.content[] | select(.type == "text") | .text | select(type == "string") | select(test("\\S"))] | length > 0' "$body" >/dev/null; then
        result=ok
      else
        result=empty
      fi
    else
      result=invalid_response
    fi
  fi
  if [[ "$result" != ok ]]; then failed=$((failed + 1)); fi
  message="$(jq -r '.error.message // .stop_reason // ""' "$body" 2>/dev/null || true)"
  jq -nc --arg model "$model" --arg result "$result" --arg http "$http" \
    --arg seconds "$elapsed" --arg message "$message" --arg file "$body" --argjson curlExit "$rc" \
    '{model:$model,result:$result,httpStatus:$http,seconds:$seconds,curlExit:$curlExit,message:$message,responseFile:$file}' >> "$OUT/results.jsonl"
  printf '%-40s %-18s HTTP %s  %ss\n' "$model" "$result" "$http" "$elapsed"
  sleep "$DELAY"
done < "$OUT/models.txt"
printf '\nTotal: %d | Responded: %d | Failed: %d\n' "$index" "$((index - failed))" "$failed"
printf 'Details: %s/results.jsonl\n' "$OUT"
echo 'A failed probe does not prove a model is permanently unavailable; inspect errors for rate limits, Tor, or provider failures.'
[[ "$failed" == 0 ]]
