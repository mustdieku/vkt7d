# vkt7d

Go daemon for periodic collection of VKT-7 archives over RS-232 and storage in PostgreSQL.

## Architecture

The communication stack is divided into independent protocol layers:

- `internal/protocol` implements the VKT-7 request/response protocol,
  including frame construction, CRC-16 validation, exception handling,
  active-element/read-list processing and value decoding.
- `internal/rfc2217` implements the RFC 2217 Telnet transport and exposes it
  through the same `serial.Port` interface used by a local RS-232 device.
- collector and storage code operate on decoded VKT-7 records and do not
  depend on whether the serial connection is local or network-based.

VKT-7 framing and RFC 2217/Telnet framing are deliberately kept separate.
RFC 2217 is a transport protocol; it does not change the VKT-7 frame format.

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

Default RS-232 format is 8N2, with no flow control. For ordinary RS-232 the
VKT-7 protocol requires two leading `0xFF` bytes before each request.

For `rfc2217://` endpoints the Telnet stream is decoded before bytes reach the
VKT-7 protocol. A literal `0xFF` data byte is transported as `0xFF 0xFF`.
Telnet negotiation and RFC 2217 subnegotiations are removed from the stream
before VKT-7 frame parsing.

RFC 2217 port configuration is acknowledged by the remote server before the
client continues. The implementation configures 8 data bits, 2 stop bits,
no parity and no flow control, then asserts RTS for normal VKT-7 operation.
RTS is released during connection shutdown.

## Important protocol behavior

### VKT-7 transaction handling

Every request/response transaction starts by clearing stale receive data.
The response parser validates the VKT-7 frame length and CRC before returning
the frame to higher layers.

CRC failures are represented by the typed `CRCError`. The transaction layer
may retry a read-only request once, but it never repairs or accepts a frame
with an invalid CRC.

VKT-7 exception responses are six bytes long:

```text
address
function | 0x80
exception code
service byte
CRC low
CRC high
```

Exception code 3 is exposed as the semantic `ErrArchiveDateMissing`
condition. Exception code 5 causes the collector to refresh the active
element/read list before retrying the affected record.

### RFC 2217 transport

RFC 2217 is a Telnet-based serial transport. TCP reads do not preserve
Telnet message boundaries, therefore the RFC 2217 decoder is a stateful
byte-stream parser.

It handles:

- escaped `IAC` bytes (`0xFF 0xFF`);
- `WILL`, `WONT`, `DO` and `DONT` negotiation;
- `IAC SB ... IAC SE` subnegotiations;
- subnegotiations split across multiple TCP reads.

`PURGE_DATA` is an RFC 2217 server-side buffer operation. It is not
equivalent to clearing bytes already decoded by the local client.
`ResetInputBuffer` therefore performs the remote purge and then drains the
local decoded queue after a short settling interval. This is transport
synchronization and is not a VKT-7 timing requirement.

The daemon deliberately does not assume that every active element is meaningful for every archive type. It builds a read list from the active element list and filters it by the documented meaning of the element for the selected value type. The raw element value, quality byte and NS byte are retained in JSONB, while the archive row contains the standard fields and metadata.

If the device reports exception 5 while reading data, the daemon refreshes the active element list and retries the record. If a requested archive date is absent (exception 3), that date is not marked as collected; the next cycle can retry it.

For an empty archive table, the daemon queries the device archive interval and starts at the beginning of the relevant archive. For a non-empty table, it starts after the maximum stored timestamp/date. A safety overlap can be enabled with `--overlap` to re-read the last N periods.

## Notes

The VKT-7 document describes several firmware-version differences. This
implementation targets the documented protocol for firmware >= 1.5 and
supports active-element IDs through 82. Test against the actual meter before
enabling unattended collection.

RFC 2217 is specified by RFC 2217, *Telnet Com Port Control Option*. The
implementation uses the `COM-PORT-OPTION` command set, including
`SET_BAUDRATE`, `SET_DATASIZE`, `SET_PARITY`, `SET_STOPSIZE`, `SET_CONTROL`
and `PURGE_DATA`.

## SQL data model

The archive tables keep a stable relational key (`device_id` + archive timestamp/date) and retain the variable VKT-7 element payload in JSONB. This is intentional: the protocol states that the active element mask depends on the measurement scheme and may change during archive history. Each element is stored with its quality and NS byte, and `active_elements` keeps the size advertised by the device.

This avoids silently assigning a historical value to the wrong column after a scheme change. SQL views can later expose selected elements as conventional columns.

## Testing with a real device

The repository does not include a hardware simulator. Before production use, connect a VKT-7 and inspect the first session with `--verbose`. The protocol document specifies a 264-byte maximum frame, a 62.5 ms frame boundary, 8N2, 1200/2400/4800/9600/19200 baud, and RTS >= +9 V for RS-232. The actual USB/RS-232 adapter must provide the required RTS electrical level.
