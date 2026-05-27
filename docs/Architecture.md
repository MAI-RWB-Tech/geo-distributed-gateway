# Архитектура

Документ описывает geo-distributed-gateway сверху вниз: что это за система,
какие у неё контейнеры, как через неё проходят данные и где какое состояние
живёт. Рассчитан на чтение целиком человеком, который никогда не открывал
этот репозиторий.

Связанные документы:

- HTTP wire-контракты — [`openapi.yaml`](openapi.yaml)
- Обоснования архитектурных решений — [`ADR.md`](ADR.md)
- Функциональные и нефункциональные требования — [`Requirements.md`](Requirements.md)
- Операционные процедуры — [`Runbook.md`](Runbook.md)

## 1. Что это

HTTP-шлюз на два дата-центра, который выставляет наружу набор stub-бэкендов
через одну публичную точку входа и адаптивно балансирует кросс-зональную
нагрузку на основе наблюдаемой latency / error rate.

| Параметр | Значение |
|---|---|
| Топология | 2 ДЦ (`zone1`, `zone2`), 5 логических сервисов × 2 зоны = 10 бэкенд-инстансов |
| Публичный вход | `http://localhost:10000` (global Envoy) |
| Discovery | Consul, single-DC dev agent, tag-based DNS |
| Адаптивная маршрутизация | ML-эвристика на метриках Prometheus → Control Plane → Envoy Lua-фильтр (per-request weighted zone pinning) |
| Cross-DC config feed | Redis Pub/Sub (`routing:<service>`) |
| Observability | Prometheus-метрики, Grafana-дашборды, Alertmanager, Jaeger OTLP-трейсинг, telemetry-события из app stdout |
| Runtime | Docker Compose, ~21 контейнер, Go 1.26 сервисы, Envoy 1.36 data plane |

## 2. Что этим НЕ является

Сознательные границы скоупа (см. [`ADR.md`](ADR.md) §1.1):

- Нет mTLS между зонами
- Нет поддержки более 2 дата-центров
- Нет online ML-инференса (рекомендации считаются офлайн по rolling window метрик)
- Не разворачивается на Kubernetes / Nomad без переписывания
- Нет xDS gRPC discovery (Consul DNS — единственный протокол discovery)

## 3. System Context (C4 — L1)

```mermaid
flowchart LR
    user[Внешний клиент<br/>traffic-gen, curl, ...]
    op[Оператор<br/>Grafana / Jaeger UI / Consul UI]

    subgraph gateway[geo-distributed-gateway]
        gw[Публичный HTTP-шлюз<br/>:10000]
        obs[Observability stack<br/>:3000 / :9090 / :16686]
    end

    user -- HTTP --> gw
    op -- browser --> obs
```

У системы два типа потребителей:

1. **HTTP-клиенты** ходят на один публичный порт `:10000` и адресуют
   логические сервисы префиксом URL (`/service-{a..e}/...`).
2. **Операторы** напрямую читают Grafana, Jaeger и Consul UI.

## 4. Container View (C4 — L2)

```mermaid
flowchart TB
    client[HTTP-клиент]

    subgraph global[global-net]
        ge[global-envoy<br/>:10000]
        consul[(Consul<br/>:8500/:8600)]
        redis[(Redis<br/>:6379)]
        jaeger[Jaeger<br/>:16686/:4318]
        prom[Prometheus<br/>:9090]
        grafana[Grafana<br/>:3000]
        alert[Alertmanager<br/>:9093]
        ec[events-collector<br/>:9100]
        ml[ml-analyzer<br/>:9200]
        cp[control-plane<br/>:9300]
    end

    subgraph zone1[zone1-net]
        ze1[zone1-envoy]
        s1a[service-a-zone1-1]
        s1b[service-b-zone1-1]
        s1c[service-c-zone1-1]
        s1d[service-d-zone1-1]
        s1e[service-e-zone1-1]
    end

    subgraph zone2[zone2-net]
        ze2[zone2-envoy]
        s2a[service-a-zone2-1]
        s2b[service-b-zone2-1]
        s2c[service-c-zone2-1]
        s2d[service-d-zone2-1]
        s2e[service-e-zone2-1]
    end

    client --> ge
    ge --> ze1
    ge --> ze2
    ge -. веса .-> cp
    ze1 --> s1a & s1b & s1c & s1d & s1e
    ze2 --> s2a & s2b & s2c & s2d & s2e

    s1a & s1b & s1c & s1d & s1e -. register .-> consul
    s2a & s2b & s2c & s2d & s2e -. register .-> consul
    ze1 & ze2 -. DNS lookup .-> consul

    s1a & s1b & s2a & s2b -. OTLP spans .-> jaeger
    ge -. OTLP spans .-> jaeger

    ec -. docker logs .-> s1a & s2a
    prom -. scrape .-> ge & ze1 & ze2 & ec & ml & cp & alert
    ml -. PromQL .-> prom
    cp -. pull recs .-> ml
    cp -. publish .-> redis
    redis -. subscribe .-> s1a & s2a

    grafana -. query .-> prom
    alert <-. alerts .-> prom
```

