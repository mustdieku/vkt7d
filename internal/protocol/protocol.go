package protocol

import (
	"bytes"
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
func frame(addr byte, fn byte, reg uint16, qty uint16, payload []byte) []byte {
	b := []byte{addr, fn, byte(reg >> 8), byte(reg), byte(qty >> 8), byte(qty)}
	b = append(b, payload...)
	c := CRC16(b)
	return append(b, byte(c), byte(c>>8))
}
func (c *Client) tx(req []byte) ([]byte, error) {
	// A serial Read must not be allowed to block longer than the protocol
	// operation timeout. go.bug.st/serial exposes SetReadTimeout for this;
	// the overall deadline below still bounds the complete transaction.
	readTimeout := 250 * time.Millisecond
	if c.Timeout > 0 && c.Timeout < readTimeout {
		readTimeout = c.Timeout
	}
	if err := c.Port.SetReadTimeout(readTimeout); err != nil {
		return nil, fmt.Errorf("set serial read timeout: %w", err)
	}

	// Drop stale bytes from a previous failed/partial transaction. A VKT-7
	// frame is requested atomically, so bytes already in the RX queue cannot
	// be safely attached to the new request.
	if err := c.Port.ResetInputBuffer(); err != nil {
		return nil, fmt.Errorf("reset serial input buffer: %w", err)
	}

	txFrame := append([]byte{0xff, 0xff}, req...)
	if c.Debug && c.Log != nil {
		c.Log.Debug("vkt7 TX", "frame", fmt.Sprintf("%X", txFrame), "len", len(txFrame), "function", fmt.Sprintf("0x%02X", req[1]))
	}
	if _, e := c.Port.Write(txFrame); e != nil {
		return nil, e
	}

	deadline := time.Now().Add(c.Timeout)
	var out []byte
	buf := make([]byte, 264)

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

		// VKT-7 exception response:
		//   address, function|0x80, error code, service byte, CRC-lo, CRC-hi
		// i.e. exactly 6 bytes. The previous implementation waited for at
		// least 7 bytes and therefore converted a valid exception response
		// into a timeout.
		if len(out) >= 2 && out[1]&0x80 != 0 {
			if len(out) >= 6 {
				if len(out) == 6 {
					csum := CRC16(out[:4])
					if out[4] == byte(csum) && out[5] == byte(csum>>8) {
						return out, nil
					}
				}
				if len(out) > 6 {
					return nil, fmt.Errorf("invalid VKT-7 exception frame: %X", out)
				}
			}
			continue
		}

		// Normal response depends on the function code.
		// 0x03 ("read") uses: address, function, byte-count, data..., CRC-lo, CRC-hi.
		// 0x10 ("write") uses the standard fixed 8-byte confirmation:
		// address, function, start-address(2), register-count(2), CRC-lo, CRC-hi.
		// The previous implementation treated every normal response as 0x03.
		// That makes a valid 0x10 response such as
		//   07 10 3F FF 00 00 FC 4B
		// look as if it contained 0x3F data bytes and causes a timeout.
		var expected int
		switch req[1] {
		case 0x03:
			if len(out) >= 3 {
				expected = int(out[2]) + 5
			}
		case 0x10:
			expected = 8
		default:
			return nil, fmt.Errorf("unsupported VKT-7 function in response: 0x%02X", req[1])
		}
		if expected > 264 {
			return nil, fmt.Errorf("frame exceeds 264 bytes")
		}
		if expected > 0 && len(out) >= expected {
			if len(out) != expected {
				return nil, fmt.Errorf("invalid VKT-7 response length: got=%d want=%d raw=%X", len(out), expected, out)
			}
			csum := CRC16(out[:expected-2])
			if out[expected-2] == byte(csum) && out[expected-1] == byte(csum>>8) {
				if c.Debug && c.Log != nil {
					c.Log.Debug("vkt7 RX", "frame", fmt.Sprintf("%X", out), "len", len(out), "function", fmt.Sprintf("0x%02X", out[1]), "data_len", expected-5)
				}
				return out, nil
			}
			if c.Debug && c.Log != nil {
				c.Log.Debug("vkt7 RX CRC ERROR", "frame", fmt.Sprintf("%X", out), "len", len(out))
			}
			return nil, fmt.Errorf("CRC error in VKT-7 response: %X", out)
		}
	}
	if c.Debug && c.Log != nil {
		c.Log.Debug("vkt7 RX TIMEOUT", "partial", fmt.Sprintf("%X", out), "len", len(out))
	}
	return nil, fmt.Errorf("timeout waiting for VKT-7 response; tx=%s rx=%s", hex.EncodeToString(req), hex.EncodeToString(out))
}
func (c *Client) read(reg uint16, qty uint16) ([]byte, error) {
	return c.tx(frame(c.Address, 0x03, reg, qty, nil))
}
func (c *Client) write(reg uint16, qty uint16, payload []byte) ([]byte, error) {
	return c.tx(frame(c.Address, 0x10, reg, qty, payload))
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
func (c *Client) ReadRange() ([]byte, error) {
	r, e := c.read(RegRange, 0)
	if e != nil {
		return nil, e
	}
	return dataPart(r), nil
}

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

func (c *Client) ReadSubscriberID() ([]byte, byte, byte, error) {
	r, err := c.read(0x3EA6, 8)
	if err != nil {
		return nil, 0, 0, err
	}
	d := dataPart(r)
	if len(d) < 4 {
		return nil, 0, 0, fmt.Errorf("bad subscriber response: %x", d)
	}
	// The response contains a VT string structure followed by quality and NS.
	if len(d) < 2 {
		return nil, 0, 0, fmt.Errorf("short subscriber response: %x", d)
	}
	n := int(binary.LittleEndian.Uint16(d[:2]))
	if n < 0 || 2+n+2 > len(d) {
		return nil, 0, 0, fmt.Errorf("invalid subscriber length %d: %x", n, d)
	}
	return append([]byte(nil), d[2:2+n]...), d[2+n], d[2+n+1], nil
}
func dataPart(r []byte) []byte {
	if len(r) < 5 {
		return nil
	}
	if r[1]&0x80 != 0 {
		return r
	}
	if len(r) < 5 {
		return nil
	}
	n := int(r[2])
	// A normal 0x03 response has an explicit byte-count. In particular,
	// byte-count == 0 is valid and must produce an empty data section rather
	// than accidentally returning the two CRC bytes as data.
	if n+5 <= len(r) {
		if n+5 == len(r) {
			return r[3 : 3+n]
		}
		// Be conservative if a transport backend returned trailing bytes.
		return r[3 : 3+n]
	}
	return nil
}
func parseException(r []byte) error {
	if len(r) >= 3 && r[1]&0x80 != 0 {
		return fmt.Errorf("VKT-7 exception code=%d", r[2])
	}
	return nil
}

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

func (c *Client) SetType(t byte) error {
	r, e := c.write(RegValueType, 0, []byte{2, t, 0})
	if e != nil {
		return e
	}
	return parseException(r)
}
func (c *Client) SetDate(t time.Time, dailyLike bool) error {
	hour := byte(t.Hour())
	if dailyLike {
		hour = 23
	}
	r, e := c.write(RegDate, 0, []byte{byte(t.Day()), byte(t.Month()), byte(t.Year() - 2000), hour})
	if e != nil {
		return e
	}
	return parseException(r)
}
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

	var es []model.Element

	for i := 0; i+6 <= len(d); i += 6 {
		// VKT-7 returns the active-element address with
		// bit 30 set (0x40000000). The actual element number
		// is contained in the lower bits.
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

	if c.Debug && c.Log != nil {
		c.Log.Debug("vkt7 active-elements parsed", "count", len(es))
	}

	return es, nil
}
func makeReadListPayload(es []model.Element) ([]byte, error) {
	p := make([]byte, 0, len(es)*6)
	for _, e := range es {
		a := uint32(e.Address) | 0x40000000
		x := make([]byte, 6)
		binary.LittleEndian.PutUint32(x, a)
		binary.LittleEndian.PutUint16(x[4:], uint16(e.Size))
		p = append(p, x...)
	}
	// Function 0x10 has a byte-count field before the actual payload.
	// VKT-7 explicitly requires this for the read-list request.
	if len(p) > 255 {
		return nil, fmt.Errorf("read list payload too large: %d bytes", len(p))
	}
	payload := make([]byte, 1+len(p))
	payload[0] = byte(len(p))
	copy(payload[1:], p)
	return payload, nil
}

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
func ElementName(a int) string {
	names := []string{"t1_1", "t2_1", "t3_1", "V1_1", "V2_1", "V3_1", "M1_1", "M2_1", "M3_1", "P1_1", "P2_1", "Mg_1", "Qo_1", "Qg_1", "dt_1", "tx", "ta", "BNP_1", "VOC_1", "G1_1", "G2_1", "G3_1", "t1_2", "t2_2", "t3_2", "V1_2", "V2_2", "V3_2", "M1_2", "M2_2", "M3_2", "P1_2", "P2_2", "Mg_2", "Qo_2", "Qg_2", "dt_2", "reserved_37", "reserved_38", "BNP_2", "VOC_2", "G1_2", "G2_2", "G3_2", "t_unit", "G_unit", "V_unit", "M_unit", "P_unit", "dt_unit", "tx_unit", "ta_unit", "Mg_unit", "Qo_unit", "Qg_unit", "BNP_unit", "VOC_unit", "t_dec", "G_dec_reserved", "V1_dec", "M1_dec", "P_dec", "dt_dec", "tx_dec", "ta_dec", "Mg_dec", "Qo1_dec", "t2_dec_reserved", "G2_dec_reserved", "V2_dec", "M2_dec", "P2_dec", "dt2_dec", "tx2_dec", "ta2_dec", "Mg2_dec", "Qo2_dec", "NS_1", "NS_2", "QntNS_1", "QntNS_2", "DI", "P3"}
	if a >= 0 && a < len(names) {
		return names[a]
	}
	return "element_" + strconv.Itoa(a)
}

// ParseProperties parses the special VKT-7 properties response used when
// server version is 1. Unlike ordinary data elements, properties do not form
// a fixed-size stream of size+quality+NS records. Unit properties are encoded
// as: uint16 string length (LE) + OEM/CP866 string + quality + NS.
// Fraction-digit properties are: value + quality + NS.
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
		var v any
		switch e.Address {
		case 77, 78:
			v = string(raw)
		case 79, 80:
			if len(raw) == 10 {
				x := make([]uint16, 5)
				for i := range x {
					x[i] = binary.LittleEndian.Uint16(raw[i*2:])
				}
				v = x
			}
		case 19, 20, 21, 41, 42, 43, 81:
			if len(raw) >= 4 {
				v = math.Float32frombits(binary.LittleEndian.Uint32(raw))
			}
		default:
			v = decodeInt(raw)
		}
		out[e.Name] = model.Value{Value: v, Quality: q, NS: ns, Raw: raw}
	}
	if off != len(d) && typ != Properties { /* tolerate extra service bytes */
	}
	return out, nil
}
func decodeInt(b []byte) int64 {
	var x int64
	n := len(b)
	if n > 8 {
		n = 8
	}
	for i := 0; i < n; i++ {
		x |= int64(b[i]) << (8 * i)
	}
	return x
}

func Open(port string, baud int) (serial.Port, error) {
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

func (c *Client) ReadArchiveRecord(typ int, when time.Time, es []model.Element) (map[string]model.Value, error) {
	if e := c.SetType(byte(typ)); e != nil {
		return nil, e
	}
	if e := c.SetReadList(es); e != nil {
		return nil, e
	}
	daily := typ == Daily || typ == Monthly || typ == Total
	if e := c.SetDate(when, daily); e != nil {
		return nil, e
	}
	d, e := c.ReadData()
	if e != nil {
		return nil, e
	}
	return ParseElements(es, d, typ)
}
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
func (c *Client) DebugRecord(v map[string]model.Value) string {
	b, _ := json.Marshal(v)
	return strings.TrimSpace(string(b))
}

var _ = bytes.Compare
var _ = slog.Default
