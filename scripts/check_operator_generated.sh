#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

cp "$repo_root/go.mod" "$repo_root/go.sum" "$work_dir/"
cp -R "$repo_root/internal" "$repo_root/operator" "$work_dir/"

generate() {
    (
        cd "$work_dir"
        GOTOOLCHAIN=auto go tool controller-gen object paths=./operator/api/v1alpha1/...
        GOTOOLCHAIN=auto go tool controller-gen crd paths=./operator/api/v1alpha1/... \
            output:crd:artifacts:config=operator/config/crd
        GOTOOLCHAIN=auto go tool controller-gen rbac:roleName=managed-valkey-operator \
            paths=./operator/internal/... output:rbac:artifacts:config=operator/config/rbac
    )
}

generated_files=(
    operator/api/v1alpha1/zz_generated.deepcopy.go
    operator/config/crd/valkey.h3llo-demo.com_valkeyinstances.yaml
    operator/config/rbac/role.yaml
)

generate
for file in "${generated_files[@]}"; do
    diff -u "$repo_root/$file" "$work_dir/$file"
done

for manifest in deploy/dev/rbac/operator.yaml deploy/prod/rbac.yaml; do
    role_ref=$(awk '
        $0 == "kind: RoleBinding" { binding = 1; section = ""; binding_name = ""; role_name = ""; next }
        binding && $0 == "metadata:" { section = "metadata"; next }
        binding && $0 == "roleRef:" { section = "roleRef"; next }
        binding && $0 == "subjects:" { section = "subjects"; next }
        binding && section == "metadata" && $1 == "name:" { binding_name = $2 }
        binding && section == "roleRef" && $1 == "name:" { role_name = $2 }
        binding && $0 == "---" {
            if (binding_name == "managed-valkey-operator-portforward") print role_name
            binding = 0
        }
        END {
            if (binding && binding_name == "managed-valkey-operator-portforward") print role_name
        }
    ' "$repo_root/$manifest")

    if [[ "$role_ref" != "managed-valkey-operator" ]]; then
        echo "проверка генерации оператора: $manifest ссылается на Role $role_ref" >&2
        exit 1
    fi
done

echo "проверка генерации оператора: расхождений нет"
