// Package jfws is a minimal RFC 6455 server-side WebSocket, written against
// the standard library so the binary stays dependency-free. Swap it for
// nhooyr.io/websocket or gorilla/websocket if you would rather not maintain
// frame handling; the Conn interface below is deliberately small enough that
// the rest of the server does not care.
//
// Only what Jellyfin's /socket needs is implemented: text frames, ping/pong,
// close, and continuation frames. No permessage-deflate, no client mode.
package jfws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type opcode byte

const (
	opContinuation opcode = 0x0
	opText         opcode = 0x1
	opBinary       opcode = 0x2
	opClose        opcode = 0x8
	opPing         opcode = 0x9
	opPong         opcode = 0xA
)

// Conn is one upgraded connection. Writes are serialized internally, so it is
// safe to broadcast to a Conn from multiple goroutines. Reads must come from a
// single goroutine.
type Conn struct {
	raw    net.Conn
	br     *bufio.Reader
	mu     sync.Mutex // guards writes
	closed bool
}

// Upgrade performs the handshake and hijacks the connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("jfws: not a websocket upgrade")
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, errors.New("jfws: missing Connection: Upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("jfws: missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("jfws: ResponseWriter does not support hijacking")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	sum := sha1.Sum([]byte(key + magicGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Conn{raw: conn, br: brw.Reader}, nil
}

// ReadMessage blocks until a complete text or binary message arrives. Control
// frames are handled transparently: pings are answered, closes return io.EOF.
func (c *Conn) ReadMessage() ([]byte, error) {
	var payload []byte
	for {
		fin, op, data, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, data); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeFrame(opClose, nil)
			return nil, io.EOF
		}
		payload = append(payload, data...)
		if fin {
			return payload, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op opcode, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	op = opcode(hdr[0] & 0x0F)
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 1<<20 {
		err = fmt.Errorf("jfws: frame too large (%d bytes)", length)
		return
	}

	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// WriteText sends one unfragmented text frame.
func (c *Conn) WriteText(b []byte) error { return c.writeFrame(opText, b) }

func (c *Conn) writeFrame(op opcode, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}

	hdr := make([]byte, 0, 10)
	hdr = append(hdr, 0x80|byte(op)) // FIN set; server frames are never masked
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, 127)
		hdr = append(hdr, ext[:]...)
	}

	_ = c.raw.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.raw.Write(hdr); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.raw.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.raw.Close()
}

func (c *Conn) SetReadDeadline(t time.Time) error { return c.raw.SetReadDeadline(t) }
func (c *Conn) RemoteAddr() net.Addr              { return c.raw.RemoteAddr() }
