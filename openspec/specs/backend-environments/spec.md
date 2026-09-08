# backend-environments Specification

## Purpose

Окружения обеспечивают локальный запуск и проверку API и оператора с PostgreSQL и k3s, а также развёртывание серверных сервисов на VM и оператора в managed Kubernetes.

## Requirements

### Requirement: Dev compose содержит только инфраструктуру

`docker-compose.dev.yml` SHALL запускать PostgreSQL 18, сервер k3s и два агента k3s. API, оператор, миграции, VictoriaLogs и bootstrap MUST NOT быть сервисами dev compose.

#### Scenario: Запуск локальной инфраструктуры

- **WHEN** разработчик запускает `docker compose -f docker-compose.dev.yml up -d --remove-orphans`
- **THEN** Docker Compose запускает только PostgreSQL и три ноды k3s

### Requirement: Dev-сервисы доступны только с хоста

PostgreSQL SHALL использовать порт `5432` внутри контейнера и публиковаться на `127.0.0.1:${POSTGRES_PORT}`. Kubernetes API `6443` и NodePort шлюза `31379` SHALL также публиковаться только на `127.0.0.1`.

#### Scenario: Подключение локального API

- **WHEN** API запускается на хосте с `DATABASE_URL`, указывающим на `127.0.0.1:${POSTGRES_PORT}`
- **THEN** API подключается к PostgreSQL без адреса внутренней сети compose

### Requirement: k3s имеет три ноды

Dev compose SHALL запускать `k3s-server`, `k3s-agent-1` и `k3s-agent-2` на закреплённой версии k3s с выключенными Traefik и servicelb. Имена нод SHALL быть постоянными. Bootstrap SHALL удалять неготовые записи agent-нод после пересоздания их контейнеров, чтобы агенты могли зарегистрироваться с новым паролем.

#### Scenario: Готовность кластера после пересоздания

- **WHEN** контейнеры агентов пересозданы при сохранённом состоянии сервера
- **THEN** `scripts/k3s_bootstrap.sh` восстанавливает регистрацию и `kubectl get nodes` показывает три готовые ноды

### Requirement: Dev-кластер готовится на хосте

`scripts/k3s_bootstrap.sh` SHALL запускать системные `kubectl`, `helm` и `mkcert` на хосте. Скрипт SHALL получить административный kubeconfig из `k3s-server`, установить Gateway API `v1.5.1`, Envoy Gateway `v1.8.4`, CRD и RBAC Managed Valkey, dev-инфраструктуру, TLS Secret и NodePort `31379`.

#### Scenario: Повторный bootstrap

- **WHEN** разработчик дважды запускает `scripts/k3s_bootstrap.sh`
- **THEN** оба запуска завершаются успешно и не удаляют прикладные ресурсы

#### Scenario: Незавершённая операция Helm

- **WHEN** сохранённый релиз Envoy Gateway имеет состояние `pending-install`, `pending-upgrade` или `pending-rollback`
- **THEN** bootstrap удаляет незавершённую установку либо откатывает релиз к последней готовой ревизии и продолжает обновление

### Requirement: Приложения получают отдельные kubeconfig

Bootstrap SHALL создать отдельные kubeconfig API и оператора в `tmp/k3s` с адресом `https://127.0.0.1:6443` и режимом `0600`. Административный kubeconfig MUST NOT использоваться приложениями. API MUST NOT иметь прав на `ValkeyInstance/status` и поды.

#### Scenario: Проверка прав API

- **WHEN** разработчик выполняет `kubectl auth can-i` с kubeconfig API
- **THEN** API может создавать `ValkeyInstance`, но не может обновлять подресурс `status` или читать поды

### Requirement: API и оператор запускаются на хосте

`api/Justfile` SHALL загружать корневой `.env`, применять миграции перед `just run` и запускать API через `go run`. `operator/Justfile` SHALL загружать `.env`, генерировать CRD и RBAC и запускать оператор через `go run` с отдельным kubeconfig. Локальные процессы SHALL писать логи в stdout без обязательного `VL_OTLP_URL`.

#### Scenario: Локальная разработка backend

