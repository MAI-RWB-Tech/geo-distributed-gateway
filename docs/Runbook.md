# Runbook: Geo-Distributed Gateway

> Операционное руководство для дежурного инженера.
> Читатель — ты в 3 ночи. Без предисловий.

---

## Быстрая навигация

| Симптом | Раздел |
|---|---|
| Алерт: error rate > 5% | [→ Высокий процент ошибок](#1-высокий-процент-ошибок-5xx) |
| Алерт: failover_triggered | [→ Failover сработал](#2-failover-сработал-цод-1-деградировал) |
| Алерт: consul_wan_partition | [→ Consul WAN partition](#3-consul-wan-partition) |
| Оба ЦОД недоступны | [→ Полный отказ](#4-полный-отказ-оба-цод-недоступны) |
| p99 latency > 500ms | [→ Высокая задержка](#5-высокая-задержка) |
| Redis недоступен | [→ Config feed упал](#6-redis-config-feed-недоступен) |
| Нужен откат | [→ Rollback](#7-rollback) |

---

## Контакты и дашборды

| | |
|---|---|
| Grafana | http://localhost:3000/d/traffic-geo |
| Consul DC1 | http://localhost:8500 |
| Consul DC2 | http://localhost:8501 |
| Envoy admin DC1 | http://localhost:9901 |
| Envoy admin DC2 | http://localhost:9902 |
| Jaeger | http://localhost:16686 |
| Канал алертов | #traffic-alerts |
| Инциденты | #incidents |
| Ответственный Consul | Влад |
| Ответственный Envoy | Юра |
| Ответственный Observability | Антон |

---

## Нормальные значения метрик

| Метрика | Норма | Алерт |
|---|---|---|
| Error rate (5xx) | < 1% | > 5% в течение 2 минут |
| Latency p99 | < 200ms | > 500ms |
| Request rate per DC | 500 ± 200 RPS | < 100 или > 1200 RPS |
| Consul health-check failures | 0 | > 3 сервиса в одном ЦОД |
| Envoy config update latency | < 1s | > 5s |
| Время failover | < 30s | — |

---

## 1. Высокий процент ошибок (5xx)

**Признак:** алерт `error_rate > 5%` или Grafana показывает красный error rate.

### Шаг 1 — Локализуй проблему

```bash
# Проверь состояние всех контейнеров
docker-compose ps

# Посмотри на Envoy — куда идут запросы
curl -s http://localhost:9901/clusters | grep -E "(health|cx_active|rq_error)"

# То же для DC2
curl -s http://localhost:9902/clusters | grep -E "(health|cx_active|rq_error)"
```

Открой Grafana → панель **"Error Rate by Service"** — найди конкретный сервис с ошибками.

### Шаг 2 — Проверь Consul

```bash
# Список деградировавших сервисов в DC1
curl -s http://localhost:8500/v1/health/state/critical | jq '.[].ServiceName'

# То же для DC2
curl -s http://localhost:8501/v1/health/state/critical | jq '.[].ServiceName'
```

### Шаг 3 — Действие по результату

**Если один сервис в DC1 упал → failover должен был сработать автоматически.**
Проверь, что Envoy перенаправил трафик (см. раздел 2).

**Если сервис упал в обоих ЦОД:**
```bash
# Перезапусти конкретный сервис (замени service-a на нужный)
docker-compose restart service-a-dc1 service-a-dc2

# Проверь логи
docker-compose logs --tail=50 service-a-dc1
```

**Если Envoy не отвечает:**
```bash
docker-compose restart envoy-dc1
# Дождись готовности (~10с), проверь
curl -s http://localhost:9901/ready
```

---

## 2. Failover сработал (ЦОД-1 деградировал)

**Признак:** алерт `failover_triggered`, Grafana показывает смещение трафика в DC2.

> Это ожидаемое поведение. Твоя задача — подтвердить корректность переключения и восстановить DC1.

### Шаг 1 — Подтверди, что failover работает корректно

```bash
# Error rate должен быть < 5% (трафик идёт через DC2)
curl -s http://localhost:9901/stats | grep "upstream_rq_5xx"

# Проверь, что DC2 принимает нагрузку
curl -s http://localhost:9902/stats | grep "upstream_rq_total"
```

Grafana → **"Traffic Distribution by DC"** — DC2 должен показывать ~100% трафика.

### Шаг 2 — Найди причину отказа DC1

```bash
# Какие сервисы упали в DC1?
curl -s http://localhost:8500/v1/health/state/critical | jq '.[].ServiceName'

# Логи упавшего сервиса
docker-compose logs --tail=100 <service-name>-dc1

# Состояние контейнеров DC1
docker-compose ps | grep dc1
```

### Шаг 3 — Восстанови DC1

```bash
# Перезапусти упавшие сервисы
docker-compose start <service-name>-dc1

# Дождись, пока Consul зафиксирует восстановление (~10с после health-check)
watch -n 2 'curl -s http://localhost:8500/v1/health/state/critical | jq length'
# Должно вернуть 0
```

### Шаг 4 — Проверь failback

После восстановления DC1 Envoy автоматически начнёт возвращать трафик.
Grafana → **"Traffic Distribution by DC"** — ожидай плавное возвращение к ~50/50.

> Резкого переключения нет — это нормально. Смотри на error rate: если < 1%, всё хорошо.

---

## 3. Consul WAN Partition

**Признак:** алерт `consul_wan_partition`, ЦОДы потеряли связь по WAN gossip.

**Последствие:** failover не сработает автоматически, каждый ЦОД работает автономно.

### Шаг 1 — Подтверди partition

```bash
# Проверь статус WAN federation
curl -s http://localhost:8500/v1/catalog/datacenters
# Должно вернуть ["dc1","dc2"]. Если только ["dc1"] — partition подтверждён.

# Проверь члены кластера
docker-compose exec consul-dc1 consul members -wan
```

### Шаг 2 — Проверь сетевую связность

```bash
# Пинг между Consul нодами
docker-compose exec consul-dc1 ping consul-dc2
```

### Шаг 3 — Перезапусти gossip

```bash
# Если сеть восстановилась, принудительно переподключи
docker-compose exec consul-dc1 consul join -wan <consul-dc2-ip>

# Проверь
curl -s http://localhost:8500/v1/catalog/datacenters
```

**Если сеть не восстанавливается** — проблема в Docker network. Перезапусти:
```bash
docker-compose down
docker-compose up -d
```

> При WAN partition > 60 секунд — звони SRE-команде.

---

## 4. Полный отказ (оба ЦОД недоступны)

**Признак:** error rate → 100%, `/ready` на обоих Envoy не отвечает.

### Шаг 1 — Быстрая диагностика

```bash
docker-compose ps

# Проверь ресурсы хоста
df -h          # диск
free -h        # память
docker stats --no-stream  # CPU/MEM по контейнерам
```

### Шаг 2 — Полный перезапуск

```bash
docker-compose down
docker-compose up -d

# Ждёшь ~30с, проверяешь
curl -s http://localhost:9901/ready  # должен вернуть "LIVE"
curl -s http://localhost:9902/ready
```

### Шаг 3 — Проверь порядок старта

Consul должен стартовать раньше Envoy. Если Envoy не получил конфиг:
```bash
docker-compose restart envoy-dc1 envoy-dc2
```

> Клиенты получат 503 пока система недоступна. После восстановления запросы пойдут автоматически.

---

## 5. Высокая задержка

**Признак:** алерт `latency_p99 > 500ms`.

### Шаг 1 — Найди медленный компонент

Grafana → **"Latency by Service"** — найди сервис с аномалией.

```bash
# Посмотри на очереди в Envoy
curl -s http://localhost:9901/stats | grep "pending"

# Проверь активные соединения
curl -s http://localhost:9901/stats | grep "cx_active"
```

### Шаг 2 — Jaeger: найди медленные трейсы

Jaeger UI → http://localhost:16686
Service: `envoy` → Sort by "Longest First" → смотри, на каком span висит время.

### Шаг 3 — Действие по результату

| Симптом в Jaeger | Причина | Действие |
|---|---|---|
| Долгий span на upstream | Сервис перегружен | Проверь CPU контейнера, перезапусти |
| Долгий span на retry | Нестабильный сервис | Проверь health-check, Consul |
| Долгий span на Consul lookup | Consul перегружен | Перезапусти `consul-dc1` |

```bash
# CPU/MEM конкретного контейнера
docker stats service-a-dc1 --no-stream

# Перезапуск конкретного сервиса
docker-compose restart <service-name>-dc1
```

---

## 6. Redis (Config Feed) недоступен

**Признак:** логи Envoy/ML содержат `redis connection refused`; алерт в Prometheus.

**Последствие:** некритично — Envoy работает на последней загруженной конфигурации. ML-рекомендации не применяются.

### Шаг 1 — Проверь Redis

```bash
docker-compose ps redis

docker-compose exec redis redis-cli ping
# Ожидаемый ответ: PONG
```

### Шаг 2 — Перезапусти

```bash
docker-compose restart redis

# Убедись, что ML снова подключился
docker-compose logs --tail=20 ml-module | grep -i redis
```

> Пока Redis недоступен, конфигурация Envoy не меняется — система работает стабильно.

---

## 7. Rollback

**Триггеры для немедленного отката:**
- Error rate > 10% в течение 5 минут
- p99 latency > 1000ms для > 50% запросов
- Consul WAN partition > 60 секунд
- Потеря > 5% запросов во время failover

### Быстрый откат через feature flag (< 2 минут)

```bash
GEO_FAILOVER_ENABLED=false docker-compose up -d envoy-dc1 envoy-dc2
```

### Полный откат к предыдущей версии (< 15 минут)

```bash
# 1. Откат кода
git log --oneline -5       # найди нужный коммит
git revert HEAD            # или конкретный хэш

# 2. Пересобери и подними
docker-compose down
docker-compose up -d

# 3. Проверь
curl -s http://localhost:9901/ready
curl -s http://localhost:8500/v1/health/state/critical | jq length  # должно быть 0

# 4. Уведоми в #incidents
```

---

## 8. Общие команды диагностики

```bash
# Состояние всех контейнеров
docker-compose ps

# Логи компонента (live)
docker-compose logs --tail=100 -f <service-name>

# Все сервисы в Consul (DC1)
curl -s http://localhost:8500/v1/catalog/services | jq 'keys'

# Health-статус всех сервисов DC1
curl -s http://localhost:8500/v1/health/state/any | jq '.[] | {name: .ServiceName, status: .Status}'

# Envoy clusters и их статус
curl -s http://localhost:9901/clusters

# Envoy конфигурация (типы секций)
curl -s http://localhost:9901/config_dump | jq '.configs[] | .["@type"]'

# Статистика запросов Envoy
curl -s http://localhost:9901/stats | grep -E "(rq_total|rq_5xx|rq_timeout)"

# Перезапуск только Envoy (без даунтайма сервисов)
docker-compose restart envoy-dc1 envoy-dc2
```

---

## 9. После инцидента

1. Убедись, что error rate < 1% на Grafana.
2. Убедись, что оба ЦОД видят друг друга: `curl -s http://localhost:8500/v1/catalog/datacenters` → `["dc1","dc2"]`.
3. Убедись, что Consul показывает 0 critical: `curl -s http://localhost:8500/v1/health/state/critical | jq length`.
4. Напиши в #incidents краткий отчёт: что сломалось, что сделал, результат.
5. Post-mortem в течение 48 часов — собери команду.
