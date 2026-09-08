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

### Requirement: Рабочая конфигурация Docker Compose запускает серверные сервисы

`docker-compose.prod.yml` SHALL запускать Caddy с рабочей сборкой клиентского приложения, одноразовое применение миграций, API, PostgreSQL 18 и VictoriaLogs из готовых образов. Caddy SHALL обслуживать SPA и передавать API только `/v1/*`, `/livez` и `/readyz`. Оператор SHALL разворачиваться в управляемом Kubernetes через `deploy/prod` и MUST NOT входить в рабочую конфигурацию Docker Compose.

#### Scenario: Запуск рабочего окружения

- **WHEN** PostgreSQL готова и миграции завершаются успешно
- **THEN** API запускается после миграций, Caddy обслуживает клиентское приложение и маршруты API, а VictoriaLogs принимает записи API и оператора

#### Scenario: Ошибка рабочей миграции

- **WHEN** сервис миграций возвращает ненулевой код
- **THEN** Docker Compose не запускает API

### Requirement: Настройки отделены от секретов

Репозиторий SHALL содержать `.env.example` с безопасными значениями для локальной разработки. Рабочая конфигурация Docker Compose SHALL получать обычные настройки из явно заданных значений по умолчанию. Передаваемый при развёртывании рабочий `.env` SHALL содержать только пароль PostgreSQL и ключ подписи JWT. Настоящие пароли, ключи, токены и kubeconfig MUST NOT храниться в отслеживаемых файлах или образах.

#### Scenario: Подготовка локального файла переменных

- **WHEN** разработчик копирует `.env.example` в неотслеживаемый `.env`
- **THEN** Docker Compose и локальные `Justfile` получают настройки без правки исходных файлов

#### Scenario: Запуск рабочего окружения с минимальным набором переменных

- **WHEN** задание `deploy` передаёт `.env` с `POSTGRES_PASSWORD` и `JWT_SECRET`
- **THEN** Docker Compose получает остальные рабочие настройки из значений по умолчанию и не требует дополнительных секретов VictoriaLogs

### Requirement: Версии образов и исходного кода закреплены

Образы инфраструктуры SHALL использовать конкретные версии, совместимые с матрицей из `SYSTEM.md`. Рабочая конфигурация MUST NOT использовать тег `latest`. Образы приложений SHALL иметь тег с полным SHA коммита, для которого прошли проверки CI.

#### Scenario: Проверка рабочей конфигурации Docker Compose

- **WHEN** проверяется разрешённая рабочая конфигурация Docker Compose
- **THEN** каждый внешний образ имеет конкретную версию, каждый образ приложения ссылается на разворачиваемый SHA и ни один сервис не использует `latest`

### Requirement: Локальная разработка запускается через корневой Justfile

Репозиторий SHALL содержать корневой `Justfile` с рецептом `run`. Рецепт SHALL запускать `docker-compose.dev.yml`, ждать готовности сервисов, затем параллельно запускать `api/Justfile:run` и `pnpm run dev` в `web`. Оператор MUST NOT запускаться этим рецептом.

#### Scenario: Запуск локальной разработки

- **WHEN** инфраструктура и bootstrap готовы, а разработчик из корня запускает `just run`
- **THEN** Docker Compose запускает dev-инфраструктуру, API и фронтенд работают на хосте, а оператор не запускается

### Requirement: Compose-файлы можно запускать напрямую

Dev- и production-окружения SHALL запускаться Docker Compose с явным путём к нужному файлу.

#### Scenario: Прямой запуск dev-окружения

- **WHEN** разработчик из корня запускает `docker compose -f docker-compose.dev.yml up -d --remove-orphans`
- **THEN** Docker Compose использует dev-конфигурацию и удаляет контейнеры сервисов, которых в ней больше нет

#### Scenario: Запуск production-окружения

- **WHEN** разработчик из корня запускает `docker compose -f docker-compose.prod.yml up -d`
- **THEN** Docker Compose использует production-конфигурацию и не затрагивает dev-проект

### Requirement: Доступ рабочего окружения к базе и логам ограничен

PostgreSQL SHALL публиковаться только на `127.0.0.1:45432`, а VictoriaLogs MUST NOT публиковать порт на хост. Caddy SHALL без проверки имени и пароля передавать `POST /insert/opentelemetry/v1/logs` и `/select/*` с публичного `logs.h3llo-demo.com` во VictoriaLogs. Остальные пути домена логов SHALL возвращать 404. Caddy SHALL удалять входной заголовок `Authorization` перед передачей запроса VictoriaLogs. Рабочая конфигурация Docker Compose MUST NOT требовать секреты VictoriaLogs.

#### Scenario: Публичный экспорт логов

- **WHEN** клиент без заголовка авторизации отправляет OTLP-запрос на разрешённый путь
- **THEN** Caddy передаёт запрос VictoriaLogs

#### Scenario: Публичный просмотр логов

- **WHEN** клиент без заголовка авторизации открывает `/select/vmui`
- **THEN** Caddy передаёт запрос интерфейсу VictoriaLogs

#### Scenario: Разделение экспорта и просмотра

- **WHEN** клиент обращается к маршруту OTLP или `/select/*` без пароля
- **THEN** Caddy передаёт только разрешённый путь VictoriaLogs и не требует отдельной учётной записи для записи или чтения

#### Scenario: Запрещённый путь VictoriaLogs

- **WHEN** клиент обращается к другому пути на домене логов
- **THEN** Caddy возвращает 404 и не передаёт запрос VictoriaLogs

#### Scenario: Экспорт оператора без секрета

- **WHEN** оператор запускается в рабочем окружении с адресом VictoriaLogs по HTTPS
- **THEN** он отправляет OTLP без `VL_OTLP_USERNAME`, `VL_OTLP_PASSWORD_FILE` и объекта Kubernetes `Secret` с именем `valkey-logs-export`
