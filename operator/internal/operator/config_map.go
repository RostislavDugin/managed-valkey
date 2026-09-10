package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

const (
	valkeyConfigKey    = "valkey.conf"
	startScriptKey     = "start.sh"
	readinessScriptKey = "readiness.sh"
	configDigestLength = 12
	gibibyte           = int64(1024 * 1024 * 1024)
	mebibyte           = int64(1024 * 1024)
)

var ErrConfigMapCollision = errors.New("имя ConfigMap занято другим содержимым")

func (r *ValkeyInstanceReconciler) reconcileConfigMap(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	desired, err := r.desiredConfigMap(instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	current := &corev1.ConfigMap{}
	key := client.ObjectKeyFromObject(desired)
	if err := r.Get(ctx, key, current); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("прочитать ConfigMap Valkey: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("создать ConfigMap Valkey: %w", err)
		}

		return requeueIf(true), nil
	}
	if current.Immutable == nil || !*current.Immutable || !maps.Equal(current.Data, desired.Data) {
		return ctrl.Result{}, ErrConfigMapCollision
	}

	return ctrl.Result{}, nil
}

func (r *ValkeyInstanceReconciler) desiredConfigMap(
	instance *valkeyv1alpha1.ValkeyInstance,
) (*corev1.ConfigMap, error) {
	accepted := instance.Status.AcceptedConfiguration
	if accepted == nil {
		return nil, errIncompleteIntent
	}

	data := configMapData(*accepted)
	immutable := true
	configMap := &corev1.ConfigMap{
		Immutable: &immutable,
		Data:      data,
	}
	configMap.Name = accepted.Slug + "-config-" + configDigest(data)
	configMap.Namespace = instance.Namespace
	if err := controllerutil.SetControllerReference(instance, configMap, r.Scheme); err != nil {
		return nil, fmt.Errorf("назначить ownerReference ConfigMap: %w", err)
	}

	return configMap, nil
}

func configMapData(accepted valkeyv1alpha1.AcceptedConfiguration) map[string]string {
	ramBytes := int64(accepted.RAMGB) * gibibyte
	maxmemory := ramBytes * 3 / 4
	backlog := min(max(ramBytes/100, mebibyte), 64*mebibyte)
	ioThreads := int64(1)
	if accepted.VCPU >= 4 {
		ioThreads = min(int64(accepted.VCPU), 8)
	}

	configuration := fmt.Sprintf(
		"port 6379\n"+
			"bind 0.0.0.0\n"+
			"protected-mode no\n"+
			"aclfile /etc/valkey-auth/users.acl\n"+
			"maxmemory %d\n"+
			"maxmemory-policy allkeys-lru\n"+
			"lazyfree-lazy-user-flush yes\n"+
			"save \"\"\n"+
			"appendonly no\n"+
			"repl-diskless-sync yes\n"+
			"repl-diskless-load on-empty-db\n"+
			"repl-backlog-size %d\n"+
			"replica-serve-stale-data yes\n"+
			"io-threads %d\n",
		maxmemory,
		backlog,
		ioThreads,
	)
	if accepted.Mode == valkeyv1alpha1.ValkeyModeHA {
		configuration += fmt.Sprintf(
			"replicaof %s-primary.valkey-%s.svc 6379\n",
			accepted.Slug,
			accepted.Slug,
		)
	}
	readiness := "#!/bin/sh\n" +
		"set -eu\n" +
		"export REDISCLI_AUTH=\"$(cat /etc/valkey-auth/health-password)\"\n" +
		fmt.Sprintf(
			"role=\"$(timeout %d valkey-cli --user health --no-auth-warning --raw ROLE | head -n 1 | tr -d '\\r')\"\n"+
				"info=\"$(timeout %d valkey-cli --user health --no-auth-warning --raw INFO replication | tr -d '\\r')\"\n",
			config.ReadinessCommandTimeout,
			config.ReadinessCommandTimeout,
		)
	if accepted.Mode == valkeyv1alpha1.ValkeyModeHA {
		readiness += "case \"$role\" in\n" +
			"  master) printf '%s\\n' \"$info\" | grep -q '^role:master$' ;;\n" +
			"  slave) printf '%s\\n' \"$info\" | grep -q '^role:slave$' && " +
			"printf '%s\\n' \"$info\" | grep -q '^master_link_status:up$' && " +
			"printf '%s\\n' \"$info\" | grep -q '^master_sync_in_progress:0$' ;;\n" +
			"  *) exit 1 ;;\n" +
			"esac\n"
	} else {
		readiness += "[ \"$role\" = master ]\n" +
			"printf '%s\\n' \"$info\" | grep -q '^role:master$'\n"
	}

	return map[string]string{
		valkeyConfigKey: configuration,
		startScriptKey: "#!/bin/sh\n" +
			"set -eu\n" +
			"umask 077\n" +
			"cp /etc/valkey-config/valkey.conf /run/valkey/valkey.conf\n" +
			"printf 'primaryuser replica\\nprimaryauth %s\\n' \"$(cat /etc/valkey-auth/replica-password)\" >> /run/valkey/valkey.conf\n" +
			"valkey-server /run/valkey/valkey.conf &\n" +
			"valkey_pid=$!\n" +
			"trap 'kill -TERM \"$valkey_pid\" 2>/dev/null || true' TERM INT\n" +
			"set +e\n" +
			"wait \"$valkey_pid\"\n" +
			"status=$?\n" +
			"if kill -0 \"$valkey_pid\" 2>/dev/null; then\n" +
			"  wait \"$valkey_pid\"\n" +
			"  status=$?\n" +
			"fi\n" +
			"exit \"$status\"\n",
		readinessScriptKey: readiness,
	}
}

func configDigest(data map[string]string) string {
	keys := slices.Sorted(maps.Keys(data))
	hash := sha256.New()
	for _, key := range keys {
		_, _ = hash.Write([]byte(key))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(data[key]))
		_, _ = hash.Write([]byte{0})
	}

	return hex.EncodeToString(hash.Sum(nil))[:configDigestLength]
}
