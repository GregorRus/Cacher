# Cacher

A cache server, solution of Uma.Challenge Highload Dev task.
This project is written in golang, all source code is contained in
the Cache.go file.

Кэш сервер, решение задачи Uma.Chellenge Highload Dev.
Этот проект написан на golang, весь исходный код содержится в файле
Cacher.go.

## Build / Сборка

    go build

## Execution / Запуск

    Cacher -proc 1 -port 80 -upstream localhost:8080

### Flags / Флаги

* `-proc` *(Default: 1)* — Count of logical CPUs using by go runtime,
0 mean count of all available logical CPUs. / Количество логических 
CPU, используемых рантаймом go, 0 означает количество всех 
доступных логических CPU.
* `-port` *(Required/Обязательный)* — Cache server listening port. / Порт, покорому кэш сервер ожидает запросы.
* `-upstream` *(Required/Обязательный)* — Host address and port of upstream web service. / Адресс и порт вышестоящего веб сервиса.

#### Logging / Логирование

* `-log-prefix` *(Default: "logs/")* — Logs path prefix or "off" to disable logging. / Префикс пути логов или "off", чтобы отключить логирование.
* `-access-log` *(Default: "access.log")* — Access log file path or "off" to disable logging. / Путь лога запросов к кэш серверу или "off", чтобы отключить логирование.
* `-error-log` *(Default: "error.log")* — Error log file path or "off" to disable logging. / Путь лога ошибок, произошедших во время обработки запросов, или "off", чтобы отключить логирование.
* `-cache-log` *(Default: "cache.log")* — Cache changes log file path or "off" to disable loggin. / Путь лога изменений кэша или "off", чтобы отключить логирование.
* `-upstream-log` *(Default: "upstream.log")* — Upstream access log file path or "off" to disable logging. / Путь лога доступа к вышестоящему серверу или "off", чтобы отключить логирование.
