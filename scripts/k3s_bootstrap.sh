#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)

if [[ -f "$repo_root/.env" ]]; then
    set -a
    source "$repo_root/.env"
    set +a
fi

for command in kubectl helm mkcert; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "bootstrap: команда $command не установлена" >&2
        exit 1
    fi
done

ADMIN_KUBECONFIG="${ADMIN_KUBECONFIG:-$repo_root/tmp/k3s/admin.kubeconfig}"
API_KUBECONFIG="${API_KUBECONFIG:-$repo_root/tmp/k3s/api.kubeconfig}"
OPERATOR_KUBECONFIG="${OPERATOR_KUBECONFIG:-$repo_root/tmp/k3s/operator.kubeconfig}"
KUBERNETES_SERVER="${KUBERNETES_SERVER:-https://127.0.0.1:6443}"
VALKEY_BASE_DOMAIN="${VALKEY_BASE_DOMAIN:-valkey.localhost}"
VALKEY_PUBLIC_PORT="${VALKEY_PUBLIC_PORT:-41379}"
ENVOY_NODE_PORT="${ENVOY_NODE_PORT:-31379}"

GATEWAY_API_VERSION="v1.5.1"
ENVOY_GATEWAY_VERSION="v1.8.4"

export KUBECONFIG="$ADMIN_KUBECONFIG"

log() {
    echo "bootstrap: $*"
}

retry() {
    local attempt

    for attempt in {1..5}; do
        if "$@"; then
            return 0
        fi

        if ((attempt < 5)); then
            log "попытка $attempt не удалась, повтор через 10 с"
            sleep 10
        fi
    done

    return 1
}

wait_for_api() {
    local attempt

    log "ожидание Kubernetes API"

    for attempt in {1..60}; do
        if kubectl version >/dev/null 2>&1; then
            return 0
        fi

        sleep 5
    done

    echo "bootstrap: Kubernetes API не ответил" >&2
    return 1
}

remove_stale_nodes() {
    local node

    while IFS= read -r node; do
        case "$node" in
        node/k3s-server|node/k3s-agent-1|node/k3s-agent-2) ;;
        *) kubectl delete "$node" --wait=false ;;
        esac
    done < <(kubectl get nodes -o name)
}

reset_not_ready_agents() {
    local node ready

    for node in k3s-agent-1 k3s-agent-2; do
        ready=$(kubectl get node "$node" \
            -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' \
            2>/dev/null || true)

        if [[ -n "$ready" && "$ready" != "True" ]]; then
            kubectl delete node "$node" --wait=false
        fi
    done
}

wait_for_nodes() {
    local attempt

    for attempt in {1..60}; do
        if kubectl get node/k3s-server node/k3s-agent-1 node/k3s-agent-2 >/dev/null 2>&1; then
            kubectl wait --for=condition=Ready \
                node/k3s-server node/k3s-agent-1 node/k3s-agent-2 --timeout=300s
            return 0
        fi

        sleep 2
    done

    echo "bootstrap: ноды k3s не зарегистрировались" >&2
    return 1
}

install_gateway_api() {
    retry kubectl apply --server-side --force-conflicts -f \
        "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/experimental-install.yaml"
}

repair_envoy_release() {
    local revision status

    status=$(helm status envoy-gateway -n envoy-gateway-system 2>/dev/null |
        awk '/^STATUS:/ { print $2 }' || true)

    case "$status" in
    pending-install)
        helm uninstall envoy-gateway -n envoy-gateway-system
        ;;
    pending-upgrade|pending-rollback)
        revision=$(helm history envoy-gateway -n envoy-gateway-system |
            awk '$3 == "deployed" { revision = $1 } END { print revision }')

        if [[ -z "$revision" ]]; then
            echo "bootstrap: у зависшего релиза Envoy Gateway нет готовой ревизии" >&2
            return 1
        fi

        helm rollback envoy-gateway "$revision" -n envoy-gateway-system --wait --timeout 10m
        ;;
    esac
}

install_envoy_gateway() {
    repair_envoy_release
    retry helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
        --version "$ENVOY_GATEWAY_VERSION" \
        --namespace envoy-gateway-system \
        --create-namespace \
        --wait --timeout 10m
}