(Стрелки к `s1a` / `s2a` представляют такую же связку для каждого `service-{a..e}`-инстанса.)

### Контейнеры, по одной строке на каждый

| Контейнер | Роль |
|---|---|
| `global-envoy` | Публичный L7-вход: path-route `/service-{X}/*` в `geo_cluster`; Lua-фильтр проставляет `x-geo` zone-pin per request по весам из `control-plane`; `outlier_detection` на zone-endpoints |
| `zone{1,2}-envoy` | Зональный L7: `prefix_rewrite: "/"`, резолвит апстримы через Consul DNS (tag-filtered), per-service кластеры с health checks |
| `service-{a..e}-zone{1,2}-1` | Stub Go HTTP-сервис на `:8080`; при старте сам регистрируется в Consul; пишет JSON-телеметрию в stdout; инструментирован OTel; подписан на routing-hints через Redis |
| `consul` | Single-DC dev-mode service registry + DNS resolver; статический IP `172.30.{0,1,2}.5` в каждой Docker-сети |
| `redis` | Pub/Sub-канал `routing:<service>` для ML-рекомендаций (без персистентности — `--save "" --appendonly no`) |
| `jaeger` | All-in-one tracing-бэкенд, принимает OTLP HTTP на `:4318`, UI на `:16686` |
| `prometheus` | Скрапит Envoy admin-эндпоинты, все кастомные сервисы, Alertmanager; 8 alert-правил |
| `grafana` | Дашборды поверх Prometheus (anonymous admin) |
| `alertmanager` | Маршрутизация алертов |
| `events-collector` | Подписан на Docker stdout контейнеров с лейблом `gateway.component=app`, парсит JSON-события телеметрии, экспонирует Prometheus-метрики на `:9100/metrics` |
| `ml-analyzer` | Офлайн-эвристика на PromQL: считает per-zone веса и рекомендации rate-limit, публикует JSON на `:9200/recommendations` |
| `control-plane` | Каждые 30s pull-ит `ml-analyzer`, держит версионированный снапшот на `:9300/config`, публикует per-service изменения в Redis |

## 5. Сети

Три Docker bridge-сети изолируют трафик:

| Сеть | Подсеть | Участники |
|---|---|---|
| `global-net` | `172.30.0.0/24` | весь observability stack, support-сервисы, `consul`/`redis` (multi-attached), `global-envoy`, оба `zone-envoy`, `jaeger` |
| `zone1-net` | `172.30.1.0/24` | `zone1-envoy`, все `service-*-zone1-1`, multi-attached `consul`/`jaeger`/`redis` |
| `zone2-net` | `172.30.2.0/24` | `zone2-envoy`, все `service-*-zone2-1`, multi-attached `consul`/`jaeger`/`redis` |

`consul`, `jaeger` и `redis` намеренно подключены ко всем трём сетям, чтобы
zone-only контейнеры (app и zone-envoy) ходили к ним без перехода через
`global-envoy`.

У `consul` фиксированные IPv4 (`172.30.{0,1,2}.5`), потому что Envoy
`dns_resolvers.address` принимает только IP-литерал, не hostname.

## 6. Поток запроса (happy path)

