#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

fake_bin=$work_dir/bin
state_dir=$work_dir/state
admin_kubeconfig=$work_dir/admin.kubeconfig
api_kubeconfig=$work_dir/api.kubeconfig
mkdir -p "$fake_bin" "$state_dir/active"
printf '%s\n' 'apiVersion: v1' >"$admin_kubeconfig"

cat >"$fake_bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"$TEST_KUBECTL_LOG"

if [[ $* == 'get --raw=/readyz' ]]; then
    exit 0
fi
if [[ ${1:-} == get && ${2:-} == statefulsets.apps ]]; then
    if [[ $TEST_KUBECTL_MODE == legacy-statefulset ]]; then
        printf '%s\n' tenant-a/valkey-a
    fi
    exit 0
fi
if [[ ${1:-} == get && ${2:-} == nodes ]]; then
    if [[ $* == *InternalIP* ]]; then
        printf '%s\n' 10.17.0.26 10.17.0.27
    else
        printf '%s\n' node-a node-b
    fi
    exit 0
fi
if [[ $* == '-n kube-system get pods -l k8s-app=cilium --field-selector=status.phase=Running -o name' ]]; then
    printf '%s\n' pod/cilium-a pod/cilium-b
    exit 0
fi
if [[ ${1:-} == -n && ${2:-} == kube-system && ${3:-} == exec ]]; then
    if [[ $TEST_KUBECTL_MODE == broken-node-network && ${4:-} == pod/cilium-a ]]; then
        printf '%s\n' 'Cluster health: 1/2 reachable'
    else
        printf '%s\n' 'Cluster health: 2/2 reachable'
    fi
    exit 0
fi
if [[ ${1:-} == -n && ${3:-} == delete && ${4:-} == pod ]]; then
    rm -f -- "$TEST_KUBECTL_STATE/active/${5:-}"
    exit 0
fi
if [[ ${1:-} == apply && ${2:-} == -f && ${3:-} == - ]]; then
    payload=$TEST_KUBECTL_STATE/payload.$$
    cat >"$payload"
    pod=$(awk '$1 == "name:" { print $2; exit }' "$payload")
    cp "$payload" "$TEST_KUBECTL_STATE/active/$pod"
    printf '%s\n' '---' >>"$TEST_KUBECTL_YAML_LOG"
    cat "$payload" >>"$TEST_KUBECTL_YAML_LOG"
    rm -f -- "$payload"
    exit 0
fi
if [[ ${1:-} == -n && ${3:-} == get && ${4:-} == pod ]]; then
    pod=${5:-}
    expected_exit_code=${pod##*-}
    actual_exit_code=$expected_exit_code
    if [[ $TEST_KUBECTL_MODE == wrong-exit && $pod == *-1-42 ]]; then
        actual_exit_code=17
    fi
    query=${*: -1}
    case "$query" in
    *restartCount*)
        if [[ $TEST_KUBECTL_MODE == restarted && $pod == *-1-42 ]]; then
            printf 1
        else
            printf 0
        fi
        ;;
    *status.phase*)
        if [[ $expected_exit_code == 0 ]]; then
            printf Succeeded
        else
            printf Failed
        fi
        ;;
    *terminated.exitCode*) printf '%s' "$actual_exit_code" ;;
    *terminated.reason*)
        if [[ $expected_exit_code == 0 ]]; then
            printf Completed
        else
            printf Error
        fi
        ;;
    *) exit 2 ;;
    esac
    exit 0
fi
if [[ ${1:-} == apply && ${2:-} == -f && ${3:-} == */deploy/prod/namespace.yaml ]]; then
    exit 73
fi

echo "неожиданный вызов kubectl: $*" >&2
exit 2
EOF

cat >"$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == manifest && ${2:-} == inspect ]]
EOF

cat >"$fake_bin/helm" <<'EOF'
#!/usr/bin/env bash
exit 2
EOF

cat >"$fake_bin/getent" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == ahostsv4 && ${2:-} == cert-manager-webhook.valkey.h3llo-demo.com ]]
printf '%s\n' '10.17.0.26 STREAM webhook' '10.17.0.27 STREAM webhook'
if [[ $TEST_KUBECTL_MODE == stale-webhook-dns ]]; then
    printf '%s\n' '10.17.0.28 STREAM webhook'
fi
EOF

cat >"$fake_bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

chmod 0755 \
    "$fake_bin/kubectl" "$fake_bin/docker" "$fake_bin/getent" "$fake_bin/helm" "$fake_bin/sleep"

