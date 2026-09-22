# vkt7d

Go daemon for periodic collection of VKT-7 archives over RS-232 and storage in PostgreSQL.

Based on the VKT-7 protocol document:
https://teplocom-sale.ru/upload/medialibrary/583/bdfk5q9bl70gouektwfq01v1ou10aqbo/Realizatsiya_protokola_obmena_dlya_svyazi_s_vychislitelem_VKT_7.pdf

## Scope

- hourly, daily, monthly and total archives;
- current and total-current values;
- properties and active element list;
- PostgreSQL persistence and resume from the last stored record;
- PostgreSQL is checked before opening the serial port;
- connection/protocol/database errors are logged and do not terminate the daemon;
- bounded batch collection per session;
- configurable serial and PostgreSQL parameters via CLI/environment;
- systemd unit for Ubuntu 24.04.

## Build

```bash
go mod download
go build -o vkt7d ./cmd/vkt7d
```

## Database

```bash
createdb vkt7
psql vkt7 < migrations/001_initial.sql
```

The program can also create the schema automatically with `--migrate`.

## Example

```bash
./vkt7d \
  --port=/dev/ttyUSB0 \
  --baud=9600 \
  --address=7 \
  --db-url='postgres://vkt7:secret@127.0.0.1:5432/vkt7?sslmode=disable' \
  --interval=3h
```

Default RS-232 format is 8N2, no flow control. For ordinary RS-232 the protocol requires at least two leading `0xFF` bytes before each request. The serial package does not expose RTS voltage control; if the USB/RS-232 adapter requires RTS to be asserted, configure that at the adapter/driver level.

## Important protocol behavior

The daemon deliberately does not assume that every active element is meaningful for every archive type. It builds a read list from the active element list and filters it by the documented meaning of the element for the selected value type. The raw element value, quality byte and NS byte are retained in JSONB, while the archive row contains the standard fields and metadata.

If the device reports exception 5 while reading data, the daemon refreshes the active element list and retries the record. If a requested archive date is absent (exception 3), that date is not marked as collected; the next cycle can retry it.

For an empty archive table, the daemon queries the device archive interval and starts at the beginning of the relevant archive. For a non-empty table, it starts after the maximum stored timestamp/date. A safety overlap can be enabled with `--overlap` to re-read the last N periods.

## Notes

The VKT-7 document describes several firmware-version differences. This implementation targets the documented protocol for firmware >= 1.5 and supports the newer active-element IDs through 82. Test against the actual meter before enabling unattended collection.

## SQL data model

The archive tables keep a stable relational key (`device_id` + archive timestamp/date) and retain the variable VKT-7 element payload in JSONB. This is intentional: the protocol states that the active element mask depends on the measurement scheme and may change during archive history. Each element is stored with its quality and NS byte, and `active_elements` keeps the size advertised by the device.

This avoids silently assigning a historical value to the wrong column after a scheme change. SQL views can later expose selected elements as conventional columns.

## Testing with a real device

The repository does not include a hardware simulator. Before production use, connect a VKT-7 and inspect the first session with `--verbose`. The protocol document specifies a 264-byte maximum frame, a 62.5 ms frame boundary, 8N2, 1200/2400/4800/9600/19200 baud, and RTS >= +9 V for RS-232. The actual USB/RS-232 adapter must provide the required RTS electrical level.

## Диагностика и тестирование

Проект также содержит две отдельные утилиты:

### vkt7check

End-to-end проверка ВКТ-7 без записи архивов в PostgreSQL. Использует те же `config`, `protocol` и `storage`, что и демон. Проверяет PostgreSQL, RS-232, начало сеанса, время, служебную информацию, свойства, активные элементы, схемы, активную БД, текущие/итоговые текущие данные и архивы до текущей даты.

```bash
./vkt7check --port=/dev/ttyUSB0 --baud=19200 --address=7 \
  --db-url='postgres://vkt7:vkt7@127.0.0.1:5432/vkt7?sslmode=disable'
```

### vkt7dbtest

Проверяет PostgreSQL через функции хранилища демона. Без `--write` выполняется безопасная проверка соединения и схемы. С `--write` выполняется полный тест записи/чтения с последующей очисткой тестовых строк.

```bash
./vkt7dbtest --write --migrate \
  --db-url='postgres://vkt7:vkt7@127.0.0.1:5432/vkt7?sslmode=disable'
```

Подробности: `README_TESTING.md`.