```mermaid
sequenceDiagram
    autonumber
    participant C as Клиент
    participant GE as global-envoy
    participant CP as control-plane
    participant ZE as zone-envoy
    participant CON as consul DNS
    participant APP as service-X-zoneN-1
    participant J as jaeger

    C->>GE: GET /service-a/ping (W3C traceparent если инструментирован)
    Note over GE: Lua on_request — cache hit берём кэш<br/>cache miss идём в control-plane
    GE-->>CP: GET /config/weights/service-a (только на cache miss, TTL 30s)
    CP-->>GE: {"zone1":0.6,"zone2":0.4}
    Note over GE: weighted random выбор зоны<br/>добавляем header x-geo=zone1
    GE->>ZE: forward с x-geo + traceparent
    Note over ZE: route по префиксу /service-a/<br/>prefix_rewrite "/"<br/>cluster service-a-zoneN-cluster
    ZE->>CON: A-lookup zoneN.service-a.service.consul на порту 8600
    CON-->>ZE: IP контейнера service-a-zoneN-1
    ZE->>APP: GET /ping
    Note over APP: otelhttp создаёт server span<br/>handler добавляет user_id, cabinet_id, zone<br/>пишет JSON-телеметрию в stdout
    APP-->>ZE: 200 + X-Served-By
    ZE-->>GE: 200
    GE-->>C: 200 + X-Served-By
    APP-->>J: OTLP span (async, батчем)
    GE-->>J: OTLP span (async, батчем)
```

Ключевые инварианты:

- App-сервисы слушают bare `/ping` и `/health`; префикс `/service-{X}/`
  снимается через `prefix_rewrite: "/"` в zone-envoy.
- Header `x-geo` имеет двух потребителей: Lua-фильтр его проставляет; уже
  существующие `header.x-geo` routes в `global-envoy.yaml` его читают.
  Явный клиентский `x-geo: zone1|zone2` обходит выбор Lua (используется в
  failure-тестах).
- Если клиент не прислал `traceparent`, server span становится корневым.

## 7. Цикл адаптивной маршрутизации

```mermaid
sequenceDiagram
    autonumber
    participant E as Envoy clusters
    participant P as Prometheus
    participant ML as ml-analyzer
    participant CP as control-plane
    participant R as Redis
    participant LUA as global-envoy Lua
    participant APP as service-X

    loop scrape
        P->>E: scrape envoy_cluster_* метрик
    end

    loop каждые interval (60s)
        ML->>P: PromQL запросы p99, rps, error_rate per cluster
        P-->>ML: time-series
        Note over ML: score = 1/(1 + p99_ms/100) * (1 - error_rate)<br/>нормализуем sum=1.0 per service<br/>rate_limit = max(zone_rps) * 1.5
        ML->>ML: кешируем Recommendations в памяти
    end

    loop каждые poll-interval (30s)
        CP->>ML: GET /recommendations
        ML-->>CP: {weights, rate_limits}
        Note over CP: DeepEqual с прошлым<br/>отличается bump version и publish<br/>совпадает skip
        CP->>R: PUBLISH в routing service-a payload
        CP->>R: PUBLISH в routing service-b payload
        Note over CP: по одному publish на сервис на каждое изменение
    end

    R-->>APP: subscriber получает RoutingHints
    Note over APP: v1 только лог

    loop на каждый request (cache miss, TTL 30s per worker)
        LUA->>CP: GET /config/weights/service-X
        CP-->>LUA: плоский {zone1, zone2}
    end
```

У снапшота два потребителя:

1. **Lua-фильтр в global-envoy** синхронно pull-ит per-service плоские веса
   с `:9300/config/weights/<svc>` на cache miss — именно это реально
   формирует трафик.
2. **App-сервисы** подписаны на Redis `routing:<service>` и сейчас только
   логируют полученные подсказки. Это v1-каркас для будущих
   self-throttling consumer-ов.

Оба пути используют один и тот же `Snapshot` (`version, updated_at, weights,
rate_limits`), который держится in-memory в `control-plane`.

## 8. Failover (отказ зоны)

```mermaid
sequenceDiagram
    autonumber
    participant C as Клиент / traffic-gen
    participant GE as global-envoy
    participant ZE1 as zone1-envoy
    participant ZE2 as zone2-envoy
    participant S1 as service-X-zone1-1
    participant S2 as service-X-zone2-1

    C->>GE: GET /service-X/ping
    GE->>ZE1: forward (Lua выбрал zone1)
    ZE1->>S1: GET /ping
    S1--xZE1: connect_failure (контейнер остановлен failure-runner-ом)
    Note over ZE1: outlier_detection эжектит S1 endpoint<br/>в cluster больше нет endpoint-ов
    ZE1--xGE: 503 no_healthy_upstream
    Note over GE: retry_policy ловит connect-failure / refused-stream / gateway-error / reset<br/>num_retries = 2<br/>geo_cluster имеет zone1_envoy И zone2_envoy как endpoints
    GE->>ZE2: retry на zone2_envoy endpoint
    ZE2->>S2: GET /ping
    S2-->>ZE2: 200
    ZE2-->>GE: 200
    GE-->>C: 200 + X-Served-By service-X-zone2-1
```

