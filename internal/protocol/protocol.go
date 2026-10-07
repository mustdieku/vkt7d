package protocol

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
	"golang.org/x/text/encoding/charmap"

	"vkt7d/internal/model"
	"vkt7d/internal/rfc2217"
)

// VKT-7 Modbus-like function codes used by this implementation.
const (
	functionRead  byte = 0x03
	functionWrite byte = 0x10
)

// VKT-7 frame and protocol constants.
const (
	maxFrameSize       = 264
	requestPrefixSize  = 2
	exceptionFrameSize = 6
	writeResponseSize  = 8
	requestPrefix      = byte(0xFF)
)

const (
	RegActive    = 0x3FFC
	RegDate      = 0x3FFB
	RegValueType = 0x3FFD
	RegReadData  = 0x3FFE
	RegReadList  = 0x3FFF
	RegService   = 0x3FF9
	RegRange     = 0x3FF6
)
const (
	Hourly       = 0
	Daily        = 1
	Monthly      = 2
	Total        = 3
	Current      = 4
	CurrentTotal = 5
	Properties   = 6
)

type Client struct {
	Port    serial.Port
	Address byte
	Timeout time.Duration
	Log     *slog.Logger
	Debug   bool
}

// CRCError reports a complete VKT-7 response whose CRC does not match.
//
// CRC validation is part of the VKT-7 protocol layer. A typed error keeps
// retry decisions independent from human-readable error strings.
type CRCError struct {
	Frame []byte
}

func (e *CRCError) Error() string {
	return fmt.Sprintf("CRC error in VKT-7 response: %X", e.Frame)
}

