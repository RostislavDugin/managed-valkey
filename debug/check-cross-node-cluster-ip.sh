#!/usr/bin/env bash
set -euo pipefail

export KUBECONFIG=/home/rostislav/development/project/managed-valkey/temp/cluster-6560_kubeconfig.yaml
FIRST_NODE=k8sc-worker-md-929247fc-ddba-4516-909c-8ca395dc7927-jljnb-kz68r
SECOND_NODE=k8sc-worker-md-929247fc-ddba-4516-909c-8ca395dc7927-jljnb-h76z5

NAMESPACE=cross-node-check

echo "1. Удаляем namespace от предыдущего запуска"
kubectl delete namespace "$NAMESPACE" --ignore-not-found --wait=true

echo "2. Создаём namespace $NAMESPACE"
kubectl create namespace "$NAMESPACE"

echo "3. Запускаем nginx на первой ноде: $FIRST_NODE"
kubectl run nginx \
    --namespace="$NAMESPACE" \
    --image=nginx:1.27.4-alpine \
    --restart=Never \
    --overrides="{\"apiVersion\":\"v1\",\"spec\":{\"nodeName\":\"$FIRST_NODE\"}}"

echo "4. Ждём готовности nginx"
kubectl wait \
    --namespace="$NAMESPACE" \
    --for=condition=Ready \
    pod/nginx \
    --timeout=120s

echo "5. Создаём внутренний сервис ClusterIP для nginx"
kubectl expose pod nginx \
    --namespace="$NAMESPACE" \
    --name=nginx \
    --port=80

echo "6. Запускаем временный Pod на второй ноде: $SECOND_NODE"
kubectl run request-from-second-node \
    --namespace="$NAMESPACE" \
    --image=busybox:1.37.0 \
    --restart=Never \
    --overrides="{\"apiVersion\":\"v1\",\"spec\":{\"nodeName\":\"$SECOND_NODE\"}}" \
    --command -- sleep 3600

echo "7. Ждём готовности Pod на второй ноде"
kubectl wait \
    --namespace="$NAMESPACE" \
    --for=condition=Ready \
    pod/request-from-second-node \
    --timeout=120s

echo "8. Проверяем, что nginx и временный Pod запущены на разных нодах"
kubectl get pods --namespace="$NAMESPACE" -o wide

echo "9. Показываем созданный сервис"
kubectl get service nginx --namespace="$NAMESPACE" -o wide

echo "10. Получаем адрес ClusterIP"
CLUSTER_IP=$(kubectl get service nginx \
    --namespace="$NAMESPACE" \
    --no-headers \
    -o custom-columns=CLUSTER_IP:.spec.clusterIP)

echo "11. Пытаемся достучаться до nginx на первой ноде со второй через Service: http://$CLUSTER_IP/"

RESULT=0
kubectl exec \
    --namespace="$NAMESPACE" \
    request-from-second-node -- wget -S -T 10 -O - "http://$CLUSTER_IP/" || RESULT=$?

echo "12. Удаляем namespace $NAMESPACE"
kubectl delete namespace "$NAMESPACE" --wait=false

exit "$RESULT"
