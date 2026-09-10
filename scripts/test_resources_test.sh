#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
resources="$repo_root/scripts/test_resources.sh"
owner_pid=$$
owner_start=$(MV_TEST_RESOURCE_STATE_DIR="$temporary" "$resources" start-ticks "$owner_pid")

export MV_TEST_RESOURCE_STATE_DIR=$temporary
export MV_TEST_MEMORY_BUDGET_MIB=16
export MV_TEST_MIN_AVAILABLE_MIB=0

"$resources" reserve first operator 8 "$owner_pid" "$owner_start" 0
"$resources" reserve first operator 8 "$owner_pid" "$owner_start" 0
[[ $("$resources" status | awk -F '\t' '$1 == "reserved_mib" { print $2 }') == 8 ]]

if "$resources" reserve too-large operator 17 "$owner_pid" "$owner_start" 0 2>/dev/null; then
    echo "резерв больше бюджета был принят" >&2
    exit 1
fi

if "$resources" reserve second api 9 "$owner_pid" "$owner_start" 0 2>/dev/null; then
    echo "суммарный резерв больше бюджета был принят" >&2
    exit 1
fi

"$resources" reserve waiting api 9 "$owner_pid" "$owner_start" 5 &
waiting_pid=$!
sleep 1
kill -0 "$waiting_pid"
"$resources" release first
wait "$waiting_pid"
"$resources" release waiting

wrong_start=$((owner_start + 1))
printf 'reused-pid\toperator\t8\t%s\t%s\t%s\n' \
    "$owner_pid" "$wrong_start" "$(< /proc/sys/kernel/random/boot_id)" \
    >>"$temporary/reservations.tsv"

printf 'stale\toperator\t8\t99999999\t1\t%s\n' "$(< /proc/sys/kernel/random/boot_id)" \
    >>"$temporary/reservations.tsv"
"$resources" reap
! rg -q '^stale\t' "$temporary/reservations.tsv"

[[ $("$resources" status | awk -F '\t' '$1 == "reserved_mib" { print $2 }') == 0 ]]
