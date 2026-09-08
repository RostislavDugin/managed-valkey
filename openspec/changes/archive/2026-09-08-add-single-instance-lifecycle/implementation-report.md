# Отчёт о реализации

## 1.1 ContainerRestartRules

Дата проверки: 2026-09-08.

Стенд использовал `rancher/k3s:v1.35.7-k3s1`, Kubernetes `v1.35.7+k3s1` и `valkey/valkey:8.1.9`. API server принял StatefulSet с `updateStrategy: OnDelete` и контейнерным `restartPolicy: Never`.

Проверка выполняется с выключенным оператором:

```sh
kubectl apply -f openspec/changes/add-single-instance-lifecycle/verification/container-restart-rules.yaml
kubectl -n mv-verify-restart-policy wait --for=condition=Ready pod/valkey-0 --timeout=180s
kubectl -n mv-verify-restart-policy exec valkey-0 -- valkey-cli shutdown nosave
sleep 30
kubectl -n mv-verify-restart-policy get pod valkey-0 \
  -o jsonpath='{.metadata.uid}{"\t"}{.metadata.deletionTimestamp}{"\t"}{.status.phase}{"\t"}{.status.containerStatuses[0].containerID}{"\t"}{.status.containerStatuses[0].restartCount}{"\t"}{.status.containerStatuses[0].state.terminated.reason}{"\t"}{.status.containerStatuses[0].state.terminated.exitCode}{"\n"}'
```

После `shutdown nosave` и ожидания Pod сохранил UID `c5329f8d-0634-4a45-a70a-2a837fec5b69` и container ID `3b5d396844d26f0ecd3f460eff5557004966f9b93ab9a82b7c0e54953a6c254c`. Состояние контейнера: `Completed`, код 0, `restartCount=0`. StatefulSet поставил Pod на удаление, а finalizer `valkey.h3llo-demo.com/process-stopped` сохранил Pod и терминальный статус до возврата оператора.

Контрольный прогон без Pod finalizer показал немедленную замену завершившегося Pod самим StatefulSet. Поэтому finalizer должен появиться до допуска `app`; одного `restartPolicy: Never` недостаточно для сохранения истории процесса.

Очистка после проверки:

```sh
kubectl -n mv-verify-restart-policy patch pod valkey-0 --type=merge -p '{"metadata":{"finalizers":null}}'
kubectl delete namespace mv-verify-restart-policy --wait=true --timeout=120s
```

## 1.2 Активная конфигурация Envoy

Дата проверки: 2026-09-08.

Стенд использовал Envoy Gateway `v1.8.4` и Envoy `1.38.4` из образа `docker.io/envoyproxy/envoy:distroless-v1.38.4@sha256:b28fbee81528c5b6e8857412e5e0f48ea5baa0199cf73ab611aa7f88a808eba7`. В Gateway работали две реплики Envoy.

Тестовый listener добавлялся в общий Gateway JSON Patch с операцией `add` по пути `/spec/listeners/-`. Остальные объекты находятся в `verification/envoy-route.yaml`. Admin API читался отдельно у каждой реплики:

```sh
kubectl --kubeconfig tmp/k3s/admin.kubeconfig apply \
  -f openspec/changes/add-single-instance-lifecycle/verification/envoy-route.yaml
kubectl --kubeconfig tmp/k3s/admin.kubeconfig -n envoy-gateway-system \
  port-forward pod/<envoy-pod> 29000:19000
curl -fsS 'http://127.0.0.1:29000/config_dump?include_eds'
curl -fsS 'http://127.0.0.1:29000/certs'
```

Обе реплики вернули `state=LIVE`, одинаковую активную версию listener и один backend текущего тестового Pod. Проверяемый listener находится в `ListenersConfigDump.dynamic_listeners[].active_state.listener.filter_chains[]`. Для готовой конфигурации `active_state` присутствует, а `warming_state` отсутствует. Наличие `warming_state` не позволяет считать конфигурацию подтверждённой.

В filter chain подтверждаются `server_names`, имя `TCPRoute`, кластер backend, SDS Secret сертификата и `idle_timeout=3600s`. Адрес backend находится в `EndpointsConfigDump.dynamic_endpoint_configs[].endpoint_config`. В `/certs` проверяются `subject_alt_names`, `valid_from` и `expiration_time` цепочки с `path=<inline>`; содержимого сертификата и закрытого ключа в сохранённом примере нет.

