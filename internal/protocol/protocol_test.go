package protocol

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"testing"

	"vkt7d/internal/model"
)

func TestCRC16VKT7(t *testing.T) {
	// Standard Modbus CRC check vector; the VKT-7 protocol uses the same
	// reflected polynomial 0xA001 and initial value 0xFFFF.
	got := CRC16([]byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x0A})
	if got != 0xCDC5 {
		t.Fatalf("CRC16 = 0x%04X, want 0xCDC5", got)
	}
}

func TestReadListPayloadIncludesByteCount(t *testing.T) {
	es := []model.Element{
		{Address: 44, Size: 7},
		{Address: 45, Size: 7},
		{Address: 76, Size: 1},
	}
	p, err := makeReadListPayload(es)
	if err != nil {
		t.Fatal(err)
	}
	if p[0] != 18 {
		t.Fatalf("byte count = %d, want 18", p[0])
	}
	if len(p) != 19 {
		t.Fatalf("payload length = %d, want 19", len(p))
	}
	if got := binary.LittleEndian.Uint32(p[1:5]); got != 0x4000002c {
		t.Fatalf("first conditional address = 0x%08X, want 0x4000002C", got)
	}
	if got := binary.LittleEndian.Uint16(p[5:7]); got != 7 {
		t.Fatalf("first element size = %d, want 7", got)
	}
}

func TestExceptionError(t *testing.T) {
	err := parseException([]byte{0x07, 0x90, 0x03, 0x00, 0x00, 0x00})
	if !IsExceptionCode(err, 3) {
		t.Fatalf("expected exception code 3, got %v", err)
	}
	var ex *ExceptionError
	if !errors.As(err, &ex) || ex.Function != 0x10 {
		t.Fatalf("unexpected exception: %#v", err)
	}
}


func TestWriteResponseCRCAndLength(t *testing.T) {
	// VKT-7 0x10 successful response for the begin-session request.
	rx := []byte{0x07, 0x10, 0x3F, 0xFF, 0x00, 0x00, 0xFC, 0x4B}
	if len(rx) != 8 {
		t.Fatalf("response length = %d, want 8", len(rx))
	}
	got := CRC16(rx[:6])
	if got != 0x4BFC {
		t.Fatalf("CRC16 = 0x%04X, want 0x4BFC", got)
	}
	if rx[6] != byte(got) || rx[7] != byte(got>>8) {
		t.Fatalf("invalid CRC bytes: %02X %02X", rx[6], rx[7])
	}
}

func TestBeginFrame(t *testing.T) {
	req := frame(7, 0x10, RegReadList, 0, []byte{0xCC, 0x80, 0, 0, 0})
	want := "07103fff0000cc800000007e20"
	if got := hex.EncodeToString(req); got != want {
		t.Fatalf("begin frame = %s, want %s", got, want)
	}
}

func TestParseService(t *testing.T) {
	d := []byte{
		0x27,
		0x88, 0x02,
		0x98, 0x0C,
		'0', '0', '1', '5', '9', '5', '3', '7',
		7,
		30,
		3,
	}

	s, err := ParseService(d)
	if err != nil {
		t.Fatal(err)
	}

	if s.Firmware != 0x27 ||
		s.SchemeTV1 != 648 ||
		s.SchemeTV2 != 3224 ||
		s.Subscriber != "00159537" ||
		s.Address != 7 ||
		s.ReportDay != 30 ||
		s.Model != 3 {
		t.Fatalf("unexpected service info: %+v", s)
	}
}

func TestExceptionCode3IsRecognized(t *testing.T) {
	err := parseException([]byte{7, 0x90, 3, 0, 0, 0})
	if !IsExceptionCode(err, 3) || !IsArchiveDateMissing(err) {
		t.Fatalf("exception 3 not recognized: %v", err)
	}
}

func TestElementNamePropertyP1Dec(t *testing.T) {
	if got := ElementName(61); got != "P1_dec" {
		t.Fatalf("ElementName(61) = %q, want P1_dec", got)
	}
}

func TestParseActiveElementsUsesLogicalAddress(t *testing.T) {
	// Two active elements: logical addresses 44 and 82. The active list
	// itself contains the logical address; 0x40000000 is added only when
	// writing the read list.
	d := make([]byte, 12)
	binary.LittleEndian.PutUint32(d[0:4], 44)
	binary.LittleEndian.PutUint16(d[4:6], 7)
	binary.LittleEndian.PutUint32(d[6:10], 82)
	binary.LittleEndian.PutUint16(d[10:12], 1)

	es := make([]model.Element, 0, 2)
	for i := 0; i < len(d); i += 6 {
		a := int(binary.LittleEndian.Uint32(d[i:i+4]) & 0x3FFFFFFF)
		sz := int(binary.LittleEndian.Uint16(d[i+4 : i+6]))
		es = append(es, model.Element{Address: a, Name: ElementName(a), Size: sz})
	}
	if len(es) != 2 || es[0].Address != 44 || es[1].Address != 82 {
		t.Fatalf("unexpected active elements: %+v", es)
	}
}