// CRC16 calculates the CRC-16 used by VKT-7.
func CRC16(b []byte) uint16 {
	crc := uint16(0xffff)
	for _, x := range b {
		crc ^= uint16(x)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xa001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// frame builds a complete VKT-7 request frame.
//
// The frame layout is:
//
//	address | function | register | quantity | payload | CRC
//
// The two leading 0xFF bytes required by the physical VKT-7 interface are
// added by tx(), not stored in the protocol frame itself.
func frame(addr byte, fn byte, reg uint16, qty uint16, payload []byte) []byte {
	b := []byte{addr, fn, byte(reg >> 8), byte(reg), byte(qty >> 8), byte(qty)}
	b = append(b, payload...)
	c := CRC16(b)
	return append(b, byte(c), byte(c>>8))
}

// tx executes one VKT-7 request/response transaction.
//
// Input buffering is cleared before the request because VKT-7 does not
// provide a transaction identifier. Bytes left over from a previous
// transaction therefore cannot safely be associated with a new request.
func (c *Client) tx(req []byte) ([]byte, error) {
	readTimeout := 250 * time.Millisecond
	if c.Timeout > 0 && c.Timeout < readTimeout {
		readTimeout = c.Timeout
	}
	if err := c.Port.SetReadTimeout(readTimeout); err != nil {
		return nil, fmt.Errorf("set serial read timeout: %w", err)
	}

	if err := c.Port.ResetInputBuffer(); err != nil {
		return nil, fmt.Errorf("reset serial input buffer: %w", err)
	}

	time.Sleep(20 * time.Millisecond)
	if err := c.Port.ResetInputBuffer(); err != nil {
		return nil, fmt.Errorf("settle/reset serial input buffer: %w", err)
	}

	const maxAttempts = 2

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		txFrame := append([]byte{requestPrefix, requestPrefix}, req...)
		if c.Debug && c.Log != nil {
			c.Log.Debug(
				"vkt7 TX",
				"frame", fmt.Sprintf("%X", txFrame),
				"len", len(txFrame),
				"function", fmt.Sprintf("0x%02X", req[1]),
				"attempt", attempt,
			)
		}

		if _, e := c.Port.Write(txFrame); e != nil {
			return nil, e
		}

		response, err := c.readResponse(req)
		if err == nil {
			return response, nil
		}

		lastErr = err

		var crcErr *CRCError
		if !errors.As(err, &crcErr) || attempt == maxAttempts {
			return nil, err
		}

		if c.Debug && c.Log != nil {
			c.Log.Debug(
				"vkt7 retry after CRC error",
				"attempt", attempt,
				"error", err,
			)
		}

		if err := c.Port.ResetInputBuffer(); err != nil {
			return nil, fmt.Errorf("reset serial input buffer after CRC error: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	return nil, lastErr
}

// responseLength returns the expected VKT-7 response length once enough
// header bytes are available to determine it.
func responseLength(requestFunction byte, response []byte) (int, bool, error) {
	if len(response) < 2 {
		return 0, false, nil
	}

	// VKT-7 exceptions have a fixed six-byte response:
	//
	//	address | function|0x80 | exception | service | CRC
	if response[1]&0x80 != 0 {
		if len(response) >= exceptionFrameSize {
			return exceptionFrameSize, true, nil
		}
		return 0, false, nil
	}

	switch requestFunction {
	case functionRead:
		if len(response) < 3 {
			return 0, false, nil
		}
		n := int(response[2]) + 5
		if n > maxFrameSize {
			return 0, false, fmt.Errorf("VKT-7 response exceeds maximum frame size: %d", n)
		}
		return n, true, nil

	case functionWrite:
		return writeResponseSize, true, nil

	default:
		return 0, false, fmt.Errorf(
			"unsupported VKT-7 function: 0x%02X",
			requestFunction,
		)
	}
}

// validateResponse validates a complete VKT-7 response frame.
func validateResponse(frame []byte, expected int) error {
	if len(frame) != expected {
		return fmt.Errorf(
			"invalid VKT-7 response length: got=%d want=%d raw=%X",
			len(frame),
			expected,
			frame,
		)
	}

	if len(frame) < 2 {
		return fmt.Errorf("VKT-7 response is too short: %d", len(frame))
	}

	got := binary.LittleEndian.Uint16(frame[len(frame)-2:])
	want := CRC16(frame[:len(frame)-2])
	if got != want {
		return &CRCError{
			Frame: append([]byte(nil), frame...),
		}
	}

	return nil
}

func (c *Client) readResponse(req []byte) ([]byte, error) {
	deadline := time.Now().Add(c.Timeout)
	var out []byte
	buf := make([]byte, maxFrameSize)

	for time.Now().Before(deadline) {
		n, e := c.Port.Read(buf)
		if e != nil {
			// With a finite read timeout, a no-data read may be reported as
			// EOF by some backends. Keep waiting until the transaction deadline;
			// other errors are real serial failures.
			if errors.Is(e, io.EOF) {
				continue
			}
			return nil, e
		}
		if n == 0 {
			continue
		}

		out = append(out, buf[:n]...)

		if len(out) > maxFrameSize {
			return nil, fmt.Errorf(
				"VKT-7 response exceeds maximum frame size: %d",
				len(out),
			)
		}

		expected, ready, err := responseLength(req[1], out)
		if err != nil {
			return nil, err
		}
		if !ready {
			continue
		}

		if len(out) < expected {
			continue
		}

		if len(out) > expected {
			return nil, fmt.Errorf(
				"VKT-7 response contains trailing bytes: got=%d want=%d raw=%X",
				len(out),
				expected,
				out,
			)
		}

		if err := validateResponse(out, expected); err != nil {
			if c.Debug && c.Log != nil {
				c.Log.Debug("vkt7 RX CRC ERROR",
					"frame", fmt.Sprintf("%X", out),
					"len", len(out),
				)
			}
			return nil, err
		}

		if c.Debug && c.Log != nil {
			c.Log.Debug(
				"vkt7 RX",
				"frame", fmt.Sprintf("%X", out),
				"len", len(out),
				"function", fmt.Sprintf("0x%02X", out[1]),
			)
		}
		return out, nil
	}

	if c.Debug && c.Log != nil {
		c.Log.Debug("vkt7 RX TIMEOUT", "partial", fmt.Sprintf("%X", out), "len", len(out))
	}
	return nil, fmt.Errorf("timeout waiting for VKT-7 response; tx=%s rx=%s", hex.EncodeToString(req), hex.EncodeToString(out))
}

func (c *Client) read(reg uint16, qty uint16) ([]byte, error) {
	return c.tx(frame(c.Address, functionRead, reg, qty, nil))
}

func (c *Client) write(reg uint16, qty uint16, payload []byte) ([]byte, error) {
	return c.tx(frame(c.Address, functionWrite, reg, qty, payload))
}

func (c *Client) Begin() error {
	_, e := c.write(RegReadList, 0, []byte{0xcc, 0x80, 0, 0, 0})
	return e
}

func (c *Client) ReadService() ([]byte, error) {
	r, e := c.read(RegService, 0)
	if e != nil {
		return nil, e
	}
	return dataPart(r), nil
}

// ServiceInfo describes the service-information record returned by register
// 0x3FF9 on VKT-7 firmware versions that support the documented service
// information format.
type ServiceInfo struct {
	Firmware   int
	SchemeTV1  int
	SchemeTV2  int
	Subscriber string
	Address    int
	ReportDay  int
	Model      int
}

// ParseService decodes the fixed-size service-information payload.
func ParseService(d []byte) (ServiceInfo, error) {
	if len(d) < 16 {
		return ServiceInfo{}, fmt.Errorf("short service response: %x", d)
	}
	return ServiceInfo{
		Firmware:   int(d[0]),
		SchemeTV1:  int(binary.LittleEndian.Uint16(d[1:3])),
		SchemeTV2:  int(binary.LittleEndian.Uint16(d[3:5])),
		Subscriber: strings.TrimRight(string(d[5:13]), "\x00 "),
		Address:    int(d[13]),
		ReportDay:  int(d[14]),
		Model:      int(d[15]),
	}, nil
}

// ReadRange returns the archive range information reported by the device.
func (c *Client) ReadRange() ([]byte, error) {
	r, e := c.read(RegRange, 0)
	if e != nil {
		return nil, e
	}
	return dataPart(r), nil
}

// ReadScheme reads the tariff/measurement scheme identifier for TV1 or TV2.
func (c *Client) ReadScheme(tv int) (byte, byte, byte, error) {
	reg := uint16(0x3ECD)
	if tv == 2 {
		reg = 0x3F5B
	}
	r, err := c.read(reg, 1)
	if err != nil {
		return 0, 0, 0, err
	}
	d := dataPart(r)
	if len(d) < 3 {
		return 0, 0, 0, fmt.Errorf("bad scheme response: %x", d)
	}
	return d[0], d[1], d[2], nil
}

// ReadActiveDB reads the currently selected archive database identifier.
func (c *Client) ReadActiveDB() (byte, byte, byte, error) {
	r, err := c.read(0x3FE9, 1)
	if err != nil {
		return 0, 0, 0, err
	}
	d := dataPart(r)
	if len(d) < 3 {
		return 0, 0, 0, fmt.Errorf("bad active-db response: %x", d)
	}
	return d[0], d[1], d[2], nil
}

// ReadSubscriberID reads the subscriber identifier and its quality metadata.
func (c *Client) ReadSubscriberID() ([]byte, byte, byte, error) {
	r, err := c.read(0x3EA6, 8)
	if err != nil {
		return nil, 0, 0, err
	}
	d := dataPart(r)
	if len(d) < 4 {
		return nil, 0, 0, fmt.Errorf("bad subscriber response: %x", d)
	}

	// The subscriber value is encoded as a length-prefixed byte string,
	// followed by quality and NS bytes.
	n := int(binary.LittleEndian.Uint16(d[:2]))
	if 2+n+2 > len(d) {
		return nil, 0, 0, fmt.Errorf("invalid subscriber length %d: %x", n, d)
	}
	return append([]byte(nil), d[2:2+n]...), d[2+n], d[2+n+1], nil
}

// dataPart extracts the payload from a normal VKT-7 read response.
//
// A normal read response is:
//
//	address | function | byte-count | data | CRC
//
// Exception frames are returned unchanged so that parseException can inspect
// the exception code.
func dataPart(r []byte) []byte {
	if len(r) < 5 {
		return nil
	}
	if r[1]&0x80 != 0 {
		return r
	}
	n := int(r[2])
	if n+5 <= len(r) {
		return r[3 : 3+n]
	}
	return nil
}

// ExceptionError represents a VKT-7 protocol exception response.
// Code 3 means that the requested archive timestamp/date is absent.
// Code 5 means that the measurement scheme changed and the active/read list
// must be refreshed before retrying the same archive record.
type ExceptionError struct {
	Code     byte
	Function byte
}

// Error implements error.
func (e *ExceptionError) Error() string {
	return fmt.Sprintf("VKT-7 exception code=%d", e.Code)
}

// IsExceptionCode reports whether err contains the specified VKT-7 exception.
func IsExceptionCode(err error, code byte) bool {
	if code == 3 && IsArchiveDateMissing(err) {
		return true
	}

	var ex *ExceptionError
	return errors.As(err, &ex) && ex.Code == code
}

// ErrArchiveDateMissing represents VKT-7 exception code 3.
//
// It is a semantic archive condition rather than a transport failure: the
// requested archive date does not contain a record.
var ErrArchiveDateMissing = errors.New("VKT-7 archive date has no data")

// IsArchiveDateMissing reports whether err represents a missing archive date.
func IsArchiveDateMissing(err error) bool {
	return errors.Is(err, ErrArchiveDateMissing)
}

// parseException converts a valid VKT-7 exception response into a typed error.
func parseException(r []byte) error {
	if len(r) >= 3 && r[1]&0x80 != 0 {
		if r[2] == 3 {
			return fmt.Errorf("%w: %w", ErrArchiveDateMissing, &ExceptionError{
				Code:     3,
				Function: r[1] & 0x7f,
			})
		}
		return &ExceptionError{Code: r[2], Function: r[1] & 0x7f}
	}
	return nil
}

// ReadDeviceTime reads the current date and time from the meter.
func (c *Client) ReadDeviceTime() (time.Time, error) {
	r, e := c.read(RegDate, 0)
	if e != nil {
		return time.Time{}, e
	}
	d := dataPart(r)
	if len(d) < 6 {
		return time.Time{}, fmt.Errorf("bad device time response: %x", d)
	}
	y := 2000 + int(d[2])
	return time.Date(y, time.Month(d[1]), int(d[0]), int(d[3]), int(d[4]), int(d[5]), 0, time.Local), nil
}

// SetType selects the VKT-7 value/archive type used by subsequent operations.
func (c *Client) SetType(t byte) error {
	r, e := c.write(RegValueType, 0, []byte{2, t, 0})
	if e != nil {
		return e
	}
	return parseException(r)
}

// SetDate selects the archive timestamp used by the next ReadData operation.
func (c *Client) SetDate(t time.Time, dailyLike bool) error {
	hour := byte(t.Hour())
	if dailyLike {
		hour = 23
	}

	// The write payload contains a byte-count followed by:
	// day, month, year-2000 and hour.
	r, e := c.write(RegDate, 0, []byte{
		4,
		byte(t.Day()),
		byte(t.Month()),
		byte(t.Year() - 2000),
		hour,
	})
	if e != nil {
		return e
	}
	return parseException(r)
}

// ActiveElements reads the device-defined list of active logical elements.
//
// Each entry consists of a 32-bit logical address and a 16-bit element size.
func (c *Client) ActiveElements() ([]model.Element, error) {
	r, e := c.read(RegActive, 0)
	if e != nil {
		return nil, e
	}

	d := dataPart(r)

	if c.Debug && c.Log != nil {
		c.Log.Debug(
			"vkt7 active-elements response",
			"raw", fmt.Sprintf("%X", r),
			"data", fmt.Sprintf("%X", d),
			"data_len", len(d),
		)
	}

	if ex := parseException(r); ex != nil {
		return nil, ex
	}

	if len(d)%6 != 0 {
		return nil, fmt.Errorf("invalid active-elements data length: %d (raw=%X)", len(d), d)
	}
	if len(d) == 0 {
		return nil, fmt.Errorf("empty active-elements response (raw=%X)", r)
	}

	var es []model.Element

	for i := 0; i+6 <= len(d); i += 6 {
		// The active-element response contains the logical address. The
		// 0x40000000 flag is added only when building the read-list request.
		a := int(binary.LittleEndian.Uint32(d[i:i+4]) & 0x3FFFFFFF)
		sz := int(binary.LittleEndian.Uint16(d[i+4 : i+6]))

		if a < 83 {
			es = append(es, model.Element{
				Address: a,
				Name:    ElementName(a),
				Size:    sz,
			})
		}
	}
	if len(es) == 0 {
		return nil, fmt.Errorf("active-elements response contains no supported logical addresses (raw=%X)", d)
	}
	if c.Debug && c.Log != nil {
		c.Log.Debug("vkt7 active-elements parsed", "count", len(es))
	}

	return es, nil
}

// makeReadListPayload serializes active elements into the VKT-7 read-list
// representation.
//
// Each element is encoded as address|0x40000000 followed by its size.
func makeReadListPayload(es []model.Element) ([]byte, error) {
	p := make([]byte, 0, len(es)*6)
	for _, e := range es {
		a := uint32(e.Address) | 0x40000000
		x := make([]byte, 6)
		binary.LittleEndian.PutUint32(x, a)
		binary.LittleEndian.PutUint16(x[4:], uint16(e.Size))
		p = append(p, x...)
	}

	if len(p) > 255 {
		return nil, fmt.Errorf("read list payload too large: %d bytes", len(p))
	}
	payload := make([]byte, 1+len(p))
	payload[0] = byte(len(p))
	copy(payload[1:], p)
	return payload, nil
}

// SetReadList writes the element list used by subsequent ReadData operations.
func (c *Client) SetReadList(es []model.Element) error {
	payload, err := makeReadListPayload(es)
	if err != nil {
		return err
	}
	r, e := c.write(RegReadList, 0, payload)
	if e != nil {
		return e
	}
	return parseException(r)
}

// ReadData reads the record prepared by SetType, SetReadList and SetDate.
func (c *Client) ReadData() ([]byte, error) {
	r, e := c.read(RegReadData, 0)
	if e != nil {
		return nil, e
	}
	if ex := parseException(r); ex != nil {
		return nil, ex
	}
	d := dataPart(r)
	if c.Debug && c.Log != nil {
		c.Log.Debug("vkt7 read-data", "raw", fmt.Sprintf("%X", r), "data", fmt.Sprintf("%X", d), "data_len", len(d))
	}
	return d, nil
}

// PrepareArchive configures the value type and read list before reading
// multiple records of the same archive type.
func (c *Client) PrepareArchive(typ int, es []model.Element) error {
	if err := c.SetType(byte(typ)); err != nil {
		return err
	}
	return c.SetReadList(es)
}

// ReadArchiveData selects the timestamp and reads one prepared archive record.
func (c *Client) ReadArchiveData(typ int, when time.Time, es []model.Element) (map[string]model.Value, error) {
	if err := c.SetDate(when, typ == Daily || typ == Monthly || typ == Total); err != nil {
		return nil, err
	}
	d, err := c.ReadData()
	if err != nil {
		return nil, err
	}
	return ParseElements(es, d, typ)
}

// ElementName maps a VKT-7 logical element address to its application name.
func ElementName(a int) string {
	names := []string{"t1_1", "t2_1", "t3_1", "V1_1", "V2_1", "V3_1", "M1_1", "M2_1", "M3_1", "P1_1", "P2_1", "Mg_1", "Qo_1", "Qg_1", "dt_1", "tx", "ta", "BNP_1", "VOC_1", "G1_1", "G2_1", "G3_1", "t1_2", "t2_2", "t3_2", "V1_2", "V2_2", "V3_2", "M1_2", "M2_2", "M3_2", "P1_2", "P2_2", "Mg_2", "Qo_2", "Qg_2", "dt_2", "reserved_37", "reserved_38", "BNP_2", "VOC_2", "G1_2", "G2_2", "G3_2", "t_unit", "G_unit", "V_unit", "M_unit", "P_unit", "dt_unit", "tx_unit", "ta_unit", "Mg_unit", "Qo_unit", "Qg_unit", "BNP_unit", "VOC_unit", "t_dec", "G_dec_reserved", "V1_dec", "M1_dec", "P1_dec", "dt_dec", "tx_dec", "ta_dec", "Mg_dec", "Qo1_dec", "t2_dec_reserved", "G2_dec_reserved", "V2_dec", "M2_dec", "P2_dec", "dt2_dec", "tx2_dec", "ta2_dec", "Mg2_dec", "Qo2_dec", "NS_1", "NS_2", "QntNS_1", "QntNS_2", "DI", "P3"}
	if a >= 0 && a < len(names) {
		return names[a]
	}
	return "element_" + strconv.Itoa(a)
}

// ParseProperties decodes the special properties record.
//
// Properties are not encoded like ordinary elements. Unit properties use:
//
//	uint16 string length (LE) | string bytes | quality | NS
//
// Fraction-digit properties use:
//
//	uint8 value | quality | NS
func ParseProperties(es []model.Element, d []byte) (map[string]model.Value, error) {
	if len(es) != 16 {
		return nil, fmt.Errorf("unexpected properties element count: %d", len(es))
	}
	out := make(map[string]model.Value, len(es))
	off := 0
	unit := func(e model.Element) error {
		if off+2 > len(d) {
			return fmt.Errorf("short properties data at element %d: missing string length", e.Address)
		}
		n := int(binary.LittleEndian.Uint16(d[off : off+2]))
		off += 2
		if n > e.Size {
			return fmt.Errorf("invalid property %d: string length %d exceeds declared size %d", e.Address, n, e.Size)
		}
		if off+n+2 > len(d) {
			return fmt.Errorf("short properties data at element %d: need string %d + quality/NS, have %d", e.Address, n, len(d)-off)
		}
		raw := append([]byte(nil), d[off:off+n]...)
		off += n
		q, ns := d[off], d[off+1]
		off += 2
		decoded, err := charmap.CodePage866.NewDecoder().Bytes(raw)
		if err != nil {
			return fmt.Errorf("decode property %d: %w", e.Address, err)
		}
		out[e.Name] = model.Value{Value: string(decoded), Quality: q, NS: ns, Raw: raw}
		return nil
	}
	frac := func(e model.Element) error {
		if off+3 > len(d) {
			return fmt.Errorf("short properties data at element %d: need 3 bytes, have %d", e.Address, len(d)-off)
		}
		v, q, ns := d[off], d[off+1], d[off+2]
		off += 3
		out[e.Name] = model.Value{Value: int64(v), Quality: q, NS: ns, Raw: []byte{v}}
		return nil
	}
	for i, e := range es {
		if i < 8 {
			if e.Size != 7 {
				return nil, fmt.Errorf("property %d has unexpected unit size %d", e.Address, e.Size)
			}
			if err := unit(e); err != nil {
				return nil, err
			}
		} else {
			if e.Size != 1 {
				return nil, fmt.Errorf("property %d has unexpected fractional size %d", e.Address, e.Size)
			}
			if err := frac(e); err != nil {
				return nil, err
			}
		}
	}
	if off != len(d) {
		return nil, fmt.Errorf("extra properties data: %d bytes", len(d)-off)
	}
	return out, nil
}

// decodeElementValue converts the raw value bytes of one ordinary VKT-7
// element into its Go representation.
//
// The decoding rule is determined by the logical element address. The byte
// width is still taken from the active-element list for signed integers.
func decodeElementValue(e model.Element, raw []byte) (any, error) {
	switch e.Address {
	case 77, 78:
		// NSPrintTypeM_1/2 are single printable characters.
		return string(raw), nil

	case 79, 80:
		// QntNS_1/2 contain five unsigned 16-bit counters.
		if len(raw) != 10 {
			return nil, fmt.Errorf(
				"invalid element %d (%s) size: got %d, want 10",
				e.Address, e.Name, len(raw),
			)
		}

		values := make([]uint16, 5)
		for i := range values {
			values[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
		}
		return values, nil

	case 19, 20, 21, 41, 42, 43, 81:
		// G1/G2/G3 and DopInpImpP_Type are IEEE-754 float32 values.
		if len(raw) != 4 {
			return nil, fmt.Errorf(
				"invalid float32 element %d (%s) size: got %d, want 4",
				e.Address, e.Name, len(raw),
			)
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(raw)), nil

	case 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56:
		// Unit properties are decoded by ParseProperties when the device
		// returns the properties record. In an ordinary element stream they
		// remain byte strings.
		return string(raw), nil

	case 57, 58, 59, 60, 61, 62, 63, 64, 65, 66,
		67, 68, 69, 70, 71, 72, 73, 74, 75, 76:
		// Fraction-digit properties are unsigned 8-bit values.
		if len(raw) != 1 {
			return nil, fmt.Errorf(
				"invalid uint8 property %d (%s) size: got %d, want 1",
				e.Address, e.Name, len(raw),
			)
		}
		return uint8(raw[0]), nil

	default:
		// Ordinary numeric elements are signed integers. Their width is
		// defined by the active-element list, so sign extension must be based
		// on len(raw), not on a fixed integer type.
		return decodeSignedInt(raw)
	}
}

func ParseElements(es []model.Element, d []byte, typ int) (map[string]model.Value, error) {
	out := map[string]model.Value{}
	off := 0
	for _, e := range es {
		if off+e.Size+2 > len(d) {
			return nil, fmt.Errorf("short data at element %d: need %d bytes, have %d", e.Address, e.Size+2, len(d)-off)
		}

		raw := append([]byte(nil), d[off:off+e.Size]...)
		q := d[off+e.Size]
		ns := d[off+e.Size+1]
		off += e.Size + 2

		v, err := decodeElementValue(e, raw)
		if err != nil {
			return nil, fmt.Errorf(
				"decode element %d (%s): %w",
				e.Address, e.Name, err,
			)
		}

		out[e.Name] = model.Value{Value: v, Quality: q, NS: ns, Raw: raw}
	}

	// typ is retained in the public API because callers use the same parser
	// for all archive types. The ordinary element stream itself does not
	// require different decoding rules based on typ.
	_ = typ

	return out, nil
}

// decodeSignedInt decodes a little-endian signed integer with the width
// specified by the supplied byte slice.
//
// VKT-7 uses different integer widths for different logical elements. The
// active-element list supplies the width, so the value must be sign-extended
// from that exact width.
//
// Examples:
//
//	0xA8       -> -88
//	0xA8 0xFF  -> -88
//	0xA8 0xFF 0xFF -> -88
func decodeSignedInt(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("empty signed integer")
	}
	if len(b) > 8 {
		return 0, fmt.Errorf("signed integer size %d exceeds 8 bytes", len(b))
	}

	var x int64
	for i, v := range b {
		x |= int64(v) << (8 * i)
	}

	// Sign-extend the encoded integer to int64.
	//
	// For an 8-byte value there is nothing to extend because the sign bit
	// is already int64's sign bit.
	if len(b) < 8 && b[len(b)-1]&0x80 != 0 {
		x |= ^int64(0) << (8 * len(b))
	}

	return x, nil
}

// Open opens either a local serial device or an RFC2217 endpoint.
func Open(port string, baud int, log *slog.Logger, debug bool) (serial.Port, error) {
	if strings.HasPrefix(strings.ToLower(port), "rfc2217://") {
		return rfc2217.Open(port, baud, log, debug)
	}
	m := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		StopBits: serial.TwoStopBits,
		Parity:   serial.NoParity,
	}
	p, err := serial.Open(port, m)
	if err != nil {
		return nil, err
	}
	// VKT-7 RS-232 requires RTS to be asserted. Set it explicitly rather
	// than relying on a platform/driver default.
	if err := p.SetRTS(true); err != nil {
		p.Close()
		return nil, fmt.Errorf("set RTS=true: %w", err)
	}
	return p, nil
}

// ReadArchiveRecord performs the complete setup and read sequence for one record.
func (c *Client) ReadArchiveRecord(typ int, when time.Time, es []model.Element) (map[string]model.Value, error) {
	if e := c.SetType(byte(typ)); e != nil {
		return nil, e
	}
	if e := c.SetReadList(es); e != nil {
		return nil, e
	}
	return c.ReadArchiveData(typ, when, es)
}

// ReadCurrent reads one current/total-current record using the supplied list.
func (c *Client) ReadCurrent(typ int, es []model.Element) (map[string]model.Value, error) {
	if e := c.SetType(byte(typ)); e != nil {
		return nil, e
	}
	if e := c.SetReadList(es); e != nil {
		return nil, e
	}
	d, e := c.ReadData()
	if e != nil {
		return nil, e
	}
	return ParseElements(es, d, typ)
}

// DebugRecord returns a compact JSON representation of a decoded record.
func (c *Client) DebugRecord(v map[string]model.Value) string {
	b, _ := json.Marshal(v)
	return strings.TrimSpace(string(b))
}
