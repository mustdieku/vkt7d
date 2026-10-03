// Package rfc2217 implements the Telnet COM Port Control Option from RFC 2217.
//
// The VKT-7 protocol itself is unchanged: RFC2217 is only a transport layer.
// The implementation deliberately satisfies go.bug.st/serial.Port so the
// protocol client, daemon and checker can use the same code path for a local
// serial device and for a network serial server.
package rfc2217

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"go.bug.st/serial"
)

const (
	IAC  = 0xff
	SE   = 0xf0
	SB   = 0xfa
	WILL = 0xfb
	WONT = 0xfc
	DO   = 0xfd
	DONT = 0xfe

	BINARY  = 0
	SGA     = 3
	COMPORT = 44

	SET_BAUDRATE        = 1
	SET_DATASIZE        = 2
	SET_PARITY          = 3
	SET_STOPSIZE        = 4
	SET_CONTROL         = 5
	NOTIFY_LINESTATE    = 6
	NOTIFY_MODEMSTATE   = 7
	FLOWCONTROL_SUSPEND = 8
	FLOWCONTROL_RESUME  = 9
	SET_LINESTATE_MASK  = 10
	SET_MODEMSTATE_MASK = 11
	PURGE_DATA          = 12

	SERVER_SET_BAUDRATE        = 101
	SERVER_SET_DATASIZE        = 102
	SERVER_SET_PARITY          = 103
	SERVER_SET_STOPSIZE        = 104
	SERVER_SET_CONTROL         = 105
	SERVER_NOTIFY_LINESTATE    = 106
	SERVER_NOTIFY_MODEMSTATE   = 107
	SERVER_FLOWCONTROL_SUSPEND = 108
	SERVER_FLOWCONTROL_RESUME  = 109
	SERVER_SET_LINESTATE_MASK  = 110
	SERVER_SET_MODEMSTATE_MASK = 111
	SERVER_PURGE_DATA          = 112

	PURGE_RECEIVE  = 1
	PURGE_TRANSMIT = 2
	PURGE_BOTH     = 3

	SET_CONTROL_REQ_FLOW_SETTING = 0
	SET_CONTROL_USE_NO_FLOW      = 1
	SET_CONTROL_USE_SW_FLOW      = 2
	SET_CONTROL_USE_HW_FLOW      = 3
	SET_CONTROL_BREAK_ON         = 5
	SET_CONTROL_BREAK_OFF        = 6
	SET_CONTROL_DTR_ON           = 8
	SET_CONTROL_DTR_OFF          = 9
	SET_CONTROL_RTS_ON           = 11
	SET_CONTROL_RTS_OFF          = 12

	PARITY_NONE  = 1
	PARITY_ODD   = 2
	PARITY_EVEN  = 3
	PARITY_MARK  = 4
	PARITY_SPACE = 5

	STOP_ONE      = 1
	STOP_TWO      = 2
	STOP_ONE_FIVE = 3

	MODEM_CD  = 0x80
	MODEM_RI  = 0x40
	MODEM_DSR = 0x20
	MODEM_CTS = 0x10
)

var answerCommand = map[byte]byte{
	SET_BAUDRATE:        SERVER_SET_BAUDRATE,
	SET_DATASIZE:        SERVER_SET_DATASIZE,
	SET_PARITY:          SERVER_SET_PARITY,
	SET_STOPSIZE:        SERVER_SET_STOPSIZE,
	SET_CONTROL:         SERVER_SET_CONTROL,
	NOTIFY_LINESTATE:    SERVER_NOTIFY_LINESTATE,
	NOTIFY_MODEMSTATE:   SERVER_NOTIFY_MODEMSTATE,
	FLOWCONTROL_SUSPEND: SERVER_FLOWCONTROL_SUSPEND,
	FLOWCONTROL_RESUME:  SERVER_FLOWCONTROL_RESUME,
	SET_LINESTATE_MASK:  SERVER_SET_LINESTATE_MASK,
	SET_MODEMSTATE_MASK: SERVER_SET_MODEMSTATE_MASK,
	PURGE_DATA:          SERVER_PURGE_DATA,
}

// Port is a network-backed serial port. Data received from the TCP connection
// is filtered through the Telnet state machine before it reaches Read, so IAC
// bytes in VKT-7 binary frames are correctly unescaped.
type Port struct {
	conn net.Conn

	writeMu sync.Mutex
	readMu  sync.Mutex
	ackMu   sync.Mutex
	acks    map[byte]chan []byte

	data      chan []byte
	errCh     chan error
	done      chan struct{}
	closeOnce sync.Once

	timeoutMu sync.RWMutex
	timeout   time.Duration

	modemMu sync.RWMutex
	modem   byte
}

