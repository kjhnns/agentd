package wsutil

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func pipeConns(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	a.SetDeadline(time.Now().Add(5 * time.Second))
	b.SetDeadline(time.Now().Add(5 * time.Second))
	wrap := func(c net.Conn, client bool) *Conn {
		return &Conn{conn: c, rw: bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c)), client: client}
	}
	return wrap(a, false), wrap(b, true)
}

func TestReadFrameLimit(t *testing.T) {
	for _, length := range []uint64{maxFrameSize + 1, 1 << 63, ^uint64(0)} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			server, client := pipeConns(t)
			header := []byte{0x81, 0xff, 0, 0, 0, 0, 0, 0, 0, 0}
			binary.BigEndian.PutUint64(header[2:], length)
			written := make(chan error, 1)
			go func() { _, err := client.conn.Write(header); written <- err }()
			if _, err := server.ReadText(); err == nil {
				t.Fatal("accepted oversized frame")
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			if _, err := client.rw.ReadByte(); err != io.EOF {
				t.Fatalf("peer read = %v, want EOF after rejected header", err)
			}
		})
	}
	t.Run("at limit", func(t *testing.T) {
		server, client := pipeConns(t)
		payload := bytes.Repeat([]byte("x"), maxFrameSize)
		written := make(chan error, 1)
		go func() { written <- client.WriteText(payload) }()
		got, err := server.ReadText()
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("frame at limit: length %d, error %v", len(got), err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
	})
}

func TestConcurrentTextAndPongWrites(t *testing.T) {
	server, client := pipeConns(t)
	const count = 32
	payload := bytes.Repeat([]byte("x"), 8192)
	errs := make(chan error, count+2)
	for i := 0; i < count; i++ {
		go func() { errs <- server.WriteText(payload) }()
	}
	go func() {
		_, err := server.ReadText()
		errs <- err
	}()
	go func() {
		for i := 0; i < count; i++ {
			if err := client.writeFrame(opPing, []byte("ping")); err != nil {
				errs <- err
				return
			}
		}
		errs <- client.WriteText([]byte("done"))
	}()
	texts, pongs := 0, 0
	for texts+pongs < 2*count {
		op, got, err := client.readFrame()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case op == opText && bytes.Equal(got, payload):
			texts++
		case op == opPong && string(got) == "ping":
			pongs++
		default:
			t.Fatalf("corrupt frame: opcode %d, length %d", op, len(got))
		}
	}
	for i := 0; i < count+2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if texts != count || pongs != count {
		t.Fatalf("got %d texts and %d pongs", texts, pongs)
	}
	go func() { errs <- server.Close() }()
	if op, _, err := client.readFrame(); err != nil || op != opClose {
		t.Fatalf("close frame: opcode %d, error %v", op, err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}
