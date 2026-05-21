# Decision Log: Geo-Distributed Gateway

| Поле | Значение |
|---|---|
| **Проект** | Geo-Distributed Gateway |
| **Команда** |  |
| **Обновлено** | 2026-04-05 |

---

## Журнал решений

| ID | Дата | Решение | Контекст / почему | Кто принял | Статус |
|---|---|---|---|---|---|
| DL-001 | 2026-04-05 | HTTP вместо gRPC для клиентского SDK и traffic-gen | DoD упоминает gRPC, но app-сервис реализует HTTP на :8080. Реализация gRPC без proto-схемы и серверного кода не имеет смысла. Функциональность (заголовки, корреляция, тайминг) полностью покрыта HTTP. | @W_F_A_I_H | ✅ Действует |
| DL-002 | 2026-04-05 | Один `geo_cluster` с двумя endpoints вместо `weighted_clusters` | `weighted_clusters` выбирает кластер один раз на запрос — retry при 5xx идут обратно в ту же упавшую зону. Один кластер с обоими zone-envoy как endpoints + outlier detection даёт cross-zone failover на уровне retry: когда endpoint zone1 эжектируется, все retries и новые запросы автоматически уходят в zone2. | @W_F_A_I_H | ✅ Действует |
| DL-003 | 2026-04-05 | `ENVOY_UID=0` вместо override entrypoint в docker-compose | Rootless Podman не может сделать `chown /dev/stdout`, что приводило к краху Envoy-контейнеров при старте. Первоначально исправлено переопределением `entrypoint`, но официальный entrypoint-скрипт Envoy проверяет `ENVOY_UID != 0` и пропускает chown-блок при `ENVOY_UID=0`. Это документированный механизм — безопаснее и совместимее, чем bypass entrypoint. | @W_F_A_I_H | ✅ Действует |
| DL-004 | 2026-04-05 | `FROM alpine:3.20` вместо `FROM scratch` для app Dockerfile | `FROM scratch` не содержит `wget`, который требуется для healthcheck-команды в docker-compose (`wget -qO- http://localhost:8080/health`). Alpine добавляет ~8 МБ, но даёт shell и wget без дополнительных усилий. | @W_F_A_I_H | ✅ Действует |
| DL-005 | 2026-04-05 | Inline `go.work` в Dockerfile вместо копирования корневого | Корневой `go.work` включает `cmd/traffic-gen` и `cmd/failure-runner`, которые не копируются в Docker context app-сервиса. Генерация минимального `go.work` внутри `RUN`-шага (`printf 'go 1.22\n\nuse (\n\t./app\n\t./sdk\n)\n'`) изолирует сборку от несуществующих модулей и не требует расширения Docker context. | @W_F_A_I_H | ✅ Действует |
| DL-006 | 2026-04-05 | `locality_weighted_lb_config` + locality labels вместо плоского round-robin | Без `locality_weighted_lb_config` Envoy игнорирует locality-метки и делает плоский round-robin. С этим флагом трафик распределяется пропорционально `load_balancing_weight` (оба = 1 → 50/50). При эжекции всех endpoints одной locality Envoy автоматически переключает 100% трафика на оставшуюся locality. | @W_F_A_I_H | ✅ Действует |
| DL-007 | 2026-04-05 | Delay injection (задержка) оставлена за скоупом failure-runner | DoD упоминает "задержка" как метод деградации. Инъекция latency через `tc netem` требует NET_ADMIN capability в контейнере; через Envoy fault filter — изменений в Envoy-конфигах всех зон и перезагрузки. Реализованные сценарии `stop` и `pause` достаточны для демонстрации failover. | @W_F_A_I_H | ✅ Действует |
| DL-008 | 2026-04-05 | `FROM docker:27-cli` для failure-runner вместо scratch/alpine | failure-runner управляет контейнерами через Docker CLI (`docker stop`, `docker pause`). Образ `docker:27-cli` содержит docker-клиент без демона — минимальная зависимость. Альтернатива через Docker API напрямую усложнила бы код без выгоды. | @W_F_A_I_H | ✅ Действует |
| DL-T1-001 | 2026-05-21 | Tag-based Consul DNS (`zoneN.<svc>.service.consul`) вместо xDS gRPC discovery | UC-02 в Requirements называет «xDS API», но ADR §5 закрепляет только связку «Envoy + Consul» без обязательства по протоколу discovery. xDS-сервер потребовал бы либо consul-template (отдельный sidecar, лишний failure-mode), либо consul-connect (тянет mTLS, который сам по себе Out-of-Scope). Tag-based DNS — нативный механизм Consul: leading `<tag>.` префикс в `*.service.consul` фильтрует SRV-ответы по тегу. Работает поверх single-DC dev-агента, попадает в TTL-окно ~5s через `respect_dns_ttl` в Envoy-кластере. NOTE-комментарий зафиксирован в начале `envoy/zone1-envoy.yaml` и `envoy/zone2-envoy.yaml`. | T1-impl | ✅ Действует |
| DL-T1-002 | 2026-05-21 | Best-effort регистрация в Consul: сервис не падает при недоступном агенте | Стандартный путь — `log.Fatal` при ошибке `client.Agent().ServiceRegister(...)`, тогда контейнер не пройдёт healthcheck и compose отметит depends_on невыполненным. Но в реальности это создаёт хрупкую цепочку: переинициализация Consul (например, при тестах failure-runner) убьёт ВСЕ 10 app-контейнеров. Решение: при пустом `CONSUL_ADDR`, ошибке `NewClient` или ошибке `ServiceRegister` — `slog.Warn` и продолжаем работу. HTTP-сервер слушает `:8080`, healthcheck `/health` отвечает 200, Envoy просто не увидит этот instance в Consul DNS и пропустит его. Trade-off: можно проспать конфигурационный баг (`CONSUL_ADDR` опечатка → silent degrade), но это перевешивается устойчивостью к рестарту control-plane Consul. | T1-impl | ✅ Действует |

---

## Открытые вопросы

| ID | Вопрос | Кто должен решить | Дедлайн |
|---|---|---|---|
| Q-001 | Нужно ли реализовать delay injection (tc netem / Envoy fault filter) для полноты сценариев failure-runner? |  | — |
| Q-002 | Переходить ли на gRPC когда app-сервис получит proto-схему? SDK-клиент потребует замены транспортного слоя. |  | При реализации gRPC сервера |
| Q-T1-001 | `app/go.sum` не сгенерирован — окружение исполнителя T1 не имело локального Go и доступа к `proxy.golang.org`, а `app/Dockerfile` запрещено менять (Out-of-Scope T1). Перед первым `make up` нужно вручную выполнить `cd app && go mod tidy`. Альтернатива: вшить `RUN go mod tidy` в `app/Dockerfile` шагом отдельной задачи. | maintainer репо | до первого `make up` после merge T1 |
| Q-T1-002 | `docs/Runbook.md` содержит ~7 ссылок на старые имена контейнеров `app-zoneN-X` (строки 92, 95, 132, 142, 272, 275, 349). T1 их не правил — Runbook не указан в `files` плана, обновление runbook'а вне scope. Нужно завести отдельную задачу или PR на синхронизацию Runbook с новыми именами `service-{a..e}-zone{1,2}-1`. | DocOps / автор Runbook'а | до следующего релизного цикла |
