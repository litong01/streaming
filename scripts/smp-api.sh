#!/usr/bin/env bash
set -euo pipefail

# Start, stop, and inspect the SMP 351's RTMP streams over its own web API,
# which is what the control server does.
#
#   ./scripts/smp-api.sh status
#   ./scripts/smp-api.sh start mandarin
#   ./scripts/smp-api.sh start english
#   ./scripts/smp-api.sh stop
#   ./scripts/smp-api.sh get /encoder/status
#
# Archive (channel 1) carries Mandarin and Confidence (channel 3) carries
# English. Where each one streams to is configured on the unit itself, so
# nothing here recalls or edits a preset.
#
# A whole RTMP resource includes its destination URL and stream key, so
# `get /streamer/rtmp/1` prints something that should not be pasted anywhere.
# The commands below read single values instead, or filter the key out.

SMP_HOST="${SMP_HOST:-192.168.252.41}"
SMP_PORT="${SMP_PORT:-443}"
SMP_USER="${SMP_USER:-admin}"

ARCHIVE=1
CONFIDENCE=3

SCHEME="https"
if [[ "$SMP_PORT" == "80" ]]; then
  SCHEME="http"
fi

# The credentials go into a file that curl reads, so the password stays out of
# the process list. SMP_PASSWORD_FILE is preferred because it keeps the
# password out of shell history as well.
curl_config=$(mktemp)
trap 'rm -f "$curl_config"' EXIT

read_password() {
  if [[ -n "${SMP_PASSWORD_FILE:-}" ]]; then
    tr -d '\r\n' < "$SMP_PASSWORD_FILE"
  elif [[ -n "${SMP_PASSWORD:-}" ]]; then
    printf '%s' "$SMP_PASSWORD"
  else
    local entered
    read -rsp "Password for $SMP_USER@$SMP_HOST: " entered < /dev/tty
    echo >&2
    printf '%s' "$entered"
  fi
}

printf 'user = "%s:%s"\n' "$SMP_USER" "$(read_password)" > "$curl_config"

# --insecure because the unit presents its own self-signed certificate.
api() {
  local method="$1" path="$2"
  shift 2
  curl -sS --insecure --max-time 15 --config "$curl_config" \
    --request "$method" "$SCHEME://$SMP_HOST:$SMP_PORT$path" "$@"
}

# One GET can ask for several resources at once, which is how the server polls.
resources() {
  local query="" uri
  for uri in "$@"; do
    query+="&uri=$uri"
  done
  api GET "/api/swis/resources?${query:1}"
}

write_resource() {
  api PUT /api/swis/resources \
    --header "Content-Type: application/json" \
    --data "[{\"uri\":\"$1\",\"value\":$2}]"
}

publishing() {
  resources "/streamer/rtmp/$1/pub_control" | grep -o '"result":[0-9]*' | cut -d: -f2
}

channel_name() {
  if [[ "$1" == "$CONFIDENCE" ]]; then
    echo "Confidence"
  else
    echo "Archive"
  fi
}

word() {
  if [[ "$1" == "1" ]]; then
    echo "$2"
  else
    echo "$3"
  fi
}

# The full RTMP resource carries the stream key, so only the address the unit
# resolved is taken out of it.
destination() {
  local found=""
  found=$(resources "/streamer/rtmp/$1" \
    | grep -o '"resolved_ip":"[^"]*","resolved_port":[0-9]*' \
    | grep -v '"0\.0\.0\.0"' \
    | head -1 \
    | sed 's/.*"resolved_ip":"\([^"]*\)","resolved_port":\([0-9]*\)/\1:\2/') || true
  echo "${found:-no address yet}"
}

report() {
  local label="$1" enabled="$2" live="$3" channel="$4"
  local line
  line="$label: encoder $(word "$enabled" running off), stream $(word "$live" live stopped)"
  if [[ "$live" == "1" ]]; then
    line+=" to $(destination "$channel")"
  fi
  echo "$line"
}

status() {
  local values=() value
  while IFS= read -r value; do
    values+=("$value")
  done < <(
    resources \
      "/encoder/$ARCHIVE/stream_enable" "/streamer/rtmp/$ARCHIVE/pub_control" \
      "/encoder/$CONFIDENCE/stream_enable" "/streamer/rtmp/$CONFIDENCE/pub_control" \
      | grep -o '"result":[0-9]*' | cut -d: -f2
  )
  if [[ "${#values[@]}" -ne 4 ]]; then
    echo "$SMP_HOST did not answer all four values. Raw reply:" >&2
    resources "/streamer/rtmp/$ARCHIVE/pub_control" >&2
    echo >&2
    exit 1
  fi
  report "Archive (Mandarin)" "${values[0]}" "${values[1]}" "$ARCHIVE"
  report "Confidence (English)" "${values[2]}" "${values[3]}" "$CONFIDENCE"
}

# The unit refuses a publish on an encoder that is not running, answering E13,
# so the encoder is switched on first. Only one language goes out at a time, so
# the other is stopped, and only when it is actually streaming.
start() {
  local wanted other
  case "${1:-}" in
    mandarin) wanted="$ARCHIVE" other="$CONFIDENCE" ;;
    english) wanted="$CONFIDENCE" other="$ARCHIVE" ;;
    *)
      echo "usage: $0 start {mandarin|english}" >&2
      exit 1
      ;;
  esac
  if [[ "$(publishing "$other")" == "1" ]]; then
    echo "Stopping the $(channel_name "$other") stream first."
    write_resource "/streamer/rtmp/$other/pub_control" 0 > /dev/null
  fi
  write_resource "/encoder/$wanted/stream_enable" 1 > /dev/null
  write_resource "/streamer/rtmp/$wanted/pub_control" 1 > /dev/null
  # Long enough for the unit to resolve its destination and connect.
  sleep 3
  status
}

stop() {
  local channel
  for channel in "$ARCHIVE" "$CONFIDENCE"; do
    if [[ "$(publishing "$channel")" == "1" ]]; then
      echo "Stopping the $(channel_name "$channel") stream."
      write_resource "/streamer/rtmp/$channel/pub_control" 0 > /dev/null
    fi
  done
  status
}

case "${1:-}" in
  status) status ;;
  start)
    shift
    start "${1:-}"
    ;;
  stop) stop ;;
  get)
    shift
    if [[ $# -eq 0 ]]; then
      echo "usage: $0 get <uri> [uri...]" >&2
      exit 1
    fi
    resources "$@"
    echo
    ;;
  *)
    echo "usage: $0 {status|start <mandarin|english>|stop|get <uri>...}" >&2
    exit 1
    ;;
esac
