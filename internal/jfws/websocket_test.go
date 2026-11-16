package jfws

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------------ harness

// wsPair spins up a real HTTP server (httptest.NewRecorder cannot be hijacked),
// upgrades one connection, and hands both ends back to the test.
type wsPair struct {
	server *Conn         // the upgraded server-side connection
	client net.Conn      // the raw client socket, still speaking frames by hand
	cbr    *bufio.Reader // buffered reader over the client socket
}

func newWSPair(t *testing.T) *wsPair {
	t.Helper()
	return dialWith(t, "dGhlIHNhbXBsZSBub25jZQ==", nil)
}

// dialWith performs the handshake with a chosen key and optional header
// overrides, and fails the test if the upgrade does not succeed.
func dialWith(t *testing.T, key string, extra map[string]string) *wsPair {
	t.Helper()

	conns := make(chan *Conn, 1)
	errs := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			errs <- err
			return
		}
		// After a hijack the connection belongs to us, not the http.Server,
		// so returning here does not close it.
		conns <- c
	}))

	client, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}

	req := "GET /socket HTTP/1.1\r\nHost: " + client.RemoteAddr().String() + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n"
	for k, v := range extra {
		req += k + ": " + v + "\r\n"
	}
	req += "\r\n"
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	cbr := bufio.NewReader(client)
	resp, err := http.ReadResponse(cbr, nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}

	var sc *Conn
	select {
	case sc = <-conns:
	case err := <-errs:
		t.Fatalf("Upgrade failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the upgrade")
	}

	p := &wsPair{server: sc, client: client, cbr: cbr}
	t.Cleanup(func() {
		// Close both ends before the server: httptest.Server.Close blocks on
		// connections it thinks are still in flight.
		_ = sc.Close()
		_ = client.Close()
		srv.Close()
	})
	return p
}

// writeClientFrame emits a client->server frame. Client frames are always
// masked, per RFC 6455 section 5.3.
func (p *wsPair) writeClientFrame(t *testing.T, fin bool, op opcode, payload []byte) {
	t.Helper()
	if err := p.writeClientFrameErr(fin, op, payload); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
}

// writeClientFrameErr is the same but safe to call from a goroutine, where
// t.Fatalf is not allowed.
func (p *wsPair) writeClientFrameErr(fin bool, op opcode, payload []byte) error {
	var hdr []byte
	b0 := byte(op)
	if fin {
		b0 |= 0x80
	}
	hdr = append(hdr, b0)

	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, 0x80|127)
		hdr = append(hdr, ext[:]...)
	}

	mask := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	hdr = append(hdr, mask[:]...)

	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := p.client.Write(append(hdr, masked...))
	return err
}

