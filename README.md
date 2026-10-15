# idempotency

[![CI](https://github.com/poseydon/idempotency/actions/workflows/ci.yml/badge.svg)](https://github.com/poseydon/idempotency/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/poseydon/idempotency.svg)](https://pkg.go.dev/github.com/poseydon/idempotency)
[![Go Report Card](https://goreportcard.com/badge/github.com/poseydon/idempotency)](https://goreportcard.com/report/github.com/poseydon/idempotency)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Идемпотентность для HTTP-сервисов на Go: middleware для `net/http`, которое выполняет запрос **один раз**, сколько бы раз клиент его ни повторил.

## Зачем это нужно

Клиент отправил `POST /payments`, сервер списал деньги, но ответ потерялся: таймаут, обрыв сети, перезапуск балансировщика. Клиент не знает, прошёл ли платёж, и повторяет запрос. Без защиты деньги списываются дважды.

Решение: клиент добавляет заголовок `Idempotency-Key` с уникальным значением. Middleware запоминает результат первого выполнения, и на повтор с тем же ключом **не вызывает хендлер**, а возвращает сохранённый ответ. Поведение совместимо с [IETF-драфтом *The Idempotency-Key HTTP Header Field*](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/) и документацией Stripe.

## Установка

```bash
go get github.com/poseydon/idempotency
```

Нужен Go 1.27 или новее. Внешних зависимостей нет.

## Быстрый старт

```go
store := idempotency.NewMemoryStore()
defer store.Close()

mux := http.NewServeMux()
mux.HandleFunc("POST /payments", createPayment)

mw := idempotency.New(store)
log.Fatal(http.ListenAndServe(":8080", mw(mux)))
```

`New` возвращает `func(http.Handler) http.Handler`, поэтому middleware подключается к любому роутеру: `chi.Use(mw)`, `gorilla/mux.Use(mux.MiddlewareFunc(mw))` и так далее.

Проверка:

```bash
curl -i -X POST localhost:8080/payments -H 'Idempotency-Key: order-1001' -d '{"amount": 500}'
# HTTP/1.1 201 Created

curl -i -X POST localhost:8080/payments -H 'Idempotency-Key: order-1001' -d '{"amount": 500}'
# HTTP/1.1 201 Created
# Idempotent-Replayed: true        <- хендлер не вызывался, ответ из хранилища

curl -i -X POST localhost:8080/payments -H 'Idempotency-Key: order-1001' -d '{"amount": 999}'
# HTTP/1.1 422 Unprocessable Entity   <- тот же ключ, но другой запрос
```

## Как это работает

| Ситуация | Что делает middleware |
|---|---|
| Метод не из списка (по умолчанию `POST` и `PATCH`) | пропускает запрос без изменений |
| Заголовка нет | пропускает запрос; с `WithRequired(true)` отвечает `400` |
| Ключ некорректный (пустой, длиннее 255 байт, не печатный ASCII) | `400` |
| Тело запроса больше лимита | `413` |
| Ключ новый | помечает ключ «в процессе», вызывает хендлер, сохраняет ответ |
| Ключ уже выполнен, запрос тот же | отдаёт сохранённый ответ и заголовок `Idempotent-Replayed: true` |
| Ключ уже выполнен, **запрос другой** (метод, URL, query или тело) | `422 Unprocessable Entity` |
| Ключ в процессе, первый запрос ещё работает | `409 Conflict` с `Retry-After: 1`; с `WithWait` ждёт результат и отдаёт его |
| Хендлер вернул `5xx` | ответ уходит клиенту, ключ освобождается, повтор выполнится заново |
| Хендлер запаниковал | ключ освобождается, паника идёт дальше в `net/http` |
| Ответ больше лимита | ответ уходит клиенту полностью, но не сохраняется, ключ освобождается |
| Хранилище недоступно | `500` («fail closed»); с `WithFailOpen(true)` запрос идёт в хендлер без защиты |
| Клиент отключился посреди запроса | хендлер доработает, результат сохранится для повтора |

Что считается «тем же запросом»: отпечаток SHA-256 от метода, пути с query и тела. Заголовки в отпечаток не входят.

### Формат ошибок

По умолчанию ошибки middleware возвращаются как JSON:

```json
{"error": "idempotency: key reused with a different request"}
```

Формат меняется через `WithErrorHandler` (см. [examples/router](examples/router)).

## Опции

| Опция | По умолчанию | Описание |
|---|---|---|
| `WithHeader(name)` | `Idempotency-Key` | имя заголовка с ключом |
| `WithMethods(...string)` | `POST`, `PATCH` | какие методы защищать (набор заменяется целиком) |
| `WithRequired(bool)` | `false` | без ключа отвечать `400` |
| `WithScope(func(*http.Request) string)` | пусто | пространство ключей, например id пользователя |
| `WithMaxRequestBody(n)` | 1 МиБ | лимит тела запроса, больше — `413` |
| `WithMaxResponseBody(n)` | 1 МиБ | ответы больше лимита не сохраняются |
| `WithCacheable(func(status int) bool)` | `status < 500` | какие ответы сохранять |
| `WithWait(d)` | выключено | ждать завершения первого запроса до `d`, а не отвечать `409` |
| `WithFailOpen(bool)` | `false` | при сбое хранилища пропускать запрос, а не отвечать `500` |
| `WithErrorHandler(f)` | JSON | свой формат ошибок |

Для `MemoryStore`:

| Опция | По умолчанию | Описание |
|---|---|---|
| `WithTTL(d)` | 24 ч | сколько хранится готовый ответ |
| `WithLockTimeout(d)` | 30 с | через сколько освобождается ключ «в процессе», если хендлер завис или процесс умер |
| `WithCleanupInterval(d)` | 1 мин | как часто фоновая горутина удаляет просроченное; `0` отключает |
| `WithMaxEntries(n)` | без лимита | максимум записей; при переполнении `503` |

Неверные значения (нулевой лимит, пустой заголовок, `nil` вместо функции) вызывают `panic` при создании: это ошибка программиста, и лучше увидеть её при старте, а не на первом запросе.

## Несколько пользователей: `WithScope`

По умолчанию ключ глобален. Если два пользователя случайно отправят одинаковый `Idempotency-Key`, один получит чужой ответ или `422`. В сервисе с несколькими пользователями **всегда** задавайте scope:

```go
mw := idempotency.New(store, idempotency.WithScope(func(r *http.Request) string {
    return userIDFromContext(r.Context()) // из проверенной аутентификации, не из заголовка клиента
}))
```

Пример: [examples/scope](examples/scope).

## Режим ожидания: `WithWait`

Без него параллельный повтор получает `409` и должен прийти позже. С `WithWait(5 * time.Second)` повтор подождёт первый запрос и вернёт его результат:

```go
mw := idempotency.New(store, idempotency.WithWait(5*time.Second))
```

Если первый запрос завершился ошибкой `5xx` или паникой, ждавший запрос становится новым исполнителем. Если за `d` ничего не произошло — `409`. Ожидание требует хранилище с интерфейсом `Waiter`; `MemoryStore` его реализует.

## Своё хранилище

`MemoryStore` хранит данные в памяти процесса: они пропадают при перезапуске и не видны другим экземплярам. Для нескольких реплик реализуйте интерфейс `Store` поверх Redis или PostgreSQL:

```go
type Store interface {
    Acquire(ctx context.Context, key, fingerprint string) (e Entry, acquired bool, err error)
    Complete(ctx context.Context, key string, resp Response) error
    Release(ctx context.Context, key string) error
}
```

Контракт:

- `Acquire` **атомарен**: из многих одновременных вызовов с одним ключом ровно один вернёт `acquired == true` (Redis: `SET NX PX`; PostgreSQL: `INSERT ... ON CONFLICT DO NOTHING`).
- Запись «в процессе» истекает сама, иначе упавший процесс заблокирует ключ навсегда.
- `Complete` сохраняет **копию** ответа; `Release` удаляет только записи «в процессе».

Рабочий пример с логированием вызовов: [examples/customstore](examples/customstore). Если хранилище умеет ждать, реализуйте ещё `Waiter`.

## Примеры

| Каталог | Что показывает |
|---|---|
| [examples/basic](examples/basic) | минимальный сервер, graceful shutdown, curl-сценарий |
| [examples/scope](examples/scope) | изоляция ключей по пользователям |
| [examples/customstore](examples/customstore) | своё хранилище |
| [examples/router](examples/router) | подключение к роутеру, `WithWait`, `WithCacheable`, свой формат ошибок |

Запуск: `go run ./examples/basic`.

## Ограничения

- **Стриминг не поддерживается**: SSE, WebSocket, `Hijack`, trailers. Не ставьте middleware перед такими хендлерами.
- **`MemoryStore` работает в одном процессе**: данные теряются при перезапуске, реплики друг друга не видят. Для нескольких инстансов нужно общее хранилище.
- Ответ целиком буферизуется в памяти (до `WithMaxResponseBody`).
- Тело запроса читается целиком (до `WithMaxRequestBody`), чтобы посчитать отпечаток.
- Идемпотентность защищает от повторов **запроса**, а не от повторов побочных эффектов внутри хендлера: если хендлер упал посередине, между «списал» и «ответил», ответ `5xx` не сохраняется, и повтор выполнится снова. Операции с внешним миром должны быть идемпотентны сами (передавайте ключ дальше платёжному провайдеру).

## Сравнение с альтернативами

- **Самописный `map` с мьютексом**: обычно ломается на гонке «проверить, затем вставить», на панике хендлера, на отключении клиента и на утечке записей. Здесь это закрыто и покрыто тестами с `-race`.
- **Готовые библиотеки с Redis**: удобны для нескольких реплик, но тянут зависимость. Здесь ядро без зависимостей, а Redis/Postgres подключаются как отдельное хранилище (см. Roadmap).
- **Идемпотентность на уровне БД** (уникальный индекс по ключу): надёжна для создания записей, но не умеет воспроизводить ответ и защищать произвольный хендлер.

## Производительность

Apple M-серии, Go 1.27, `go test -bench . -benchmem` (в цифры входит создание `httptest`-запроса и ответа):

| Бенчмарк | Время | Память | Аллокации |
|---|---|---|---|
| повтор (replay) | ~1,8 мкс | 8,3 КБ | 38 |
| новый ключ | ~3,6 мкс | 10 КБ | 55 |
| метод вне списка (pass-through) | ~0,8 мкс | 6,1 КБ | 19 |
| `MemoryStore.Acquire` (параллельно) | ~120 нс | 0 | 0 |

Накладные расходы на повтор и на новый ключ — микросекунды, на порядки меньше любой реальной операции с базой.

## Разработка

```bash
make check       # go vet + тесты с -race + golangci-lint
make test-cover  # покрытие
make bench       # бенчмарки
make fuzz        # fuzz-тесты (по 10 секунд на каждый)
make examples    # собрать и проверить примеры
```

Подробности решений — в [DESIGN.md](DESIGN.md), история изменений — в [CHANGELOG.md](CHANGELOG.md).

## Roadmap

- Хранилища `redisstore` и `pgstore` отдельными модулями (корень остаётся без зависимостей).
- Метрики и tracing (хуки на replay, mismatch, wait).
- Вытеснение старых записей в `MemoryStore` вместо отказа при `WithMaxEntries`.

## Лицензия

[MIT](LICENSE)