// Open opens an RFC2217 endpoint. The endpoint syntax is:
//
//	rfc2217://host:port
//
// If the port is omitted, RFC2217's conventional port 2217 is used.
// The baud argument is applied to the remote serial port during negotiation.
func Open(address string, baud int) (*Port, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("invalid RFC2217 URL %q: %w", address, err)
	}
	if u.Scheme != "rfc2217" {
		return nil, fmt.Errorf("RFC2217 URL must use rfc2217://")
	}
	if u.User != nil {
		return nil, fmt.Errorf("RFC2217 URL userinfo is not supported")
	}
	host := u.Host
	if host == "" {
		return nil, fmt.Errorf("RFC2217 URL has no host")
	}
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		// A bare hostname/IP is accepted for convenience.
		host = net.JoinHostPort(host, "2217")
	}

	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect RFC2217 %s: %w", host, err)
	}
	p := &Port{
		conn:    conn,
		acks:    make(map[byte]chan []byte),
		data:    make(chan []byte, 64),
		errCh:   make(chan error, 1),
		done:    make(chan struct{}),
		timeout: 250 * time.Millisecond,
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}

	go p.reader()
	if err := p.negotiate(); err != nil {
		_ = p.Close()
		return nil, err
	}

	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		StopBits: serial.TwoStopBits,
		Parity:   serial.NoParity,
	}
	if err := p.SetMode(mode); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("RFC2217 port configuration: %w", err)
	}
	// VKT-7 requires RTS asserted during normal communication, matching the
	// existing local serial transport in internal/protocol.Open.
	if err := p.SetRTS(true); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("RFC2217 RTS=true: %w", err)
	}
	return p, nil
}

func (p *Port) negotiate() error {
	// RFC 2217 is a Telnet option. BINARY/SGA avoid Telnet transformations of
	// arbitrary VKT-7 binary bytes; COM-PORT-OPTION enables RFC2217 commands.
	for _, msg := range [][]byte{
		{IAC, WILL, BINARY}, {IAC, DO, BINARY},
		{IAC, WILL, SGA}, {IAC, DO, SGA},
		{IAC, WILL, COMPORT}, {IAC, DO, COMPORT},
	} {
		if err := p.writeRaw(msg); err != nil {
			return err
		}
	}
	// Negotiation is asynchronous. Actual RFC2217 support is verified by the
	// parameter acknowledgements below, so no fixed sleep is used as a gate.
	return nil
}

func (p *Port) SetMode(m *serial.Mode) error {
	if m == nil {
		return errors.New("nil serial mode")
	}
	if m.BaudRate <= 0 || uint64(m.BaudRate) > uint64(^uint32(0)) {
		return fmt.Errorf("invalid baud rate %d", m.BaudRate)
	}
	if m.DataBits < 5 || m.DataBits > 8 {
		return fmt.Errorf("unsupported data bits %d", m.DataBits)
	}

	var parity byte
	switch m.Parity {
	case serial.NoParity:
		parity = PARITY_NONE
	case serial.OddParity:
		parity = PARITY_ODD
	case serial.EvenParity:
		parity = PARITY_EVEN
	case serial.MarkParity:
		parity = PARITY_MARK
	case serial.SpaceParity:
		parity = PARITY_SPACE
	default:
		return fmt.Errorf("unsupported parity %d", m.Parity)
	}

	var stop byte
	switch m.StopBits {
	case serial.OneStopBit:
		stop = STOP_ONE
	case serial.TwoStopBits:
		stop = STOP_TWO
	case serial.OnePointFiveStopBits:
		stop = STOP_ONE_FIVE
	default:
		return fmt.Errorf("unsupported stop bits %d", m.StopBits)
	}

	if err := p.command(SET_BAUDRATE, u32be(uint32(m.BaudRate))); err != nil {
		return err
	}
	if err := p.command(SET_DATASIZE, []byte{byte(m.DataBits)}); err != nil {
		return err
	}
	if err := p.command(SET_PARITY, []byte{parity}); err != nil {
		return err
	}
	if err := p.command(SET_STOPSIZE, []byte{stop}); err != nil {
		return err
	}
	if m.InitialStatusBits != nil {
		if err := p.SetDTR(m.InitialStatusBits.DTR); err != nil {
			return err
		}
		if err := p.SetRTS(m.InitialStatusBits.RTS); err != nil {
			return err
		}
	}
	// Explicitly select no flow control; VKT-7 uses RTS as a control line,
	// not as an RTS/CTS flow-control input.
	return p.command(SET_CONTROL, []byte{SET_CONTROL_USE_NO_FLOW})
}