run_case() {
    local mode=$1
    local output=$2

    rm -rf -- "$state_dir/active"
    mkdir -p "$state_dir/active"
    : >"$state_dir/kubectl.log"
    : >"$state_dir/pods.yaml"

    set +e
    PATH="$fake_bin:$PATH" \
        TEST_KUBECTL_MODE="$mode" \
        TEST_KUBECTL_LOG="$state_dir/kubectl.log" \
        TEST_KUBECTL_STATE="$state_dir" \
        TEST_KUBECTL_YAML_LOG="$state_dir/pods.yaml" \
        CLOUDFLARE_API_TOKEN=test-token \
        "$repo_root/scripts/deploy_kubernetes.sh" \
        aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
        "$admin_kubeconfig" \
        "$api_kubeconfig" >"$output" 2>&1
    case_status=$?
    set -e
}

run_case success "$work_dir/success.log"
[[ $case_status == 73 ]]
grep -Fq "apply -f $repo_root/deploy/prod/namespace.yaml" "$state_dir/kubectl.log"
[[ $(grep -c '^  restartPolicy: Never$' "$state_dir/pods.yaml") == 4 ]]
[[ $(grep -c 'command: \["/bin/sh", "-c", "exit 0"\]' "$state_dir/pods.yaml") == 2 ]]
[[ $(grep -c 'command: \["/bin/sh", "-c", "exit 42"\]' "$state_dir/pods.yaml") == 2 ]]
[[ $(grep -c '^  nodeName: node-a$' "$state_dir/pods.yaml") == 2 ]]
[[ $(grep -c '^  nodeName: node-b$' "$state_dir/pods.yaml") == 2 ]]
if grep -Eq '^      restartPolicy:' "$state_dir/pods.yaml"; then
    echo "политика перезапуска задана контейнеру" >&2
    exit 1
fi
[[ -z $(find "$state_dir/active" -type f -print -quit) ]]

run_case legacy-statefulset "$work_dir/legacy.log"
[[ $case_status == 1 ]]
grep -Fq 'обнаружены пользовательские StatefulSet прежнего оператора' "$work_dir/legacy.log"
grep -Fq 'tenant-a/valkey-a' "$work_dir/legacy.log"
if grep -Fq 'get nodes' "$state_dir/kubectl.log"; then
    echo "проверка продолжилась при наличии прежнего StatefulSet" >&2
    exit 1
fi

run_case stale-webhook-dns "$work_dir/stale-webhook-dns.log"
[[ $case_status == 1 ]]
grep -Fq 'должен содержать внутренние адреса всех рабочих нод' \
    "$work_dir/stale-webhook-dns.log"
if grep -Fq 'managed-valkey-restart-check' "$state_dir/kubectl.log"; then
    echo "проверка продолжилась при неверном DNS cert-manager-webhook" >&2
    exit 1
fi

run_case broken-node-network "$work_dir/broken-node-network.log"
[[ $case_status == 1 ]]
grep -Fq 'межнодовая сеть Cilium недоступна' "$work_dir/broken-node-network.log"
if grep -Fq 'managed-valkey-restart-check' "$state_dir/kubectl.log"; then
    echo "проверка продолжилась при недоступной межнодовой сети" >&2
    exit 1
fi

run_case wrong-exit "$work_dir/wrong-exit.log"
[[ $case_status == 1 ]]
grep -Fq 'завершился с неожиданным состоянием' "$work_dir/wrong-exit.log"
[[ -z $(find "$state_dir/active" -type f -print -quit) ]]
if grep -Fq "apply -f $repo_root/deploy/prod/namespace.yaml" "$state_dir/kubectl.log"; then
    echo "подготовка кластера продолжилась после неверного кода завершения" >&2
    exit 1
fi

run_case restarted "$work_dir/restarted.log"
[[ $case_status == 1 ]]
grep -Fq 'перезапустил контейнер' "$work_dir/restarted.log"
[[ -z $(find "$state_dir/active" -type f -print -quit) ]]

if rg -n 'ContainerRestartRules|restartPolicy: Always' "$repo_root/scripts/deploy_kubernetes.sh"; then
    echo "в сценарии осталась зависимость от ContainerRestartRules" >&2
    exit 1
fi
if ! rg -Fq 'valkeyinstances.valkey.h3llo-demo.com --subresource=status' \
    "$repo_root/scripts/deploy_kubernetes.sh"; then
    echo "проверка прав API не указывает status как дочерний ресурс" >&2
    exit 1
fi
if ! rg -q '^podDnsPolicy: Default$' "$repo_root/deploy/prod/cert-manager-values.yaml"; then
    echo "контроллер cert-manager не использует DNS рабочей ноды" >&2
    exit 1
fi

echo "проверка политики перезапуска Pod прошла"
