# API Reference: Geo-Distributed Gateway

| Поле | Значение |
|---|---|
| **Версия API** | v1 |
| **Base URL** | `http://localhost:10000` (локальный стенд через global-envoy) |
| **Обновлено** | 2026-04-05 |

---

## Аутентификация

**Тип:** internal (без авторизации)

Сервис предназначен для внутреннего использования. Авторизация не требуется.

---

## Общие правила

| Параметр | Значение |
|---|---|
| Формат ответа | `application/json` |
| Кодировка | UTF-8 |

### Заголовки идентификации (проставляются SDK-клиентом)

| Заголовок | Описание | Пример |
|---|---|---|
| `X-User-ID` | Идентификатор пользователя | `user-7` |
| `X-Cabinet-ID` | Идентификатор кабинета | `cabinet-3` |
| `X-Correlation-ID` | Уникальный ID запроса для трассировки | `1743840000000000000-42` |

### Заголовок маршрутизации (zone pinning)

| Заголовок | Значение | Описание |
|---|---|---|
| `X-Geo` | `zone1` или `zone2` | Принудительно направить запрос в конкретную зону. Без заголовка — locality-aware балансировка 50/50. |

### Коды ответов

| Код | Значение |
|---|---|
| 200 OK | Успех |
| 503 Service Unavailable | Зона недоступна (возникает при отказе зоны, до срабатывания outlier detection) |

---

## Эндпоинты

### GET /ping

Основной эндпоинт нагрузки. Возвращает информацию об обработавшем запрос инстансе.

**Пример запроса:**

```http
GET /ping HTTP/1.1
Host: localhost:10000
X-User-ID: user-1
X-Cabinet-ID: cabinet-5
X-Correlation-ID: 1743840000000000000-1
```

**Пример ответа (200 OK):**

```
pong: zone1-1 (zone=zone1)
```

Content-Type: `text/plain`. Заголовки ответа:

| Заголовок | Пример | Описание |
|---|---|---|
| `X-Served-By` | `zone1-1` | Имя инстанса, обработавшего запрос |
| `X-Zone` | `zone1` | Зона инстанса |

---

### GET /health

Healthcheck-эндпоинт. Используется Docker Compose `depends_on` и zone-envoy health checks.

**Пример запроса:**

```http
GET /health HTTP/1.1
Host: localhost:10000
```

**Пример ответа (200 OK):**

```json
{
  "status": "ok",
  "instance": "zone1-1",
  "zone": "zone1"
}
```

| Поле | Тип | Описание |
|---|---|---|
| `status` | string | Всегда `"ok"` при живом инстансе |
| `instance` | string | Имя инстанса |
| `zone` | string | Зона инстанса |

---

## Go SDK

### Пакет `sdk/client`

Клиент для вызова app-сервиса. Автоматически проставляет заголовки идентификации и трассировки.

```go
import "github.com/geo-distributed-gateway/sdk/client"

c := client.New(client.Options{
    BaseURL: "http://localhost:10000",
    Timeout: 5 * time.Second,
})

result, err := c.Do(ctx, "/ping", "user-1", "cabinet-5")
// result.StatusCode  — HTTP статус
// result.Latency     — время ответа
// result.CorrelationID — сгенерированный X-Correlation-ID
```

**Метод `Do`:**

| Параметр | Тип | Описание |
|---|---|---|
| `ctx` | `context.Context` | Контекст запроса (таймаут, отмена) |
| `path` | `string` | Путь эндпоинта (`/ping`, `/health`) |
| `userID` | `string` | Значение заголовка `X-User-ID` |
| `cabinetID` | `string` | Значение заголовка `X-Cabinet-ID` |

**Метод `DoWithHeaders`** — расширение `Do` с дополнительными заголовками (используется для `X-Geo` zone pinning):

```go
result, err := c.DoWithHeaders(ctx, "/ping", "user-1", "cabinet-5", map[string]string{
    "X-Geo": "zone1",
})
```

---

### Пакет `sdk/telemetry`

Публикует JSON-события в `io.Writer` (по умолчанию `stdout`).

```go
import "github.com/geo-distributed-gateway/sdk/telemetry"

col := telemetry.New(telemetry.Options{
    Service:  "app",
    Instance: "zone1-1",
    Zone:     "zone1",
})

col.Start()
col.Request("user-1", "cabinet-5", correlationID, 200, 12*time.Millisecond)
col.Error(correlationID, "timeout")
col.Stop()
```

**Формат события (JSON Lines, stdout):**

```json
{"v":1,"kind":"request","service":"app","instance":"zone1-1","zone":"zone1","user_id":"user-1","cabinet_id":"cabinet-5","correlation_id":"1743840000000000000-1","status_code":200,"latency_ms":12,"ts":"2026-04-05T10:00:00Z"}
```

| Поле | Тип | Описание |
|---|---|---|
| `v` | int | Версия схемы (всегда `1`) |
| `kind` | string | `start` / `stop` / `request` / `error` |
| `service` | string | Имя сервиса |
| `instance` | string | Имя инстанса |
| `zone` | string | Зона |
| `user_id` | string | ID пользователя (только для `request`) |
| `cabinet_id` | string | ID кабинета (только для `request`) |
| `correlation_id` | string | ID корреляции |
| `status_code` | int | HTTP статус (только для `request`) |
| `latency_ms` | int | Задержка в мс (только для `request`) |
| `error` | string | Сообщение об ошибке (только для `error`) |
| `ts` | string | Временная метка в RFC3339 |

---

### Пакет `sdk/config`

Горячая перезагрузка конфигурации из файла.

```go
import "github.com/geo-distributed-gateway/sdk/config"

w, err := config.NewWatcher("config.json", 2*time.Second)

cfg := w.Get() // текущий конфиг

ch := w.Subscribe() // получать уведомления об изменениях
go func() {
    for newCfg := range ch {
        // применить обновлённый конфиг
        _ = newCfg.RequestTimeout
    }
}()

w.Close()
```

**Структура `ServiceConfig`:**

| Поле | Тип JSON | Пример | Описание |
|---|---|---|---|
| `request_timeout` | string (duration) | `"5s"` | Таймаут одного запроса |
| `max_retries` | int | `3` | Максимальное число повторов |
| `retry_backoff` | string (duration) | `"200ms"` | Задержка между повторами |
| `zone` | string | `"zone1"` | Целевая зона |

---

### Пакет `sdk/stats`

Потокобезопасная запись latency и подсчёт перцентилей.

```go
import "github.com/geo-distributed-gateway/sdk/stats"

rec := stats.NewRecorder()
rec.Add(12*time.Millisecond, false)
rec.Add(250*time.Millisecond, true) // isError=true

snap := rec.Snapshot()
fmt.Println(snap.P50, snap.P95, snap.P99)
fmt.Println(snap.ErrorRate())
fmt.Println(snap.Report(5 * time.Second)) // "total=2 success=1 errors=1 error_rate=50.00% rps=0.40 p50=12ms p95=250ms p99=250ms"

rec.Reset() // сбросить для следующего окна
```

---

## Envoy Admin API

Используется `failure-runner` для сбора статистики.

| URL | Описание |
|---|---|
| `http://localhost:19000/stats?filter=upstream_rq_total` | Статистика global-envoy |
| `http://localhost:19001/stats?filter=upstream_rq_total` | Статистика zone1-envoy |
| `http://localhost:19002/stats?filter=upstream_rq_total` | Статистика zone2-envoy |

Порты: `19000` — global-envoy, `19001` — zone1-envoy, `19002` — zone2-envoy.
