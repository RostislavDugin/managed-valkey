#!/usr/bin/env bash
set -euo pipefail

state_dir=${1:?задайте каталог состояния}
scenario=${2:?задайте ID сценария}
action=${3:?задайте действие}
target=${4:?задайте цель}

[[ -f "$state_dir/environment.env" ]] || {
    echo "fault-event: нет состояния $state_dir" >&2
    exit 1
}
set -a
source "$state_dir/environment.env"
set +a
FAULT_EVENTS_FILE=${FAULT_EVENTS_FILE:-$state_dir/fault-events.tsv}
[[ "$MV_SUITE" == operator && "$MV_STATE_DIR" == "$state_dir" ]] || {
    echo "fault-event: состояние не принадлежит стенду оператора" >&2
    exit 1
}
[[ "$FAULT_EVENTS_FILE" == "$state_dir/fault-events.tsv" ]] || {
    echo "fault-event: журнал не принадлежит стенду" >&2
    exit 1
}
[[ "$scenario" =~ ^(HA|FP|ND|NT|OP|RZ|PW|DL|CT)-[0-9]{2}$ ]] || {
    echo "fault-event: неверный ID сценария" >&2
    exit 1
}
[[ "$action" =~ ^[a-z][a-z0-9-]{0,63}$ ]] || {
    echo "fault-event: неверное действие" >&2
    exit 1
}
[[ "$target" =~ ^[A-Za-z0-9._:/=,+\>@-]{1,256}$ ]] || {
    echo "fault-event: неверная цель" >&2
    exit 1
}

touch "$FAULT_EVENTS_FILE"
chmod 0600 "$FAULT_EVENTS_FILE"
exec 9>>"$FAULT_EVENTS_FILE.lock"
flock 9
printf '%(%Y-%m-%dT%H:%M:%SZ)T\t%s\t%s\t%s\n' -1 "$scenario" "$action" "$target" \
    >>"$FAULT_EVENTS_FILE"
