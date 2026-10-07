package rfc2217

import (
	"bytes"
	"testing"
)

func TestTelnetDecoderEscapedIAC(t *testing.T) {
	var got []byte
	var negotiations int

	var decoder telnetDecoder
	decoder.feed(
		[]byte{'A', IAC, IAC, 'B'},
		func(b []byte) {
			got = append(got, b...)
		},
		func(byte, byte) {
			negotiations++
		},
		func([]byte) {
			t.Fatal("unexpected subnegotiation")
		},
	)

	want := []byte{'A', IAC, 'B'}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded data = %X, want %X", got, want)
	}

	if negotiations != 0 {
		t.Fatalf("negotiations = %d, want 0", negotiations)
	}
}

func TestTelnetDecoderSubnegotiationAcrossReads(t *testing.T) {
	var sub []byte
	var decoder telnetDecoder

	decoder.feed(
		[]byte{IAC, SB, COMPORT, SET_BAUDRATE, 0x00},
		func([]byte) {
			t.Fatal("unexpected application data")
		},
		func(byte, byte) {
			t.Fatal("unexpected negotiation")
		},
		func(b []byte) {
			sub = append(sub, b...)
		},
	)

	decoder.feed(
		[]byte{0x00, 0x4B, 0x20, IAC, SE},
		func([]byte) {
			t.Fatal("unexpected application data")
		},
		func(byte, byte) {
			t.Fatal("unexpected negotiation")
		},
		func(b []byte) {
			sub = append(sub, b...)
		},
	)

	want := []byte{
		COMPORT,
		SET_BAUDRATE,
		0x00,
		0x00,
		0x4B,
		0x20,
	}

	if !bytes.Equal(sub, want) {
		t.Fatalf("subnegotiation = %X, want %X", sub, want)
	}
}

func TestTelnetDecoderNegotiation(t *testing.T) {
	var gotCommand, gotOption byte
	var decoder telnetDecoder

	decoder.feed(
		[]byte{IAC, WILL, COMPORT},
		func([]byte) {
			t.Fatal("unexpected application data")
		},
		func(command, option byte) {
			gotCommand = command
			gotOption = option
		},
		func([]byte) {
			t.Fatal("unexpected subnegotiation")
		},
	)

	if gotCommand != WILL || gotOption != COMPORT {
		t.Fatalf(
			"negotiation = %02X %02X, want %02X %02X",
			gotCommand,
			gotOption,
			WILL,
			COMPORT,
		)
	}
}