install_managed_valkey() {
    kubectl apply --server-side --force-conflicts -k "$repo_root/operator/config/crd"
    kubectl apply -f "$repo_root/deploy/dev/infra/namespace.yaml"
    kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/valkey-system --timeout=60s
    kubectl apply -f "$repo_root/deploy/dev/infra"
    kubectl apply -f "$repo_root/operator/config/rbac"
    kubectl apply -f "$repo_root/deploy/dev/rbac"
}

remove_legacy_operator() {
    if kubectl -n valkey-system get statefulset operator >/dev/null 2>&1; then
        kubectl -n valkey-system delete statefulset operator --wait=true
        kubectl -n valkey-system delete lease managed-valkey-operator --ignore-not-found
    fi
}

install_dev_certificate() (
    local certificate_dir

    if kubectl -n valkey-system get secret valkey-wildcard-tls >/dev/null 2>&1; then
        return 0
    fi

    mkcert -install
    certificate_dir=$(mktemp -d)
    trap 'rm -rf "$certificate_dir"' EXIT

    mkcert \
        -cert-file "$certificate_dir/tls.crt" \
        -key-file "$certificate_dir/tls.key" \
        "*.$VALKEY_BASE_DOMAIN" "$VALKEY_BASE_DOMAIN"

    kubectl -n valkey-system create secret tls valkey-wildcard-tls \
        --cert="$certificate_dir/tls.crt" \
        --key="$certificate_dir/tls.key"
)

pin_envoy_node_port() {
    local attempt index service

    for attempt in {1..60}; do
        service=$(kubectl -n envoy-gateway-system get svc \
            -l gateway.envoyproxy.io/owning-gateway-name=valkey \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        [[ -n "$service" ]] && break
        sleep 5
    done

    if [[ -z "${service:-}" ]]; then
        echo "bootstrap: Service шлюза не появился" >&2
        return 1
    fi

    index=$(kubectl -n envoy-gateway-system get svc "$service" \
        -o jsonpath="{range .spec.ports[*]}{.port}{'\n'}{end}" |
        grep -n "^${VALKEY_PUBLIC_PORT}$" |
        cut -d: -f1 || true)

    if [[ -z "$index" ]]; then
        echo "bootstrap: у Service $service нет порта $VALKEY_PUBLIC_PORT" >&2
        return 1
    fi

    kubectl -n envoy-gateway-system patch svc "$service" --type=json \
        -p "[{\"op\":\"replace\",\"path\":\"/spec/ports/$((index - 1))/nodePort\",\"value\":${ENVOY_NODE_PORT}}]"
}

write_kubeconfig() {
    local account=$1
    local output=$2
    local authority token

    for _ in {1..60}; do
        token=$(kubectl -n valkey-system get secret "${account}-token" \
            -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)
        [[ -n "$token" ]] && break
        sleep 2
    done

    if [[ -z "${token:-}" ]]; then
        echo "bootstrap: token учётной записи $account не выдан" >&2
        return 1
    fi

    authority=$(kubectl config view --raw \
        -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')

    umask 077
    cat >"$output" <<YAML
apiVersion: v1
kind: Config
clusters:
  - name: managed-valkey-dev
    cluster:
      server: ${KUBERNETES_SERVER}
      certificate-authority-data: ${authority}
users:
  - name: ${account}
    user:
      token: ${token}
contexts:
  - name: managed-valkey-dev
    context:
      cluster: managed-valkey-dev
      user: ${account}
      namespace: valkey-system
current-context: managed-valkey-dev
YAML
    chmod 0600 "$output"
}

mkdir -p "$repo_root/tmp/k3s"
"$repo_root/scripts/k3s_host_kubeconfig.sh" "$ADMIN_KUBECONFIG" >/dev/null

wait_for_api
remove_stale_nodes
reset_not_ready_agents
wait_for_nodes
install_gateway_api
install_envoy_gateway
install_managed_valkey
remove_legacy_operator
install_dev_certificate
pin_envoy_node_port
write_kubeconfig managed-valkey-api "$API_KUBECONFIG"
write_kubeconfig managed-valkey-operator "$OPERATOR_KUBECONFIG"

log "готово"