// readServerFrame parses one server->client frame. Server frames are never
// masked, which this asserts.
func (p *wsPair) readServerFrame(t *testing.T) (fin bool, op opcode, payload []byte) {
	t.Helper()
	_ = p.client.SetReadDeadline(time.Now().Add(5 * time.Second))

	var hdr [2]byte
	if _, err := io.ReadFull(p.cbr, hdr[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	fin = hdr[0]&0x80 != 0
	op = opcode(hdr[0] & 0x0F)
	if hdr[1]&0x80 != 0 {
		t.Fatal("server frame is masked; servers must not mask")
	}

	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(p.cbr, ext[:]); err != nil {
			t.Fatalf("read 16-bit length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(p.cbr, ext[:]); err != nil {
			t.Fatalf("read 64-bit length: %v", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(p.cbr, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return fin, op, payload
}

// ---------------------------------------------------------------- handshake

// The accept key is SHA-1(key + magic GUID), base64'd. Getting it wrong means
// every browser refuses the connection. Vectors computed independently; the
// first is the example from RFC 6455 section 1.3.
func TestHandshakeAcceptKey(t *testing.T) {
	cases := []struct{ key, accept string }{
		{"dGhlIHNhbXBsZSBub25jZQ==", "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="},
		{"x3JJHMbDL1EzLkh9GBhXDw==", "HSmrc0sMlYUkAGmm5OPpG2HaGWk="},
		{"AAAAAAAAAAAAAAAAAAAAAA==", "ICX+Yqv66kxgM0FcWaLWlFLwTAI="},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			// Re-run the handshake capturing the response headers.
			conns := make(chan *Conn, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c, err := Upgrade(w, r); err == nil {
					conns <- c
				}
			}))
			client, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = client.Write([]byte("GET /socket HTTP/1.1\r\nHost: x\r\n" +
				"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
				"Sec-WebSocket-Key: " + c.key + "\r\n\r\n"))

			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if sc := <-conns; sc != nil {
					_ = sc.Close()
				}
				_ = client.Close()
				srv.Close()
			})

			if resp.StatusCode != 101 {
				t.Errorf("status = %d, want 101", resp.StatusCode)
			}
			if got := resp.Header.Get("Sec-WebSocket-Accept"); got != c.accept {
				t.Errorf("Sec-WebSocket-Accept = %q, want %q", got, c.accept)
			}
			if got := resp.Header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
				t.Errorf("Upgrade = %q, want websocket", got)
			}
			if got := resp.Header.Get("Connection"); !strings.EqualFold(got, "Upgrade") {
				t.Errorf("Connection = %q, want Upgrade", got)
			}
		})
	}
}

