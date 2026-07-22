// Package wsutil is a minimal, dependency-free RFC 6455 WebSocket implementation
// supporting BOTH directions of text frames plus a client dialer, kept in-tree so
// agentd stays a pure-stdlib single static binary (design 3.11) with no
// gorilla/websocket to vendor.
//
// The existing api/ws.go is server-write-only (the /events one-way stream). The
// web channel needs a client to also SEND typed input over the socket, so this
// package adds masked client-frame reading and a small client Dial for tests and
// tooling. Fragmentation is not supported (our messages are small single
// frames); ping is answered with pong; close is surfaced as io.EOF.
package wsutil

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// opcodes
const (
	opText  = 0x1
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

// Conn is one WebSocket connection. Server-side conns write unmasked frames;
// client-side conns (from Dial) mask their outgoing frames per the spec.
type Conn struct {
	conn   net.Conn
	rw     *bufio.ReadWriter
	client bool
}

// Upgrade performs the server handshake and hijacks the connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("not a websocket upgrade request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("response writer does not support hijack")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	h := sha1.Sum([]byte(key + magic))
	accept := base64.StdEncoding.EncodeToString(h[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &Conn{conn: conn, rw: rw}, nil
}

// Dial connects to a ws:// URL and completes the client handshake. header may
// carry auth (e.g. Authorization: Bearer ...); the query string may carry a
// token instead. Only ws:// (plaintext) is supported (local/Tailscale use).
func Dial(rawURL string, header http.Header) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("wsutil.Dial: only ws:// is supported, got %q", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	keyRaw := make([]byte, 16)
	_, _ = rand.Read(keyRaw)
	key := base64.StdEncoding.EncodeToString(keyRaw)
	path := u.RequestURI()
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", u.Host)
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	for k, vs := range header {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		conn.Close()
		return nil, err
	}
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	// Read the handshake response status line + headers.
	status, err := rw.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, "101") {
		// Drain a short body for a useful error, then fail.
		conn.Close()
		return nil, fmt.Errorf("wsutil.Dial: handshake not 101: %s", strings.TrimSpace(status))
	}
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &Conn{conn: conn, rw: rw, client: true}, nil
}

// WriteText sends one text frame. Client conns mask the payload; server conns do
// not (per RFC 6455).
func (c *Conn) WriteText(payload []byte) error {
	return c.writeFrame(opText, payload)
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	var header []byte
	header = append(header, 0x80|opcode) // FIN + opcode
	n := len(payload)
	maskBit := byte(0)
	if c.client {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		header = append(header, maskBit|byte(n))
	case n < 65536:
		header = append(header, maskBit|126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(n))
	default:
		header = append(header, maskBit|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(n))
	}
	if c.client {
		var mask [4]byte
		_, _ = rand.Read(mask[:])
		header = append(header, mask[:]...)
		masked := make([]byte, n)
		for i := 0; i < n; i++ {
			masked[i] = payload[i] ^ mask[i%4]
		}
		payload = masked
	}
	if _, err := c.rw.Write(header); err != nil {
		return err
	}
	if _, err := c.rw.Write(payload); err != nil {
		return err
	}
	return c.rw.Flush()
}

// ReadText blocks until a text frame arrives and returns its payload. Ping
// frames are answered with a pong and skipped; a close frame returns io.EOF.
// Non-text data frames are skipped.
func (c *Conn) ReadText() ([]byte, error) {
	for {
		op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opText:
			return payload, nil
		case opClose:
			return nil, io.EOF
		case opPing:
			_ = c.writeFrame(opPong, payload)
			continue
		default:
			// pong / binary / continuation: ignore and read the next frame.
			continue
		}
	}
}

func (c *Conn) readFrame() (byte, []byte, error) {
	b0, err := c.rw.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode := b0 & 0x0F
	b1, err := c.rw.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	masked := b1&0x80 != 0
	length := uint64(b1 & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.rw, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.rw, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.rw, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.rw, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

// Close sends a close frame and closes the socket.
func (c *Conn) Close() error {
	_ = c.writeFrame(opClose, nil)
	return c.conn.Close()
}