func TestDecodeSignedInt(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want int64
	}{
		{
			name: "int8 positive",
			raw:  []byte{0x58},
			want: 88,
		},
		{
			name: "int8 negative",
			raw:  []byte{0xA8},
			want: -88,
		},
		{
			name: "int16 positive",
			raw:  []byte{0x58, 0x00},
			want: 88,
		},
		{
			name: "int16 negative",
			raw:  []byte{0xA8, 0xFF},
			want: -88,
		},
		{
			name: "int24 negative",
			raw:  []byte{0xA8, 0xFF, 0xFF},
			want: -88,
		},
		{
			name: "int32 positive",
			raw:  []byte{0x58, 0x00, 0x00, 0x00},
			want: 88,
		},
		{
			name: "int32 negative",
			raw:  []byte{0xA8, 0xFF, 0xFF, 0xFF},
			want: -88,
		},
		{
			name: "int64 positive",
			raw:  []byte{0x58, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			want: 88,
		},
		{
			name: "int64 negative",
			raw:  []byte{0xA8, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			want: -88,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeSignedInt(tt.raw)
			if err != nil {
				t.Fatalf("decodeSignedInt(%X): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("decodeSignedInt(%X) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDecodeSignedIntRejectsInvalidSize(t *testing.T) {
	if _, err := decodeSignedInt(nil); err == nil {
		t.Fatal("expected error for empty integer")
	}

	if _, err := decodeSignedInt(make([]byte, 9)); err == nil {
		t.Fatal("expected error for integer larger than 8 bytes")
	}
}

func TestParseElementsSignedInt16(t *testing.T) {
	// dt_1 (element 14), 2-byte signed integer:
	//   A8 FF == 0xFFA8 == -88 as int16.
	//
	// Followed by quality and NS bytes.
	es := []model.Element{
		{
			Address: 14,
			Name:    "dt_1",
			Size:    2,
		},
	}

	d := []byte{
		0xA8, 0xFF,
		0xC0, // quality
		0x00, // NS
	}

	got, err := ParseElements(es, d, Hourly)
	if err != nil {
		t.Fatal(err)
	}

	value, ok := got["dt_1"]
	if !ok {
		t.Fatal("dt_1 not found")
	}

	n, ok := value.Value.(int64)
	if !ok {
		t.Fatalf("dt_1 type = %T, want int64", value.Value)
	}
	if n != -88 {
		t.Fatalf("dt_1 = %d, want -88", n)
	}
}

func TestParseElementsFloat32(t *testing.T) {
	es := []model.Element{
		{
			Address: 19,
			Name:    "G1_1",
			Size:    4,
		},
	}

	d := make([]byte, 6)
	binary.LittleEndian.PutUint32(d[0:4], math.Float32bits(12.5))
	d[4] = 0xC0
	d[5] = 0x00

	got, err := ParseElements(es, d, Hourly)
	if err != nil {
		t.Fatal(err)
	}

	value, ok := got["G1_1"]
	if !ok {
		t.Fatal("G1_1 not found")
	}

	f, ok := value.Value.(float32)
	if !ok {
		t.Fatalf("G1_1 type = %T, want float32", value.Value)
	}
	if f != float32(12.5) {
		t.Fatalf("G1_1 = %v, want 12.5", f)
	}
}

func TestParseElementsQntNS(t *testing.T) {
	es := []model.Element{
		{
			Address: 79,
			Name:    "QntNS_1",
			Size:    10,
		},
	}

	d := make([]byte, 12)
	for i := 0; i < 5; i++ {
		binary.LittleEndian.PutUint16(d[i*2:i*2+2], uint16(i+1))
	}
	d[10] = 0xC0
	d[11] = 0x00

	got, err := ParseElements(es, d, Hourly)
	if err != nil {
		t.Fatal(err)
	}

	value, ok := got["QntNS_1"]
	if !ok {
		t.Fatal("QntNS_1 not found")
	}

	x, ok := value.Value.([]uint16)
	if !ok {
		t.Fatalf("QntNS_1 type = %T, want []uint16", value.Value)
	}

	want := []uint16{1, 2, 3, 4, 5}
	if !reflect.DeepEqual(x, want) {
		t.Fatalf("QntNS_1 = %v, want %v", x, want)
	}
}
