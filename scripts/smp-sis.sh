#!/usr/bin/env bash
set -euo pipefail

# Send Extron SIS commands to the SMP 351 over SSH.
#
#   ./scripts/smp-sis.sh status
#   ./scripts/smp-sis.sh names
#   ./scripts/smp-sis.sh recall 1        # onto Archive
#   ./scripts/smp-sis.sh recall 12 3     # onto Confidence
#   ./scripts/smp-sis.sh rollback
#
# Encoder 1 is the Archive channel, encoder 3 is Confidence. A streaming preset
# holds a separate half for each, so recalling one means two commands.

SMP_HOST="${SMP_HOST:-192.168.252.41}"
SMP_PORT="${SMP_PORT:-22023}"
SMP_USER="${SMP_USER:-admin}"

# Preset 10 holds the original Archive push, 12 the original Confidence push.
# Preset 11 is a duplicate of 10 rather than the Confidence backup it was meant
# to be, so it is deliberately not used here.
ARCHIVE_SNAPSHOT="${ARCHIVE_SNAPSHOT:-10}"
CONFIDENCE_SNAPSHOT="${CONFIDENCE_SNAPSHOT:-12}"

# Built as literal bytes rather than printf escapes, because "\0331" is read as
# one octal byte instead of ESC followed by a digit.
ESC=$(printf '\033')
CR=$(printf '\r')

# The SMP needs a pause between commands or it answers with E22 (busy), and it
# prints a two-line banner before every reply when verbose mode is off.
send() {
  local delay="${SMP_DELAY:-2}"
  {
    for command in "$@"; do
      printf '%s' "$command"
      sleep "$delay"
    done
  } | ssh -T -o StrictHostKeyChecking=no -p "$SMP_PORT" "$SMP_USER@$SMP_HOST" | cat -v
}

status() {
  echo "Archive stream, Confidence stream, Archive preset, Confidence preset, Confidence stream name:"
  send "${ESC}1STRC${CR}" "${ESC}3STRC${CR}" '46I' '48I' "${ESC}N3STRC${CR}"
}

names() {
  local slots=("${@:-1 2 10 11 12}")
  echo "Names for streaming presets ${slots[*]}:"
  local commands=()
  for slot in ${slots[*]}; do
    commands+=("${ESC}3*${slot}PNAM${CR}")
  done
  send "${commands[@]}"
}

# A slot holds one encoder's settings, not one per encoder, so a recall targets
# a single encoder. Archive is the default because Confidence serves a fixed
# RTSP preview that no preset should overwrite. The encoder is stopped first
# because the SMP refuses to rewrite a destination while that encoder is live.
recall() {
  local preset="$1" encoder="${2:-1}"
  local query='46I'
  if [[ "$encoder" == 3 ]]; then
    query='48I'
  fi
  echo "Stopping encoder $encoder, recalling preset $preset onto it, then reading back:"
  send "${ESC}${encoder}*0STRC${CR}" "3*${encoder}*${preset}." "$query"
}

rollback() {
  echo "Restoring Archive from preset $ARCHIVE_SNAPSHOT:"
  send "${ESC}1*0STRC${CR}" "3*1*$ARCHIVE_SNAPSHOT." '46I'
  echo "Restoring Confidence from preset $CONFIDENCE_SNAPSHOT:"
  send "${ESC}3*0STRC${CR}" "3*3*$CONFIDENCE_SNAPSHOT." '48I'
}

case "${1:-}" in
  status) status ;;
  names) shift; names "$@" ;;
  recall)
    if [[ ! "${2:-}" =~ ^[0-9]+$ || ! "${3:-1}" =~ ^[13]$ ]]; then
      echo "usage: $0 recall <preset number> [encoder: 1=Archive, 3=Confidence]" >&2
      exit 1
    fi
    recall "$2" "${3:-1}"
    ;;
  rollback) rollback ;;
  *)
    echo "usage: $0 {status|names [preset...]|recall <preset> [encoder]|rollback}" >&2
    exit 1
    ;;
esac
