#!/usr/bin/env bash
set -euo pipefail

trap 'exit 130' INT
trap 'exit 143' TERM

repo_root=$(git rev-parse --show-toplevel)
project=${MANAGED_VALKEY_COMPOSE_PROJECT:-managed-valkey-dev}

if [[ "$project" == managed-valkey-dev ]]; then
    source "$repo_root/scripts/local_env.sh"
    load_local_env "$repo_root"
fi

state_dir=${MV_STATE_DIR:-$repo_root/tmp/k3s/dev}
ADMIN_KUBECONFIG=${ADMIN_KUBECONFIG:-$state_dir/admin.kubeconfig}
API_KUBECONFIG=${API_KUBECONFIG:-$state_dir/api.kubeconfig}
OPERATOR_KUBECONFIG=${OPERATOR_KUBECONFIG:-$state_dir/operator.kubeconfig}
OPERATOR_ENV=${OPERATOR_ENV:-$state_dir/operator.env}
ROUTES_FILE=${ROUTES_FILE:-$state_dir/routes.tsv}
KUBERNETES_SERVER=${KUBERNETES_SERVER:-https://127.0.0.1:6443}
VALKEY_BASE_DOMAIN=${VALKEY_BASE_DOMAIN:-valkey.localhost}
VALKEY_LISTENER_PORT=${VALKEY_LISTENER_PORT:-41379}
ENVOY_NODE_PORT=${ENVOY_NODE_PORT:-31379}
BOOTSTRAP_PROFILE=${BOOTSTRAP_PROFILE:-full}
K3S_EXPECTED_NODES=${K3S_EXPECTED_NODES:-3}
MANAGED_VALKEY_ENVOY_REPLICAS=${MANAGED_VALKEY_ENVOY_REPLICAS:-2}
MANAGED_VALKEY_ENVOY_EXTERNAL_TRAFFIC_POLICY=${MANAGED_VALKEY_ENVOY_EXTERNAL_TRAFFIC_POLICY:-Local}
MANAGED_VALKEY_DOCKER_NETWORK=${MANAGED_VALKEY_DOCKER_NETWORK:-${project}_default}
MANAGED_VALKEY_CA_FILE=${MANAGED_VALKEY_CA_FILE:-$state_dir/ca.crt}

GATEWAY_API_VERSION=v1.5.1
ENVOY_GATEWAY_VERSION=v1.8.4

export KUBECONFIG=$ADMIN_KUBECONFIG

log() {
    echo "bootstrap: $*"
}

require_commands() {
    local command

    for command in "$@"; do
        if ! command -v "$command" >/dev/null 2>&1; then
            echo "bootstrap: команда $command не установлена" >&2
            exit 1
        fi
    done
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
        if kubectl get --raw=/readyz >/dev/null 2>&1; then
            return 0
        fi
        sleep 5
    done
    echo "bootstrap: Kubernetes API не ответил" >&2
    return 1
}

expected_nodes() {
    local index

    printf '%s\n' k3s-server
    for ((index = 1; index < K3S_EXPECTED_NODES; index++)); do
        printf 'k3s-agent-%d\n' "$index"
    done
}

remove_stale_nodes() {
    local node name keep expected

    while IFS= read -r node; do
        name=${node#node/}
        keep=0
        while IFS= read -r expected; do
            [[ "$name" != "$expected" ]] || keep=1
        done < <(expected_nodes)
        ((keep == 1)) || kubectl delete "$node" --wait=false
    done < <(kubectl get nodes -o name)
}

repair_rejected_agents() {
    local container node running

    ((K3S_EXPECTED_NODES >= 2)) || return 0
    for node in $(expected_nodes | sed '1d'); do
        container=$(docker ps -aq \
            --filter "label=com.docker.compose.project=$project" \
            --filter "label=com.docker.compose.service=$node" | head -n 1)
        [[ -n "$container" ]] || continue
        running=$(docker inspect "$container" --format '{{.State.Running}}')
        if [[ "$running" == false ]] &&
            docker logs --since 10m "$container" 2>&1 | grep -q 'Node password rejected'; then
            kubectl delete node "$node" --ignore-not-found
            kubectl -n kube-system delete secret "$node.node-password.k3s" --ignore-not-found
            docker restart "$container" >/dev/null
        fi
    done
}

wait_for_nodes() {
    local attempt count

    for attempt in {1..90}; do
        count=$(kubectl get nodes -o name 2>/dev/null | wc -l)
        if ((count == K3S_EXPECTED_NODES)); then
            kubectl wait --for=condition=Ready node --all --timeout=300s
            return 0
        fi
        sleep 2
    done
    echo "bootstrap: ожидалось нод: $K3S_EXPECTED_NODES" >&2
    return 1
}

configure_pod_routes() (
    local attempt count current node_ip pod_cidr
    local route_plan

    route_plan=$(mktemp)
    trap 'rm -f "$route_plan"' EXIT

    for attempt in {1..60}; do
        kubectl get nodes -o jsonpath='{range .items[*]}{.spec.podCIDR}{"\t"}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{end}{"\n"}{end}' \
            >"$route_plan"
        count=0
        while IFS=$'\t' read -r pod_cidr node_ip; do
            [[ -n "$pod_cidr" && -n "$node_ip" ]] || continue
            ((count += 1))
        done <"$route_plan"
        ((count == K3S_EXPECTED_NODES)) && break
        sleep 2
    done
    if ((count != K3S_EXPECTED_NODES)); then
        echo "bootstrap: у Node отсутствует Pod CIDR или InternalIP" >&2
        return 1
    fi

    while IFS=$'\t' read -r pod_cidr node_ip; do
        current=$(ip route show exact "$pod_cidr" 2>/dev/null || true)
        if [[ -n "$current" && "$current" != *"via $node_ip"* ]]; then
            echo "bootstrap: маршрут $pod_cidr уже принадлежит другому gateway" >&2
            return 1
        fi
    done <"$route_plan"

    umask 077
    : >"$ROUTES_FILE"
    while IFS=$'\t' read -r pod_cidr node_ip; do
        sudo ip route replace "$pod_cidr" via "$node_ip"
        printf '%s\t%s\n' "$pod_cidr" "$node_ip" >>"$ROUTES_FILE"
    done <"$route_plan"
)

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
    pending-upgrade | pending-rollback)
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

install_rbac() {
    kubectl apply -f "$repo_root/operator/config/rbac"
    kubectl apply -f "$repo_root/deploy/dev/rbac"
}

install_managed_valkey() {
    local envoy_patch

    kubectl apply --server-side --force-conflicts -k "$repo_root/operator/config/crd"
    kubectl apply -f "$repo_root/deploy/dev/infra/namespace.yaml"
    kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/valkey-system --timeout=60s

    if [[ "$BOOTSTRAP_PROFILE" == full ]]; then
        kubectl apply -f "$repo_root/deploy/dev/infra"
        envoy_patch="{\"spec\":{\"provider\":{\"kubernetes\":{\"envoyService\":{\"externalTrafficPolicy\":\"${MANAGED_VALKEY_ENVOY_EXTERNAL_TRAFFIC_POLICY}\"},\"envoyDeployment\":{\"replicas\":${MANAGED_VALKEY_ENVOY_REPLICAS}}}}}}"
        if ((MANAGED_VALKEY_ENVOY_REPLICAS == 1)); then
            envoy_patch="{\"spec\":{\"provider\":{\"kubernetes\":{\"envoyService\":{\"externalTrafficPolicy\":\"${MANAGED_VALKEY_ENVOY_EXTERNAL_TRAFFIC_POLICY}\"},\"envoyDeployment\":{\"replicas\":1,\"pod\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"k3s-server\"}}}}}}}"
        fi
        kubectl -n valkey-system patch envoyproxy valkey-dev --type=merge \
            -p "$envoy_patch"
    else
        kubectl create namespace envoy-gateway-system --dry-run=client -o yaml | kubectl apply -f -
    fi
    install_rbac
}

remove_legacy_operator() {
    if kubectl -n valkey-system get statefulset operator >/dev/null 2>&1; then
        kubectl -n valkey-system delete statefulset operator --wait=true
        kubectl -n valkey-system delete lease managed-valkey-operator --ignore-not-found
    fi
}

install_dev_certificate() (
    local ca_file certificate_dir

    mkcert -install
    ca_file="$(mkcert -CAROOT)/rootCA.pem"
    certificate_dir=$(mktemp -d)
    trap 'rm -rf "$certificate_dir"' EXIT

    if kubectl -n valkey-system get secret valkey-wildcard-tls \
        -o go-template='{{index .data "tls.crt" | base64decode}}' \
        >"$certificate_dir/current.crt" 2>/dev/null &&
        openssl verify -CAfile "$ca_file" -verify_hostname "$VALKEY_BASE_DOMAIN" \
            "$certificate_dir/current.crt" >/dev/null 2>&1; then
        return 0
    fi

    mkcert -cert-file "$certificate_dir/tls.crt" -key-file "$certificate_dir/tls.key" \
        "*.$VALKEY_BASE_DOMAIN" "$VALKEY_BASE_DOMAIN"
    kubectl -n valkey-system create secret tls valkey-wildcard-tls \
        --cert="$certificate_dir/tls.crt" --key="$certificate_dir/tls.key" \
        --dry-run=client -o yaml | kubectl apply -f -
)

install_test_certificate() (
    local certificate_dir=$state_dir/tls

    mkdir -m 0700 -p "$certificate_dir"
    openssl genrsa -out "$certificate_dir/ca.key" 2048 >/dev/null 2>&1
    openssl req -x509 -new -nodes -key "$certificate_dir/ca.key" -sha256 -days 2 \
        -subj "/CN=managed-valkey-test-ca" -out "$MANAGED_VALKEY_CA_FILE"
    openssl genrsa -out "$certificate_dir/tls.key" 2048 >/dev/null 2>&1
    openssl req -new -key "$certificate_dir/tls.key" -subj "/CN=$VALKEY_BASE_DOMAIN" \
        -out "$certificate_dir/tls.csr"
    openssl x509 -req -in "$certificate_dir/tls.csr" \
        -CA "$MANAGED_VALKEY_CA_FILE" -CAkey "$certificate_dir/ca.key" -CAcreateserial \
        -out "$certificate_dir/tls.crt" -days 2 -sha256 \
        -extfile <(printf 'subjectAltName=DNS:%s,DNS:*.%s\n' "$VALKEY_BASE_DOMAIN" "$VALKEY_BASE_DOMAIN")
    chmod 0600 "$MANAGED_VALKEY_CA_FILE" "$certificate_dir"/*
    kubectl -n valkey-system create secret tls valkey-wildcard-tls \
        --cert="$certificate_dir/tls.crt" --key="$certificate_dir/tls.key" \
        --dry-run=client -o yaml | kubectl apply -f -
)

pin_envoy_node_port() {
    local attempt index service=""

    for attempt in {1..60}; do
        service=$(kubectl -n envoy-gateway-system get svc \
            -l gateway.envoyproxy.io/owning-gateway-name=valkey \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        [[ -z "$service" ]] || break
        sleep 5
    done
    if [[ -z "$service" ]]; then
        echo "bootstrap: Service шлюза не появился" >&2
        return 1
    fi

    index=$(kubectl -n envoy-gateway-system get svc "$service" \
        -o jsonpath="{range .spec.ports[*]}{.port}{'\n'}{end}" |
        grep -n "^${VALKEY_LISTENER_PORT}$" | cut -d: -f1 || true)
    if [[ -z "$index" ]]; then
        echo "bootstrap: у Service $service нет порта $VALKEY_LISTENER_PORT" >&2
        return 1
    fi
    kubectl -n envoy-gateway-system patch svc "$service" --type=json \
        -p "[{\"op\":\"replace\",\"path\":\"/spec/ports/$((index - 1))/nodePort\",\"value\":${ENVOY_NODE_PORT}}]"
    if [[ "$project" != managed-valkey-dev ]]; then
        kubectl -n envoy-gateway-system patch svc "$service" --type=merge \
            -p "{\"spec\":{\"externalTrafficPolicy\":\"${MANAGED_VALKEY_ENVOY_EXTERNAL_TRAFFIC_POLICY}\"}}"
    fi
    kubectl -n envoy-gateway-system wait \
        -l gateway.envoyproxy.io/owning-gateway-name=valkey \
        --for=condition=Ready pod --timeout=300s
}

write_kubeconfig() {
    local account=$1 output=$2 authority token=""

    for _ in {1..60}; do
        token=$(kubectl -n valkey-system get secret "${account}-token" \
            -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)
        [[ -z "$token" ]] || break
        sleep 2
    done
    if [[ -z "$token" ]]; then
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
  - name: $project
    cluster:
      server: $KUBERNETES_SERVER
      certificate-authority-data: $authority
users:
  - name: $account
    user:
      token: $token
contexts:
  - name: $project
    context:
      cluster: $project
      user: $account
      namespace: valkey-system
current-context: $project
YAML
    chmod 0600 "$output"
}

write_operator_env() {
    local gateway=${K3S_DOCKER_GATEWAY:-}

    if [[ -z "$gateway" ]]; then
        gateway=$(docker network inspect "$MANAGED_VALKEY_DOCKER_NETWORK" \
            --format '{{(index .IPAM.Config 0).Gateway}}')
    fi
    if [[ -z "$gateway" ]]; then
        echo "bootstrap: не найден gateway сети $MANAGED_VALKEY_DOCKER_NETWORK" >&2
        return 1
    fi
    umask 077
    printf 'VALKEY_OPERATOR_CIDRS=%s/32\n' "$gateway" >"$OPERATOR_ENV"
    chmod 0600 "$OPERATOR_ENV"
}

require_commands docker kubectl base64
if [[ ! "$K3S_EXPECTED_NODES" =~ ^[1-4]$ ]]; then
    echo "bootstrap: число нод должно быть от 1 до 4" >&2
    exit 1
fi
if [[ "$BOOTSTRAP_PROFILE" == full ]]; then
    if [[ "$MANAGED_VALKEY_ENVOY_REPLICAS" != 1 && "$MANAGED_VALKEY_ENVOY_REPLICAS" != 2 ]]; then
        echo "bootstrap: число реплик Envoy должно быть 1 или 2" >&2
        exit 1
    fi
    if ((MANAGED_VALKEY_ENVOY_REPLICAS > K3S_EXPECTED_NODES)); then
        echo "bootstrap: реплики Envoy нельзя разнести по $K3S_EXPECTED_NODES нодам" >&2
        exit 1
    fi
    case "$MANAGED_VALKEY_ENVOY_EXTERNAL_TRAFFIC_POLICY" in
    Local | Cluster) ;;
    *)
        echo "bootstrap: externalTrafficPolicy должен быть Local или Cluster" >&2
        exit 1
        ;;
    esac
    require_commands helm ip openssl sudo
    if [[ "$project" == managed-valkey-dev ]]; then
        require_commands mkcert
    fi
elif [[ "$BOOTSTRAP_PROFILE" != api ]]; then
    echo "bootstrap: неизвестный профиль $BOOTSTRAP_PROFILE" >&2
    exit 1
fi

mkdir -p "$state_dir"
"$repo_root/scripts/k3s_host_kubeconfig.sh" "$ADMIN_KUBECONFIG" >/dev/null
wait_for_api
repair_rejected_agents
remove_stale_nodes
wait_for_nodes

if [[ "$BOOTSTRAP_PROFILE" == full ]]; then
    configure_pod_routes
    install_gateway_api
    install_envoy_gateway
fi
install_managed_valkey
remove_legacy_operator
if [[ "$BOOTSTRAP_PROFILE" == full ]]; then
    if [[ "$project" == managed-valkey-dev ]]; then
        install_dev_certificate
    else
        install_test_certificate
    fi
    pin_envoy_node_port
fi
write_kubeconfig managed-valkey-api "$API_KUBECONFIG"
write_kubeconfig managed-valkey-operator "$OPERATOR_KUBECONFIG"
write_operator_env

log "готово"
