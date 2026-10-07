package rfc2217

// telnetDecoder removes the Telnet framing used by RFC 2217.
//
// RFC 2217 transports serial data inside a Telnet byte stream. A literal
// 0xFF data byte is encoded as 0xFF 0xFF, while IAC WILL/WONT/DO/DONT and
// IAC SB ... IAC SE are protocol control sequences. The decoder is
// deliberately independent of net.Conn so that the byte-stream rules can
// be tested without a live RFC 2217 server.
type telnetDecoder struct {
	state byte
	sub   []byte
}

const (
	telnetData byte = iota
	telnetIAC
	telnetWILL
	telnetWONT
	telnetDO
	telnetDONT
	telnetSB
	telnetSBIAC
)

func (d *telnetDecoder) feed(
	input []byte,
	onData func([]byte),
	onNegotiation func(command, option byte),
	onSubnegotiation func(data []byte),
) {
	data := make([]byte, 0, len(input))

	flush := func() {
		if len(data) == 0 {
			return
		}
		onData(data)
		data = data[:0]
	}

	for _, b := range input {
		switch d.state {
		case telnetData:
			if b == IAC {
				flush()
				d.state = telnetIAC
			} else {
				data = append(data, b)
			}

		case telnetIAC:
			switch b {
			case IAC:
				// IAC IAC represents a literal 0xFF data byte.
				data = append(data, IAC)
				d.state = telnetData

			case WILL:
				d.state = telnetWILL

			case WONT:
				d.state = telnetWONT

			case DO:
				d.state = telnetDO

			case DONT:
				d.state = telnetDONT

			case SB:
				d.sub = d.sub[:0]
				d.state = telnetSB

			default:
				// VKT-7 does not use one-byte Telnet commands.
				// Ignore unsupported commands and continue decoding.
				d.state = telnetData
			}

		case telnetWILL, telnetWONT, telnetDO, telnetDONT:
			onNegotiation(d.stateToCommand(), b)
			d.state = telnetData

		case telnetSB:
			if b == IAC {
				d.state = telnetSBIAC
			} else {
				d.sub = append(d.sub, b)
			}

		case telnetSBIAC:
			switch b {
			case IAC:
				// IAC IAC inside subnegotiation represents a literal
				// 0xFF in the subnegotiation payload.
				d.sub = append(d.sub, IAC)
				d.state = telnetSB

			case SE:
				onSubnegotiation(append([]byte(nil), d.sub...))
				d.state = telnetData

			default:
				// Malformed subnegotiation. Do not expose its contents
				// as application data.
				d.state = telnetData
			}
		}
	}

	flush()
}

func (d *telnetDecoder) stateToCommand() byte {
	switch d.state {
	case telnetWILL:
		return WILL
	case telnetWONT:
		return WONT
	case telnetDO:
		return DO
	case telnetDONT:
		return DONT
	default:
		return 0
	}
}