// These rejections all happen before the hijack, so a plain recorder is enough.
func TestUpgradeRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		wantErr string
	}{
		{"no upgrade header", map[string]string{
			"Connection": "Upgrade", "Sec-WebSocket-Key": "k"}, "not a websocket upgrade"},
		{"wrong upgrade value", map[string]string{
			"Upgrade": "h2c", "Connection": "Upgrade", "Sec-WebSocket-Key": "k"}, "not a websocket upgrade"},
		{"no connection header", map[string]string{
			"Upgrade": "websocket", "Sec-WebSocket-Key": "k"}, "missing Connection: Upgrade"},
		{"no key", map[string]string{
			"Upgrade": "websocket", "Connection": "Upgrade"}, "missing Sec-WebSocket-Key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/socket", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			_, err := Upgrade(httptest.NewRecorder(), r)
			if err == nil {
				t.Fatal("Upgrade succeeded, want an error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

// Header matching must be case- and format-insensitive: browsers send
// "Connection: keep-alive, Upgrade" and vary the casing of "WebSocket".
func TestUpgradeAcceptsHeaderVariants(t *testing.T) {
	for _, hdrs := range []map[string]string{
		{"Upgrade": "WebSocket"},
		{"Connection": "keep-alive, Upgrade"},
		{"Connection": "upgrade"},
	} {
		p := dialWith(t, "dGhlIHNhbXBsZSBub25jZQ==", hdrs)
		if p.server == nil {
			t.Errorf("no connection for headers %v", hdrs)
		}
	}
}

// A ResponseWriter that cannot be hijacked must be reported, not panicked on.
func TestUpgradeRequiresHijacker(t *testing.T) {
	r := httptest.NewRequest("GET", "/socket", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	_, err := Upgrade(httptest.NewRecorder(), r)
	if err == nil || !strings.Contains(err.Error(), "hijack") {
		t.Fatalf("error = %v, want it to mention hijacking", err)
	}
}

// ------------------------------------------------------------------- reading

func TestReadMessageUnmasksPayload(t *testing.T) {
	p := newWSPair(t)
	want := `{"MessageType":"KeepAlive"}`
	p.writeClientFrame(t, true, opText, []byte(want))

	got, err := p.server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("ReadMessage = %q, want %q", got, want)
	}
}

func TestReadMessageEmptyPayload(t *testing.T) {
	p := newWSPair(t)
	p.writeClientFrame(t, true, opText, nil)

	got, err := p.server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ReadMessage = %q, want empty", got)
	}
}

// Large messages arrive split across continuation frames; dropping the
// continuation handling truncates exactly the messages that matter most.
func TestReadMessageReassemblesContinuations(t *testing.T) {
	p := newWSPair(t)
	p.writeClientFrame(t, false, opText, []byte("{\"Message"))
	p.writeClientFrame(t, false, opContinuation, []byte("Type\":\"Keep"))
	p.writeClientFrame(t, true, opContinuation, []byte("Alive\"}"))

	got, err := p.server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"MessageType":"KeepAlive"}`
	if string(got) != want {
		t.Fatalf("ReadMessage = %q, want %q", got, want)
	}
}

// A ping in the middle of a fragmented message must be answered without
// corrupting the message being assembled.
func TestPingIsAnsweredTransparently(t *testing.T) {
	p := newWSPair(t)
	p.writeClientFrame(t, true, opPing, []byte("hello"))
	p.writeClientFrame(t, true, opText, []byte("after-ping"))

	got, err := p.server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after-ping" {
		t.Fatalf("ReadMessage = %q, want after-ping", got)
	}

	// The pong must echo the ping's payload.
	fin, op, payload := p.readServerFrame(t)
	if op != opPong {
		t.Errorf("opcode = %#x, want pong (%#x)", op, opPong)
	}
	if !fin {
		t.Error("pong should have FIN set")
	}
	if string(payload) != "hello" {
		t.Errorf("pong payload = %q, want it to echo %q", payload, "hello")
	}
}

// Pongs are unsolicited keep-alive replies and must not surface as messages.
func TestPongIsIgnored(t *testing.T) {
	p := newWSPair(t)
	p.writeClientFrame(t, true, opPong, []byte("unsolicited"))
	p.writeClientFrame(t, true, opText, []byte("real message"))

	got, err := p.server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "real message" {
		t.Fatalf("ReadMessage = %q, want the text frame", got)
	}
}

// A close frame surfaces as io.EOF so callers can use the usual read loop.
func TestCloseFrameReturnsEOF(t *testing.T) {
	p := newWSPair(t)
	p.writeClientFrame(t, true, opClose, nil)

	_, err := p.server.ReadMessage()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadMessage error = %v, want io.EOF", err)
	}

	// The server must echo a close frame back before going away.
	_, op, _ := p.readServerFrame(t)
	if op != opClose {
		t.Errorf("opcode = %#x, want close (%#x)", op, opClose)
	}
}

func TestReadMessageOnClosedSocketErrors(t *testing.T) {
	p := newWSPair(t)
	_ = p.client.Close()

	if _, err := p.server.ReadMessage(); err == nil {
		t.Fatal("ReadMessage succeeded on a dead socket, want an error")
	}
}

// The 1MiB cap stops a malicious or confused client from forcing a huge
// allocation. The length is checked before the payload is read, so this does
// not need to actually send the bytes.
func TestOversizedFrameIsRejected(t *testing.T) {
	p := newWSPair(t)

	hdr := []byte{0x80 | byte(opText), 0x80 | 127}
	var ext [8]byte
	binary.BigEndian.PutUint64(ext[:], (1<<20)+1)
	hdr = append(hdr, ext[:]...)
	hdr = append(hdr, 0, 0, 0, 0) // mask
	if _, err := p.client.Write(hdr); err != nil {
		t.Fatal(err)
	}

	_, err := p.server.ReadMessage()
	if err == nil {
		t.Fatal("ReadMessage accepted an oversized frame")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %q, want it to mention the size", err)
	}
}

// Exactly at the cap is still allowed -- the check is > not >=.
func TestFrameAtSizeLimitIsAccepted(t *testing.T) {
	p := newWSPair(t)
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	// A 1MiB write blocks until the server drains it, so it cannot run on the
	// test goroutine.
	werr := make(chan error, 1)
	go func() { werr <- p.writeClientFrameErr(true, opBinary, payload) }()

	got, err := p.server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("got %d bytes, want %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("payload differs at byte %d", i)
		}
	}
	if err := <-werr; err != nil {
		t.Fatalf("write client frame: %v", err)
	}
}

// ------------------------------------------------------------------- writing

// The three payload-length encodings are the classic place to get framing
// wrong, and the boundaries are where it happens.
func TestWriteTextLengthEncodings(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"short", 5},
		{"largest 7-bit length", 125},
		{"smallest 16-bit length", 126},
		{"largest 16-bit length", 65535},
		{"smallest 64-bit length", 65536},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newWSPair(t)
			payload := make([]byte, c.size)
			for i := range payload {
				payload[i] = byte('a' + i%26)
			}
			go func() {
				if err := p.server.WriteText(payload); err != nil {
					t.Errorf("WriteText: %v", err)
				}
			}()

			fin, op, got := p.readServerFrame(t)
			if !fin {
				t.Error("FIN not set on an unfragmented frame")
			}
			if op != opText {
				t.Errorf("opcode = %#x, want text (%#x)", op, opText)
			}
			if len(got) != c.size {
				t.Fatalf("got %d bytes, want %d", len(got), c.size)
			}
			for i := range got {
				if got[i] != payload[i] {
					t.Fatalf("payload differs at byte %d", i)
				}
			}
		})
	}
}

// A round trip through both directions on one connection, which is what the
// /socket handler actually does.
func TestFullDuplexExchange(t *testing.T) {
	p := newWSPair(t)

	go func() {
		_ = p.server.WriteText([]byte(`{"MessageType":"ForceKeepAlive","Data":30}`))
	}()
	_, op, got := p.readServerFrame(t)
	if op != opText || string(got) != `{"MessageType":"ForceKeepAlive","Data":30}` {
		t.Fatalf("server frame = %#x %q", op, got)
	}

	p.writeClientFrame(t, true, opText, []byte(`{"MessageType":"KeepAlive"}`))
	msg, err := p.server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != `{"MessageType":"KeepAlive"}` {
		t.Fatalf("ReadMessage = %q", msg)
	}
}

// -------------------------------------------------------------------- close

func TestCloseIsIdempotent(t *testing.T) {
	p := newWSPair(t)
	if err := p.server.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.server.Close(); err != nil {
		t.Errorf("second Close: %v, want nil", err)
	}
}

// Writing after Close must fail cleanly rather than on a reused fd. The
// broadcast paths in the server rely on this.
func TestWriteAfterCloseReturnsErrClosed(t *testing.T) {
	p := newWSPair(t)
	_ = p.server.Close()

	err := p.server.WriteText([]byte("too late"))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("WriteText after Close = %v, want net.ErrClosed", err)
	}
}

// Writes are serialized internally, so broadcasting from many goroutines is
// safe. Run with -race for this to mean anything.
func TestConcurrentWritesAreSerialized(t *testing.T) {
	p := newWSPair(t)

	const writers = 8
	const each = 20
	payload := []byte(`{"MessageType":"Sessions"}`)

	for i := 0; i < writers; i++ {
		go func() {
			for j := 0; j < each; j++ {
				if err := p.server.WriteText(payload); err != nil {
					t.Errorf("WriteText: %v", err)
					return
				}
			}
		}()
	}

	// Read on the test goroutine: every frame must arrive whole, with no
	// interleaving of one writer's bytes into another's frame.
	for i := 0; i < writers*each; i++ {
		fin, op, got := p.readServerFrame(t)
		if !fin || op != opText {
			t.Fatalf("frame %d: fin=%v op=%#x", i, fin, op)
		}
		if string(got) != string(payload) {
			t.Fatalf("frame %d interleaved: %q", i, got)
		}
	}
}

func TestRemoteAddrAndDeadline(t *testing.T) {
	p := newWSPair(t)
	if p.server.RemoteAddr() == nil {
		t.Error("RemoteAddr returned nil")
	}
	if err := p.server.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Errorf("SetReadDeadline: %v", err)
	}

	// An elapsed deadline must actually interrupt a blocked read.
	if err := p.server.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := p.server.ReadMessage(); err == nil {
		t.Fatal("ReadMessage succeeded, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("read blocked for %v, deadline was not honoured", elapsed)
	}
}