Конфигурации whitelist различаются однозначно. При выключенном whitelist фильтр `envoy.filters.network.rbac` отсутствует. Вариант allow содержит `SourceIPInput`, CIDR matcher, действие `ALLOW` и `on_no_match=DENY`. Вариант deny-all содержит RBAC с пустым списком matchers и `on_no_match=DENY`.

Сокращённые обезличенные ответы сохранены в `verification/envoy-config-dump.example.json` и `verification/envoy-certs.example.json`. Они содержат только поля, нужные будущей проверке оператора.

## 1.3 Сетевой путь и права dev-оператора

Дата проверки: 2026-09-08.

Учётная запись `system:serviceaccount:valkey-system:managed-valkey-operator` читает Pod и может создавать подресурс `pods/portforward` в `envoy-gateway-system`. Port-forward к admin API каждой из двух реплик вернул `state=LIVE` и Envoy `1.38.4`:

```sh
kubectl --kubeconfig tmp/k3s/operator.kubeconfig auth can-i create pods \
  --subresource=portforward -n envoy-gateway-system
kubectl --kubeconfig tmp/k3s/operator.kubeconfig -n envoy-gateway-system \
  port-forward pod/<envoy-pod> 29100:19000
curl -fsS http://127.0.0.1:29100/server_info
```

Хост подключился к порту 6379 тестового Pod на worker-ноде при действующей NetworkPolicy из `verification/envoy-route.yaml`:

```sh
pod_ip=$(kubectl --kubeconfig tmp/k3s/operator.kubeconfig \
  -n mv-verify-envoy get pod verify-valkey -o jsonpath='{.status.podIP}')
timeout 3 bash -c "exec 3<>/dev/tcp/$pod_ip/6379"
```

Два временных контейнера с адресами `172.27.0.101` и `172.27.0.102` получили `PONG` через TLS listener. Access log Envoy сохранил оба адреса в `downstream_remote_address`, SNI `verify.valkey.localhost` и кластер `tcproute/mv-verify-envoy/verify/rule/-1`:

```sh
docker run --rm --network managed-valkey-dev_default --ip 172.27.0.101 \
  valkey/valkey:8.1.9 valkey-cli --tls --insecure \
  --sni verify.valkey.localhost -h 172.27.0.3 -p 31379 ping
docker run --rm --network managed-valkey-dev_default --ip 172.27.0.102 \
  valkey/valkey:8.1.9 valkey-cli --tls --insecure \
  --sni verify.valkey.localhost -h 172.27.0.3 -p 31379 ping
```

Сгенерированный ClusterRole даёт чтение Pod, а Role `managed-valkey-operator` в `envoy-gateway-system` разрешает port-forward. RoleBinding в манифестах dev и prod ссылается на эту Role. Проверка выполнена после удаления Role `managed-valkey-operator`, прежней Role `managed-valkey-operator-portforward` и RoleBinding, а затем повторного запуска bootstrap. Service Envoy уже имел `externalTrafficPolicy: Local`, поэтому адреса клиентов не терялись.

## 5.1 и 5.2 Запуск single на k3s

Дата проверки: 2026-09-08.

Оператор запущен на хосте с admin kubeconfig после установки сгенерированной CRD. Тест напрямую создал namespace, Secret с тестовым хешем и полный `ValkeyInstance` режима `single`. Оператор создал неизменяемую ConfigMap, StatefulSet, headless и primary Services и NetworkPolicy. PDB и ресурсы `-ro` не появились.

Pod на `valkey/valkey:8.1.9` перешёл в Ready. У него `restartPolicy=Always`, у контейнера `restartPolicy=Never`, `restartCount=0`, liveness отсутствует. Readiness выполнил `ROLE` и `INFO replication` под пользователем `health`. Для `valkey-cli` используется переменная `REDISCLI_AUTH`; вывод `INFO` очищается от `CR`, прежде чем скрипт сравнивает строку `role:master`.

Состояние `app` проверено под служебной учётной записью без вывода пароля:

```sh
kubectl --kubeconfig tmp/k3s/admin.kubeconfig -n valkey-stage-k3s001 \
  wait --for=condition=Ready pod/stage-k3s001-0 --timeout=180s
kubectl --kubeconfig tmp/k3s/admin.kubeconfig -n valkey-stage-k3s001 \
  exec stage-k3s001-0 -- sh -c \
  'export REDISCLI_AUTH="$(cat /etc/valkey-auth/operator-password)"; valkey-cli --user operator --no-auth-warning ACL GETUSER app | sed -n "1,2p"'
```

