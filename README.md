# Cacher

A cache server, solution of Uma.Challenge Highload Dev task.
This project is written in golang, all source code is contained in the Cache.go file.

Кэш сервер, решение задачи Uma.Chellenge Highload Dev.
Этот проект написан на golang, весь исходный код содержится в файле Cacher.go.

## Build / Сборка

    go build

## Execution / Запуск

    Cacher -proc 1 -port 80 -upstream localhost:8080

### Flags / Флаги

* `-proc` *(Optional/Необязательный)* — Count of logical CPUs using by go runtime, 0 mean count of all available logical CPUs. / Количество логических CPU, используемых рантаймом go, 0 означает количество всех доступных логических CPU.
* `-port` *(Required/Обязательный)* — Cache server listening port. / Порт, покорому кэш сервер ожидает запросы.
* `-upstream` *(Required/Обязательный)* — Host address and port of upstream web service. / Адресс и порт основного веб сервиса.