func u32be(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func (p *Port) command(cmd byte, value []byte) error {
	ack := make(chan []byte, 1)
	p.ackMu.Lock()
	if _, exists := p.acks[cmd]; exists {
		p.ackMu.Unlock()
		return fmt.Errorf("RFC2217 command 0x%02x already pending", cmd)
	}
	p.acks[cmd] = ack
	p.ackMu.Unlock()

	msg := make([]byte, 0, 6+len(value))
	msg = append(msg, IAC, SB, COMPORT, cmd)
	for _, b := range value {
		msg = append(msg, b)
		if b == IAC {
			msg = append(msg, IAC)
		}
	}
	msg = append(msg, IAC, SE)
	if err := p.writeRaw(msg); err != nil {
		p.removeAck(cmd)
		return err
	}

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case got := <-ack:
		p.removeAck(cmd)
		expected := answerCommand[cmd]
		if len(got) == 0 || got[0] != expected {
			return fmt.Errorf("RFC2217 unexpected acknowledgement for command 0x%02x: %x", cmd, got)
		}
		return nil
	case err := <-p.errCh:
		p.removeAck(cmd)
		if err == nil {
			err = io.EOF
		}
		return err
	case <-p.done:
		p.removeAck(cmd)
		return io.EOF
	case <-timer.C:
		p.removeAck(cmd)
		return fmt.Errorf("timeout waiting for RFC2217 acknowledgement for command 0x%02x", cmd)
	}
}

func (p *Port) removeAck(cmd byte) {
	p.ackMu.Lock()
	delete(p.acks, cmd)
	p.ackMu.Unlock()
}

func (p *Port) reader() {
	buf := make([]byte, 4096)
	state := byte(0)
	var sub []byte
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			select {
			case p.errCh <- err:
			default:
			}
			return
		}
		for _, b := range buf[:n] {
			switch state {
			case 0: // ordinary data
				if b == IAC {
					state = 1
				} else {
					p.pushData([]byte{b})
				}
			case 1: // after IAC
				switch b {
				case IAC:
					p.pushData([]byte{IAC})
					state = 0
				case WILL, WONT, DO, DONT:
					state = b
				case SB:
					sub = sub[:0]
					state = 3
				default:
					state = 0
				}
			case WILL, WONT, DO, DONT:
				p.handleNegotiation(state, b)
				state = 0
			case 3: // subnegotiation
				if b == IAC {
					state = 4
				} else {
					sub = append(sub, b)
				}
			case 4: // IAC inside subnegotiation
				if b == IAC {
					sub = append(sub, IAC)
					state = 3
				} else if b == SE {
					p.handleSubneg(sub)
					state = 0
				} else {
					state = 0
				}
			}
		}
	}
}

func (p *Port) handleNegotiation(cmd, opt byte) {
	switch cmd {
	case DO:
		if opt == BINARY || opt == SGA || opt == COMPORT {
			_ = p.writeRaw([]byte{IAC, WILL, opt})
		} else {
			_ = p.writeRaw([]byte{IAC, WONT, opt})
		}
	case DONT:
		_ = p.writeRaw([]byte{IAC, WONT, opt})
	case WILL:
		if opt == BINARY || opt == SGA || opt == COMPORT {
			_ = p.writeRaw([]byte{IAC, DO, opt})
		} else {
			_ = p.writeRaw([]byte{IAC, DONT, opt})
		}
	case WONT:
		_ = p.writeRaw([]byte{IAC, DONT, opt})
	}
}

func (p *Port) handleSubneg(v []byte) {
	if len(v) < 2 || v[0] != COMPORT {
		return
	}
	cmd := v[1]
	if cmd == SERVER_NOTIFY_MODEMSTATE && len(v) >= 3 {
		p.modemMu.Lock()
		p.modem = v[2]
		p.modemMu.Unlock()
		return
	}
	p.ackMu.Lock()
	defer p.ackMu.Unlock()
	var clientCmd byte
	for c, serverCmd := range answerCommand {
		if serverCmd == cmd {
			clientCmd = c
			break
		}
	}
	if clientCmd != 0 {
		if ch := p.acks[clientCmd]; ch != nil {
			select {
			case ch <- append([]byte(nil), v[1:]...):
			default:
			}
		}
	}
}