Ответ содержал `flags` и `off`: клиентская учётная запись осталась выключенной после запуска. Тестовый namespace удалён после остановки оператора.

## 5.3 и 5.4 История процесса и образ

Дата проверки: 2026-09-08.

Контроллер наблюдает за Pod, Secret, StatefulSet, ConfigMap, Service и NetworkPolicy. Перед первым подключением к Valkey он сохраняет finalizer `valkey.h3llo-demo.com/process-stopped` в Pod. После приёма управления он записывает Pod UID, container ID, `run_id`, имя и UID Node, роль и готовность контейнера в `status.nodes`. Клиентская учётная запись остаётся выключенной. Смена Pod UID, container ID или `run_id` без сохранённого завершения прежнего процесса даёт `RecoveryRequired` с причиной `TerminationProofLost` и не заменяет прежнюю запись.

Если `VALKEY_IMAGE` отличается от образа существующего StatefulSet, контроллер записывает `RecoveryRequired` с причиной `ValkeyImageChanged` и строит желаемый шаблон с прежним образом. После возврата настройки к исходному образу условие снимается. Проверка Secret очищает только причины восстановления, связанные с Secret, поэтому причины образа и истории процесса не теряются.

Проверки выполнены командой:

```sh
cd operator
just test-fast
```

Тесты проверяют порядок finalizer и обращения к процессу, полный снимок идентичности, смену трёх составляющих идентичности, очередь reconcile по метке Pod, неизменность образа StatefulSet и снятие условия после возврата настройки.

## 6. Маршруты и проверка Envoy

Дата проверки: 2026-09-08.

В схему оператора добавлены Gateway API `v1.5.1` и типы Envoy Gateway `v1.8.4`. Сгенерированные права разрешают только `create` для `pods/portforward` в `envoy-gateway-system` и чтение `Certificate` в `valkey-system`. Роли в манифестах dev и prod используют сгенерированные правила.

Контроллер добавляет listener инстанса в общий `Gateway valkey`, создаёт `TCPRoute` к primary Service и управляет `SecurityPolicy`. Обновление Gateway повторяется после конфликта по `resourceVersion` и сохраняет listeners других инстансов. Для выключенного whitelist политика отсутствует. Включённый пустой whitelist создаёт правило deny-all, а непустой добавляет разрешённые CIDR. Ресурсы `-ro` для single не создаются.

Перед чтением Envoy оператор проверяет актуальные conditions Gateway, TCPRoute, SecurityPolicy и общей ClientTrafficPolicy. TLS Secret проверяется по сроку и имени хоста. В prod также требуется `Certificate Ready=True` текущего поколения, а в dev достаточно действующего сертификата из Secret.

Оператор читает `/config_dump?include_eds` и `/certs` через отдельный port-forward к двум текущим процессам Envoy. Разбор соответствует полям Envoy `1.38.4`, зафиксированным в примерах этапа 1: SNI filter chain, backend-кластер и endpoint, RBAC, SDS Secret, сертификат и TCP idle timeout. Warming даёт `pending`. Недоступный admin API, один процесс, смена состава и неизвестный формат дают `unknown`. Содержимое ответов остаётся только в памяти.

Отпечаток включает hostname, имя и namespace backend Service, порт, состояние whitelist и отсортированные CIDR, хеш сертификата и idle timeout. IP Pod и поколение общего Gateway в него не входят. Снимок двух Envoy переиспользуется в течение `NetworkVerifyInterval`; новый container ID и истечение интервала требуют нового чтения. Последнее время успешной проверки и идентичности Envoy сохраняются при `pending` и `unknown`. Если поколение Gateway изменилось только из-за соседнего listener, прежнее подтверждение используется при совпадении отпечатка и состава Envoy.

Проверки выполнены командой:

```sh
cd operator
just test-fast
```

Envtest поднимает API server Kubernetes `1.35.0`. Он проверяет параллельное обновление общего Gateway с конфликтом, все варианты whitelist, внешний контракт CRD, актуальность поколений и полный reconcile вместе с Gateway API и Envoy CRD.

## 7. Готовность и наблюдение

Дата проверки: 2026-09-08.

Оператор включает `app` только после сохранения идентичности текущего процесса, назначения роли primary, проверки EndpointSlice и подтверждения активной конфигурации обеих реплик Envoy. EndpointSlice должен содержать единственный готовый адрес текущего Pod с совпадающими UID и IP. После повторной проверки роли и ACL контроллер записывает `initialized`, `applied`, `appliedPasswordVersion`, `primaryOrdinal`, идентичность primary и `observedGeneration` принятого намерения.

