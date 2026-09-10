## MODIFIED Requirements

### Requirement: Ресурс принимает последние снимки метрик узлов

`status.metrics` ресурса `ValkeyInstance` SHALL принимать не более трёх элементов с уникальным `ordinal`. Каждый элемент SHALL содержать `ordinal`, `podUID`, `containerID`, `runId`, `collectedAt`, `role`, `usedMemoryBytes`, `maxmemoryBytes`, `connectedClients`, `opsPerSec`, `keyspaceHits`, `keyspaceMisses`, `evictedKeys` и допускающее `null` поле `cpuMillicores`. Идентификаторы SHALL быть непустыми, роль SHALL быть `primary` или `replica`, числовые показатели SHALL быть неотрицательными. `collectedAt` SHALL сохранять время UTC с точностью до микросекунд. Оператор SHALL заполнять поле последними успешными снимками текущих процессов согласно контракту `operator-valkey-metrics`.

#### Scenario: Снимок с недоступным CPU

- **WHEN** административный клиент записывает допустимый снимок с `cpuMillicores=null` и микросекундами в `collectedAt`
- **THEN** Kubernetes принимает снимок и возвращает исходное время с микросекундами

#### Scenario: Повтор номера ноды

- **WHEN** клиент пытается записать два элемента `status.metrics` с одним `ordinal`
- **THEN** схема CRD отклоняет изменение

#### Scenario: Недопустимый показатель

- **WHEN** снимок содержит отрицательный счётчик или неизвестную роль
- **THEN** схема CRD отклоняет изменение

## RENAMED Requirements

- FROM: `### Requirement: CR принимает последние снимки метрик нод`
- TO: `### Requirement: Ресурс принимает последние снимки метрик узлов`
