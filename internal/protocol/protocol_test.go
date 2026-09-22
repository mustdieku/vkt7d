package protocol

import (
	"encoding/binary"
	"encoding/hex"
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