func (p *Port) pushData(b []byte) {
	if len(b) == 0 {
		return
	}
	cp := append([]byte(nil), b...)
	select {
	case p.data <- cp:
	case <-p.done:
	}
}

func (p *Port) writeRaw(b []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-p.done:
		return io.EOF
	default:
	}
	for len(b) > 0 {
		n, err := p.conn.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func (p *Port) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	encoded := make([]byte, 0, len(b)+4)
	for _, x := range b {
		encoded = append(encoded, x)
		if x == IAC {
			encoded = append(encoded, IAC)
		}
	}
	if err := p.writeRaw(encoded); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *Port) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.readMu.Lock()
	defer p.readMu.Unlock()

	p.timeoutMu.RLock()
	timeout := p.timeout
	p.timeoutMu.RUnlock()
	var timer <-chan time.Time
	var t *time.Timer
	if timeout >= 0 {
		t = time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	select {
	case d := <-p.data:
		n := copy(b, d)
		if n < len(d) {
			rest := append([]byte(nil), d[n:]...)
			select {
			case p.data <- rest:
			case <-p.done:
			}
		}
		return n, nil
	case err := <-p.errCh:
		if err == nil {
			err = io.EOF
		}
		return 0, err
	case <-p.done:
		return 0, io.EOF
	case <-timer:
		return 0, nil
	}
}

func (p *Port) SetReadTimeout(t time.Duration) error {
	if t < 0 && t != serial.NoTimeout {
		return fmt.Errorf("invalid read timeout %s", t)
	}
	p.timeoutMu.Lock()
	p.timeout = t
	p.timeoutMu.Unlock()
	return nil
}

func (p *Port) ResetInputBuffer() error {
	if err := p.command(PURGE_DATA, []byte{PURGE_RECEIVE}); err != nil {
		return err
	}
	for {
		select {
		case <-p.data:
		default:
			return nil
		}
	}
}

func (p *Port) ResetOutputBuffer() error {
	return p.command(PURGE_DATA, []byte{PURGE_TRANSMIT})
}

func (p *Port) SetRTS(v bool) error {
	if v {
		return p.command(SET_CONTROL, []byte{SET_CONTROL_RTS_ON})
	}
	return p.command(SET_CONTROL, []byte{SET_CONTROL_RTS_OFF})
}

func (p *Port) SetDTR(v bool) error {
	if v {
		return p.command(SET_CONTROL, []byte{SET_CONTROL_DTR_ON})
	}
	return p.command(SET_CONTROL, []byte{SET_CONTROL_DTR_OFF})
}

func (p *Port) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	p.modemMu.RLock()
	m := p.modem
	p.modemMu.RUnlock()
	return &serial.ModemStatusBits{
		CTS: m&MODEM_CTS != 0,
		DSR: m&MODEM_DSR != 0,
		RI:  m&MODEM_RI != 0,
		DCD: m&MODEM_CD != 0,
	}, nil
}

// RFC2217 has no separate drain primitive. TCP Write returning means the
// bytes were accepted by the local TCP stack; the remote serial server owns
// the physical transmitter queue.
func (p *Port) Drain() error { return nil }

func (p *Port) Break(d time.Duration) error {
	if err := p.command(SET_CONTROL, []byte{SET_CONTROL_BREAK_ON}); err != nil {
		return err
	}
	if d > 0 {
		time.Sleep(d)
	}
	return p.command(SET_CONTROL, []byte{SET_CONTROL_BREAK_OFF})
}

func (p *Port) Close() error {
	p.closeOnce.Do(func() {
		// VKT-7 requires RTS to be asserted during normal communication,
		// but it must be released before disconnecting the RFC2217 session.
		//
		// Do this before closing p.done and the TCP connection: SetRTS(false)
		// sends an RFC2217 SET_CONTROL command and waits for its
		// acknowledgement. If the remote connection is already broken,
		// the error is intentionally ignored because the TCP disconnect
		// itself is the only remaining way to terminate the session.
		_ = p.SetRTS(false)
		close(p.done)
		_ = p.conn.Close()
	})
	return nil
}

var _ serial.Port = (*Port)(nil)
