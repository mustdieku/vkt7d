# Диагностические программы

В проект добавлены две отдельные программы, использующие те же `config`, `protocol` и `storage`, что и основной демон.

## 1. vkt7check — проверка ВКТ-7

Программа сначала подключается к PostgreSQL, затем открывает RS-232. Она не записывает архивные данные в БД.

Проверяет:

- PostgreSQL;
- создание/поиск устройства;
- открытие RS-232;
- начало сеанса;
- дату/время ВКТ-7;
- свойства;
- список активных элементов и их размеры;
- текущие значения;
- итоговые текущие значения;
- диапазон и чтение часового архива;
- диапазон и чтение суточного архива;
- диапазон и чтение месячного архива;
- диапазон и чтение итогового архива;
- разбор каждого ответа с выводом значения, quality, NS и raw.

Запуск с теми же параметрами, что у демона:

```bash
./vkt7check \
  --port=/dev/ttyUSB0 \
  --baud=19200 \
  --address=7 \
  --db-url='postgres://vkt7:vkt7@127.0.0.1:5432/vkt7?sslmode=disable'
```

Если БД еще не создана:

```bash
./vkt7check --migrate ...
```

При первой проблеме программа завершает тест с указанием этапа. Это сделано специально: диагностическая программа должна показать, на каком именно вызове протокола произошла ошибка.

## 2. vkt7dbtest — проверка PostgreSQL

Без `--write` выполняется безопасная проверка:

```bash
./vkt7dbtest --db-url='postgres://vkt7:vkt7@127.0.0.1:5432/vkt7?sslmode=disable'
```

Проверяется соединение, наличие схемы и основные операции с устройством.

Для полноценного теста функций записи:

```bash
./vkt7dbtest --write --migrate ...
```

Тест создает отдельное устройство с суффиксом `-dbtest`, проверяет `UpsertActive`, `SaveArchive`, `Last`, `SaveCurrent`, `CurrentLast`, `Log`, после чего удаляет тестовое устройство каскадно.

## Сборка

```bash
go mod download
go build -o vkt7d ./cmd/vkt7d
go build -o vkt7check ./cmd/vkt7check
go build -o vkt7dbtest ./cmd/vkt7dbtest
```

Важно: диагностический `vkt7check` читает архивы без записи в PostgreSQL, поэтому его можно запускать до основного демона. Однако чтение архива реально обращается к ВКТ-7 и расходует ресурс интерфейса/батареи; для первого теста лучше использовать его вручную, а не запускать циклически.


### Properties parsing

For VKT-7 server version 1, properties are parsed using the variable-length response format described in the official protocol: unit strings are `uint16 length + OEM/CP866 bytes + quality + NS`; fractional-digit properties are `value + quality + NS`. They must not be parsed by the ordinary fixed-size element parser.
