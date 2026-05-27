# Geo-Distributed Gateway

HTTP-шлюз на два дата-центра в Docker Compose: 10 stub-сервисов, Envoy в роли data plane, Consul для discovery, ML-эвристика для адаптивной маршрутизации между зонами.

## Требования

- Docker 25+ (или совместимый)
- GNU Make
- Свободные TCP-порты: 3000, 4318, 6379, 8500, 9090, 9093, 9100, 9200, 9300, 10000, 16686, 19000-19002
- Свободные Docker-подсети: 172.30.0.0/24, 172.30.1.0/24, 172.30.2.0/24

## Быстрый старт

```bash
make up           # собрать и поднять весь стек (~21 контейнер)
make status       # docker compose ps
make traffic-gen  # нагрузка 100 RPS x 30s (smoke-тест)
make down         # остановить и удалить
```

## Эндпоинты

| Сервис | Порт | Что |
|---|---|---|
| Global Envoy (публичный) | 10000 | `GET /service-{a..e}/ping`, `GET /ping` |
| Consul UI | 8500 | Каталог сервисов |
| Jaeger UI | 16686 | Распределённые трейсы |
| Prometheus | 9090 | Метрики и алерты |
| Grafana | 3000 | Дашборды (anonymous admin) |
| Alertmanager | 9093 | Маршрутизация алертов |
| Events Collector | 9100 | `/metrics`, `/healthz` |
| ML Analyzer | 9200 | `/recommendations`, `/healthz`, `/metrics` |
| Control Plane | 9300 | `/config`, `/config/weights/{service}`, `/healthz`, `/metrics` |
| Envoy admin | 19000 (global), 19001 (zone1), 19002 (zone2) | `/stats`, `/clusters`, `/config_dump` |

Полная спека HTTP API: [`docs/openapi.yaml`](docs/openapi.yaml).

## Возможности

- **10 stub-сервисов** на две зоны (`service-{a..e} x zone{1,2}`), каждый при старте сам регистрируется в Consul.
- **Tag-based Consul DNS discovery** (`<zone>.<service>.service.consul`) — zone-envoy резолвит апстримы через статический resolver на порт 8600.
- **Global Envoy** делает path-route `/service-{a..e}/*` в общий `geo_cluster` с `outlier_detection` для кросс-зонального failover.
- **Lua-фильтр score** в global-envoy: для каждого запроса выбирает зону взвешенным random'ом по весам из Control Plane, проставляет `x-geo` header. Кэш на воркер с TTL 30s, fallback 50/50 при недоступном CP.
- **Events Collector**: подписывается через Docker SDK на stdout контейнеров с лейблом `gateway.component=app`, парсит JSON-события телеметрии, экспонирует Prometheus-метрики с ограниченной кардинальностью (без `user_id` / `cabinet_id`).
- **ML Analyzer**: эвристика на PromQL (`score = 1 / (1 + p99_ms/100) * (1 - error_rate)`), публикует JSON-рекомендации на `:9200/recommendations`.
- **Control Plane**: каждые 30s опрашивает ML, держит версионированный снапшот на `:9300/config`, при изменении контента публикует в Redis `routing:<service>`.
- **Подписчик routing hints в SDK**: app-сервисы слушают `routing:<service>` и логируют полученные подсказки (v1 — только наблюдаемость).
- **OTel-трейсинг** end-to-end: `traffic-gen` → `global-envoy` → `service-X`, экспорт в Jaeger через OTLP HTTP. Server-spans содержат атрибуты `user_id`, `cabinet_id`, `zone`.
- **Хаос-инструменты**: `make failure-zone1`, `make failure-partial` имитируют отказ зоны; `make traffic-gen` — синтетическая нагрузка.

## Тестирование

```bash
# Go-тесты по модулям (через Go 1.26 контейнер, если локально Go нет):
cd sdk && go test ./...
cd app && go test ./...
cd cmd/ml-analyzer && go test ./...

# End-to-end smoke:
make up && make traffic-gen && make failure-zone1
```

## Структура репозитория

```
app/                  Stub HTTP-сервис
sdk/                  Общая библиотека (client, config, stats, telemetry)
cmd/events-collector/ Docker-логи -> Prometheus
cmd/ml-analyzer/      Prometheus -> JSON-рекомендации
cmd/control-plane/    ML -> снапшот + Redis publish
cmd/traffic-gen/      Нагрузочный генератор
cmd/failure-runner/   Хаос-инжектор / тесты failover
envoy/                3 конфига Envoy (global + zone1 + zone2) + Lua-фильтр
monitoring/           Конфиги Prometheus + Grafana + Alertmanager
docs/                 ADR, Requirements, Runbook, Architecture, OpenAPI
```

## Архитектура

[`docs/Architecture.md`](docs/Architecture.md) — обзор сверху вниз (system context, container-диаграмма, потоки запросов / failover / ML, внутреннее устройство компонент). Рассчитан на холодное чтение без онбординга.

Сопутствующие документы: [`docs/ADR.md`](docs/ADR.md) (обоснования решений), [`docs/Runbook.md`](docs/Runbook.md) (операционка), [`docs/Requirements.md`](docs/Requirements.md) (функциональные и нефункциональные требования).