- **WHEN** инфраструктура и bootstrap готовы, а разработчик запускает `just run` в каталогах `api` и `operator`
- **THEN** оба процесса работают на хосте и отвечают на свои проверки состояния

### Requirement: Тестовая база создаётся по требованию

`api just test` SHALL создать базу `managed_valkey_test`, если она отсутствует, применить к ней миграции и запустить тесты. Dev compose MUST NOT монтировать init-скрипт PostgreSQL.

#### Scenario: Первый запуск тестов

- **WHEN** PostgreSQL запущена с новым томом и разработчик выполняет `api just test`
- **THEN** команда создаёт тестовую базу, применяет миграции и запускает тесты без ручной подготовки

### Requirement: Production compose запускает серверные сервисы

`docker-compose.prod.yml` SHALL запускать Caddy, одноразовое применение миграций, API, PostgreSQL 18 и VictoriaLogs. Оператор SHALL разворачиваться в managed Kubernetes через `deploy/prod`.

#### Scenario: Запуск production compose

- **WHEN** PostgreSQL готова и миграции завершаются успешно
- **THEN** API запускается после миграций, Caddy направляет внешний HTTPS-трафик в API, а VictoriaLogs принимает записи API и оператора

#### Scenario: Ошибка production-миграции

- **WHEN** сервис миграций возвращает ненулевой код
- **THEN** production compose не запускает API

### Requirement: Конфигурация и секреты приходят из env

Репозиторий SHALL содержать `.env.example` со всеми обязательными переменными и безопасными значениями-заглушками. Настоящие пароли, токены, kubeconfig и закрытые ключи MUST NOT храниться в отслеживаемых файлах или образах.

#### Scenario: Подготовка локального env-файла

- **WHEN** разработчик копирует `.env.example` в неотслеживаемый `.env`
- **THEN** Compose и локальные `Justfile` получают конфигурацию без правки исходных файлов

### Requirement: Версии образов закреплены

Образы инфраструктуры SHALL использовать конкретные версии, совместимые с матрицей из `SYSTEM.md`. Production-конфигурация MUST NOT использовать тег `latest`; итоговые образы SHALL быть закрепляемы по digest.

#### Scenario: Проверка production compose

- **WHEN** проверяется разрешённая конфигурация production compose
- **THEN** каждый внешний образ имеет конкретную версию и ни один сервис не ссылается на `latest`

### Requirement: Compose-файлы запускаются напрямую

Репозиторий MUST NOT содержать корневой `Justfile`. Dev- и production-окружения SHALL запускаться Docker Compose с явным путём к нужному файлу.

#### Scenario: Запуск dev-окружения

- **WHEN** разработчик из корня запускает `docker compose -f docker-compose.dev.yml up -d --remove-orphans`
- **THEN** Docker Compose использует dev-конфигурацию и удаляет контейнеры сервисов, которых в ней больше нет

#### Scenario: Запуск production-окружения

- **WHEN** разработчик из корня запускает `docker compose -f docker-compose.prod.yml up -d`
- **THEN** Docker Compose использует production-конфигурацию и не затрагивает dev-проект

### Requirement: Production-доступ к базе и логам ограничен

PostgreSQL SHALL публиковаться только на `127.0.0.1:45432`, VictoriaLogs MUST NOT публиковать порт на хост. Caddy SHALL принимать OTLP оператора через HTTPS на `logs.h3llo-demo.com` с Basic Auth `valkey-operator` только для `POST /insert/opentelemetry/v1/logs`. Учётная запись `logs-reader` SHALL получать доступ только к `/select/*`. Caddy SHALL удалять заголовок `Authorization` перед передачей запроса VictoriaLogs. Оба bcrypt-хеша SHALL быть обязательными переменными production compose.

#### Scenario: Разделение экспорта и просмотра

- **WHEN** клиент обращается к домену логов без пароля, с неверным паролем или с учётной записью для другого назначения
- **THEN** Caddy отклоняет запрос; остальные пути возвращают 404

#### Scenario: Секрет экспорта оператора

- **WHEN** оператор запускается в production
- **THEN** он читает пароль из смонтированного только для чтения Secret `valkey-logs-export`, а его `VL_OTLP_URL` использует HTTPS