До первого успеха фаза остаётся `provisioning`. Таймаут считается от `metadata.creationTimestamp`, поэтому рестарт оператора не начинает десятиминутный интервал заново. После первого успеха потеря primary или подтверждённая проблема сети переводит инстанс в `unavailable`; успешный повтор возвращает `running`. `observedAt` обновляется по наблюдениям Kubernetes и Valkey независимо от результата сверки Envoy, а `network.verifiedAt` меняется только после новой успешной проверки.

Unit-тесты используют поддельные часы и проверяют таймаут, heartbeat, переходы `running` и `unavailable`, повтор после потерянного ответа `ACL SETUSER` и сохранение времени последней проверки Envoy. Envtest проверяет `resourceVersion` CR после неизменного reconcile. Отдельный тест воспроизводит конфликт записи status и подтверждает сохранение поля, записанного параллельно.

## 8. Остановка, замена и удаление

Дата проверки: 2026-09-08.

Терминальное состояние контейнера сохраняется в `status.nodes[].termination` до снятия finalizer Pod. Доказательство содержит идентичность процесса, время, причину, код выхода и источник `container_status`. Новый экземпляр reconciler продолжает удаление удержанного Pod по сохранённому status, в том числе когда Kubernetes принял удаление, но ответ клиенту потерялся. Если Pod исчез без терминального состояния, контроллер разрешает продолжение только после удаления Node с сохранёнными именем и UID; состояние NotReady и ошибка Kubernetes доказательством не считаются. Повторное использование имени Node с другим UID считается удалением прежней ноды.

Удаление CR проходит через сохранённые стадии `removing_network`, `disabling_app`, `stopping` и `verifying`. Контроллер удаляет только маршрут и listener этого инстанса, снимает метку primary, выключает `app`, закрывает его соединения и уменьшает StatefulSet до нуля. Secret остаётся до подтверждённой остановки всех известных процессов. Каждый проход можно выполнить новым экземпляром reconciler. Удаление до появления workload, во время `provisioning`, после `error` и при `UnsupportedChange` не создаёт ресурсы заново.

Интеграционный тест выключает оператор, штатно завершает Valkey, затем запускает оператор снова. StatefulSet получает новый Pod только после сохранения завершения прежнего процесса; новый single проходит допуск заново и имеет пустой кэш. Ещё один рестарт выполняется между запросом удаления CR и завершением стадий. CR и его дочерние ресурсы исчезают штатно, после чего тест удаляет namespace и ждёт его фактического удаления.

## 9. Самостоятельный полный тест

Дата проверки: 2026-09-08.

`operator just test-full` запускает одну копию manager как часть Go-теста и создаёт входные namespace, Secret и `ValkeyInstance` напрямую через Kubernetes. HTTP API и PostgreSQL не запускаются и не используются. При отсутствии kubeconfig, Gateway, ClientTrafficPolicy, TLS Secret или готового API команда завершается с ошибкой. Очистка запрашивает обычное удаление CR и не снимает finalizers принудительно.

Тест проверяет TLS от корневого сертификата mkcert, правильный и неправильный SNI, успешный и ошибочный `AUTH`, `SET`, `GET`, запрет `CONFIG GET`, начальный запрет доступа, whitelist off, один разрешённый CIDR и deny-all. Два Docker-клиента используют адреса `172.27.0.101` и `172.27.0.102`; пароль передаётся через окружение процесса и не входит в аргументы или вывод. Для каждой из двух реплик Envoy тест держит обычное и pub/sub соединение первого инстанса во время создания и удаления соседа. После операции соединения отвечают, а данные первого инстанса сохраняются.

Проверка выполнена на k3s `v1.35.7+k3s1`, Gateway API `v1.5.1`, Envoy Gateway `v1.8.4`, Envoy `1.38.4`, Valkey `8.1.9` и Go `1.27.1`:

```sh
cd operator
just test-fast
just test-full
```

Точный момент потери ответа после применения ACL воспроизводится локальным RESP-сервером: изменяющая команда отправляется один раз, первый проход не подтверждает готовность, следующий заново читает состояние и завершает допуск. Уничтожение настоящей worker-ноды проверка не выполняет; правила доказательства её удаления покрыты unit-тестами имени и UID Node. HA, изменение размера, ротация пароля, обновление whitelist работающего инстанса, метрики и восстановление зависших процессов остаются за пределами этого изменения и перечислены в `operator/TODO.md`.
