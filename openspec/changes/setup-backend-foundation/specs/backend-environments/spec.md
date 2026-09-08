## Purpose

Определяет одинаково запускаемые окружения разработки и production с явным порядком подготовки базы, закреплёнными образами и разделением компонентов между виртуальной машиной и Kubernetes.

## ADDED Requirements

### Requirement: Полное dev-окружение

`docker-compose.dev.yml` SHALL запускать PostgreSQL 18, VictoriaLogs, сервер k3s, два агента k3s, одноразовый bootstrap, API и оператор. Три ноды Kubernetes SHALL быть доступны для проверки жёсткого anti-affinity режима `ha`.

#### Scenario: Запуск dev-окружения

- **WHEN** разработчик запускает dev compose из корня с заполненным локальным env-файлом
- **THEN** PostgreSQL и VictoriaLogs становятся доступны, k3s формирует кластер из трёх нод, bootstrap настраивает исходную инфраструктуру, после чего запускаются API и оператор

### Requirement: Идемпотентный bootstrap k3s

Dev bootstrap SHALL ожидать готовности Kubernetes API и повторяемо устанавливать закреплённые CRD Gateway API, Envoy Gateway, инфраструктуру Managed Valkey, раздельные RBAC и kubeconfig для API и оператора. Повторный запуск SHALL приводить кластер к той же конфигурации без выдачи приложениям административного kubeconfig.

#### Scenario: Повторный bootstrap

- **WHEN** одноразовый bootstrap запускается повторно на уже настроенном dev-кластере
- **THEN** он завершается успешно, сохраняет прикладные ресурсы и обновляет только управляемую им инфраструктуру

#### Scenario: Раздельные права приложений

- **WHEN** API и оператор используют созданные bootstrap учётные записи
- **THEN** API не может менять `status` или управлять подами, а оператор не получает административный kubeconfig

### Requirement: Горячая перезагрузка Go-приложений

API и оператор в dev compose SHALL запускаться с горячей перезагрузкой при изменении соответствующих Go-файлов. Пересборка одного приложения MUST NOT перезапускать другое приложение, PostgreSQL или k3s.

#### Scenario: Изменение исходника API

- **WHEN** разработчик сохраняет Go-файл в каталоге `api` или общем `internal`
- **THEN** dev-контейнер пересобирает и перезапускает API без перезапуска инфраструктурных сервисов

### Requirement: Frontend запускается на хосте

Dev compose MUST NOT запускать отдельный frontend-контейнер. Vite SHALL запускаться из `web` на хосте и направлять `/v1` в опубликованный HTTP-порт API.

#### Scenario: Совместная разработка frontend и API

- **WHEN** dev compose работает и разработчик запускает `pnpm dev` в `web`
- **THEN** браузер получает SPA от Vite, а запросы `/v1` доходят до API в compose

### Requirement: Production-сервисы виртуальной машины

`docker-compose.prod.yml` SHALL запускать Caddy, одноразовое применение миграций, API, PostgreSQL 18 и VictoriaLogs. Frontend SHALL входить в образ API; оператор MUST NOT запускаться в production compose.

#### Scenario: Запуск production compose

- **WHEN** оператор уже развёрнут в managed Kubernetes, а на виртуальной машине запускается production compose
- **THEN** миграции применяются к PostgreSQL, API начинает работу после них, Caddy принимает внешний HTTPS-трафик, а VictoriaLogs принимает записи API и оператора

### Requirement: Production-оператор разворачивается в Kubernetes

Production-манифесты SHALL разворачивать образ оператора внутри managed Kubernetes с отдельным ServiceAccount, leader election и проверками состояния. Конфигурация оператора MUST NOT содержать адрес PostgreSQL, `DATABASE_URL` или адрес внутреннего API сервиса.

#### Scenario: Проверка конфигурации оператора

- **WHEN** production-манифесты собраны через `kustomize`
- **THEN** оператор получает только настройки Kubernetes, Valkey и экспорта логов, необходимые его ответственности

### Requirement: Миграции предшествуют API

Оба compose-окружения SHALL запускать API только после успешного применения миграций. Ошибка миграции MUST блокировать первый запуск или перезапуск API с новой версией схемы.

#### Scenario: Миграция завершилась ошибкой

- **WHEN** одноразовый сервис миграций возвращает ненулевой код
- **THEN** compose не запускает зависящий от него API

### Requirement: Конфигурация и секреты приходят из env

Репозиторий SHALL содержать `.env.example` со всеми обязательными переменными и безопасными поясняющими значениями. Настоящие пароли, токены, kubeconfig и закрытые ключи MUST NOT храниться в отслеживаемых файлах или образах.

#### Scenario: Подготовка локального env-файла

- **WHEN** разработчик копирует `.env.example` в неотслеживаемый `.env` и заполняет обязательные секреты
- **THEN** compose получает полную конфигурацию без правки YAML-файлов

### Requirement: Версии образов закреплены

Образы инфраструктуры и инструменты bootstrap SHALL использовать конкретные версии, совместимые с матрицей из `SYSTEM.md`. Production-конфигурация MUST NOT использовать тег `latest`; итоговые образы SHALL быть закрепляемы по digest.

#### Scenario: Проверка compose-файлов

- **WHEN** проверяется разрешённая конфигурация production compose
- **THEN** каждый внешний образ имеет конкретную версию и ни один сервис не ссылается на `latest`

### Requirement: Compose-файлы запускаются напрямую

Репозиторий MUST NOT содержать корневой `Justfile`. Dev- и production-окружения SHALL запускаться стандартной командой Docker Compose с явным путём к нужному файлу.

#### Scenario: Запуск dev-окружения

- **WHEN** разработчик из корня запускает `docker compose -f docker-compose.dev.yml up -d`
- **THEN** Docker Compose использует dev-конфигурацию без промежуточной команды проекта

#### Scenario: Запуск production-окружения

- **WHEN** разработчик из корня запускает `docker compose -f docker-compose.prod.yml up -d`
- **THEN** Docker Compose использует production-конфигурацию и не затрагивает dev-проект