Работает за счёт связки трёх вещей:

- `geo_cluster` в `global-envoy.yaml` содержит **оба** zone-envoy как
  endpoints с `outlier_detection` — когда один эжектится, кластер
  автоматически перенаправляет на оставшийся (в отличие от
  `weighted_clusters`, который при retry заново выбрал бы тот же кластер
  — см. ADR).
- `retry_policy` разрешает retry на другом endpoint.
- Валидация: `make failure-zone1` останавливает все контейнеры zone1 на 30s
  и проверяет, что error rate во время отказа остаётся ниже 5%.

## 9. Самостоятельная регистрация сервисов

```mermaid
sequenceDiagram
    autonumber
    participant SVC as service-X-zoneN-1
    participant C as Consul agent

    Note over SVC: на старте читаем env SERVICE_NAME, ZONE, CONSUL_ADDR<br/>определяем свой IP через net.InterfaceAddrs()
    SVC->>C: agent.ServiceRegister(name=service-X, id=service-X-zoneN-1,<br/>tags=[zoneN], address=own IP, port=8080,<br/>check=HTTP /health каждые 5s)
    C-->>SVC: ok
    Note over SVC: serve HTTP /ping и /health
    Note over SVC: получен SIGTERM
    SVC->>C: agent.ServiceDeregister(service-X-zoneN-1)
    C-->>SVC: ok
    Note over SVC: srv.Shutdown с таймаутом 15s
```

Best-effort: любая ошибка (Consul недоступен, ошибка регистрации) логируется,
сервис продолжает обслуживать HTTP. Это предотвращает каскадный рестарт
всех 10 app-контейнеров при флапе Consul.

## 10. Внутреннее устройство компонент (избранное)

### 10.1 control-plane state

```mermaid
classDiagram
    class Snapshot {
        +int Version
        +time.Time UpdatedAt
        +map[string]map[string]float64 Weights
        +map[string]RateLimit RateLimits
    }
    class RateLimit {
        +int RPS
    }
    class state {
        -atomic.Value snap
        -atomic.Int64 lastSuccess
        +get() Snapshot
        +set(Snapshot)
    }
    state --> Snapshot : holds
    Snapshot --> RateLimit : per service
```

- `Snapshot` хранится в `atomic.Value` — читатели (HTTP-хендлеры,
  Redis-publisher) никогда не блокируются на poll-горутине.
- `Version` инкрементируется только при изменении содержимого
  (`reflect.DeepEqual` по weights+limits); идентичный pull не двигает её.
- `lastSuccess` (UnixNano gauge) питает и freshness в `/healthz`, и
  метрику `control_plane_age_seconds`.

### 10.2 эвристика ml-analyzer

Для каждой пары `(service, zone)` каждый `-interval`:

```text
p99_ms = histogram_quantile(0.99, rate(envoy_cluster_upstream_rq_time_bucket[5m]))
rps    = sum(rate(envoy_cluster_upstream_rq_total[5m]))
err    = sum(rate(envoy_cluster_upstream_rq_xx{class="5"}[5m]))

score   = 1 / (1 + p99_ms/100) * (1 - error_rate)
weights = normalise(scores) per service, так чтобы sum(zone1, zone2) == 1.0
rps_rec = max(zone_rps_per_service) * 1.5   # fallback: 100
```

Никакого обучения моделей, никакой исторической retention — чистая
арифметика на rolling-window.

### 10.3 Lua-фильтр score

Per-worker state (обычно 2-4 воркера у Envoy):

```text
cache[service] = { weights = {zone1, zone2}, exp = epoch_seconds }
TTL_SECONDS = 30
```

На запрос:

```text
если клиент прислал x-geo:zone1|zone2 -> уважаем, return
service = распарсить из :path (default service-a)
weights = cache[service] если свежий, иначе httpCall(control-plane), кешируем только при успехе
zone    = weighted random
add x-geo header
```

Cache miss стоит один синхронный `httpCall` (~5ms p99); cache hit — это
in-process Lua-table lookup.

## 11. Состояние / хранилища

| Где | Что | Время жизни | Персистентность |
|---|---|---|---|
| `control-plane` in-memory `atomic.Value` | Последний `Snapshot{version, weights, rate_limits}` | Процесс | Нет — рестарт обнуляет `version` |
| `consul` (dev mode) | Каталог сервисов | Процесс | Нет — `-dev` всё в памяти |
| `redis` | Канал `routing:<service>` | Per message | Нет — `--save "" --appendonly no` |
| `prometheus` | TSDB на 7d | 7 дней | Диск (volume) |
| `jaeger` (all-in-one) | Недавние spans | In-memory cap | Нет |
| `app`-сервисы | Нет (stateless) | n/a | n/a |
| Envoy admin / clusters | Health endpoint-ов | Процесс | Нет — пересчитывается каждые `dns_refresh_rate=5s` |

Ничего на пути запроса не персистентно — by design, это маршрутизирующий
слой, а не system of record.

## 12. Observability fabric

```mermaid
flowchart LR
    subgraph signals[Источники сигналов]
        envoy_admin[Envoy admin /stats]
        app_telemetry[App stdout JSON Lines]
        otel_traces[OTel spans из app + traffic-gen]
        self_metrics[Self-метрики ec / ml / cp]
    end

    envoy_admin --> prom[Prometheus]
    app_telemetry --> ec[events-collector] --> prom
    self_metrics --> prom
    otel_traces --> jaeger[Jaeger]

    prom --> grafana[Grafana]
    prom --> alert[Alertmanager]
```

Три независимых пайплайна:

1. **Метрики** (Prometheus pull): Envoy admin-эндпоинты + self-метрики
   кастомных сервисов + агрегации events-collector из app stdout.
2. **Трейсы** (OTel push): traffic-gen и app-сервисы экспортируют OTLP HTTP
   в Jaeger, включая распространение W3C trace context между хопами.
3. **Логи**: `slog` JSON в stderr оставляем для `docker logs`; telemetry
   события на stdout читает только events-collector (намеренное разделение
   — см. конвенции «Logging & telemetry»).

## 13. Дисциплина кардинальности

`events-collector` и `control-plane` намеренно держат label set
Prometheus-метрик маленьким:

| Метрика | Лейблы | Max series |
|---|---|---|
| `events_total` | `service, zone, kind` | 5 × 2 × 4 = 40 |
| `requests_total` | `service, zone, status_class` | 5 × 2 × 4 = 40 |
| `errors_total` | `service, zone` | 5 × 2 = 10 |
| `request_latency_ms` | `service, zone` (histogram, 11 buckets) | ~110 |
| `control_plane_redis_publishes_total` | `service, result` | 5 × 2 = 10 |

`user_id` и `cabinet_id` сознательно НЕ являются Prometheus-лейблами —
они идут только в Jaeger span attributes. Это удерживает суммарную
кардинальность всего шлюза существенно ниже 500 серий.

## 14. Сборка и запуск

| Действие | Команда |
|---|---|
| Запуск | `make up` |
| Остановка | `make down` |
| Статус | `make status` |
| Smoke-нагрузка (100 RPS × 30s) | `make traffic-gen` |
| Хаос-тест: отказ zone1 | `make failure-zone1` |
| Частичный отказ (половина zone1) | `make failure-partial` |

Каждый Go-бинарь собирается в собственном multi-stage Dockerfile с inline
`go.work`, чтобы локальная директива `replace ../../sdk` резолвилась без
обращения к сети. См. `app/Dockerfile` / `cmd/*/Dockerfile`.

## 15. Что читать дальше

| Если хочется разобраться в… | Читать |
|---|---|
| Почему каждая деталь сделана так, как сделана | [`ADR.md`](ADR.md) |
| Что система должна делать / NFR | [`Requirements.md`](Requirements.md) |
| Как эксплуатировать и дебажить | [`Runbook.md`](Runbook.md) |
| Точные формы HTTP request / response | [`openapi.yaml`](openapi.yaml) |
| Как поднять локально | [`../README.md`](../README.md) |
