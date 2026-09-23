package protocol

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func decodeBlock(t *testing.T, block []byte) map[string]string {
	t.Helper()
	fields := map[string]string{}
	dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) { fields[f.Name] = f.Value })
	if _, err := dec.Write(block); err != nil {
		t.Fatalf("decode %x: %v", block, err)
	}
	if err := dec.Close(); err != nil {
		t.Fatal(err)
	}
	return fields
}

// TestEncodeHeaderBlock holds the hand-made HPACK to what x/net's decoder,
// which net/http's server decodes it with, makes of it.
func TestEncodeHeaderBlock(t *testing.T) {
	long := string(bytes.Repeat([]byte("h"), 300))
	fields := decodeBlock(t, EncodeHeaderBlock("POST", long, "/echo", make([]byte, 1024)))
	want := map[string]string{
		":method": "POST", ":scheme": "http", ":path": "/echo", ":authority": long,
		"content-type": "application/octet-stream", "content-length": "1024",
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("%v = %q, want %q", k, fields[k], v)
		}
	}
	if len(fields) != len(want) {
		t.Errorf("decoded %v, want %v", fields, want)
	}

	get := decodeBlock(t, EncodeHeaderBlock("GET", "[::1]", "/echo", nil))
	if get[":method"] != "GET" || get["content-length"] != "" || len(get) != 4 {
		t.Errorf("GET decoded %v", get)
	}
}

// TestRequestFrames reads an encoded request back with x/net's framer: one
// HEADERS frame, the body in DATA frames no bigger than a peer has to take,
// and every frame on the stream it was written on.
func TestRequestFrames(t *testing.T) {
	body := make([]byte, 40000)
	rand.Read(body)
	r := NewRequest("POST", "127.0.0.1", "/echo", body)
	for _, id := range []uint32{1, 12345, maxStreamID} {
		fr := http2.NewFramer(nil, bytes.NewReader(r.AppendTo(nil, id)))
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		h, ok := f.(*http2.HeadersFrame)
		if !ok || h.StreamID != id || !h.HeadersEnded() || h.StreamEnded() {
			t.Fatalf("first frame %v", f)
		}
		var got []byte
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			d := f.(*http2.DataFrame)
			if d.StreamID != id || len(d.Data()) > defaultMaxFrameSize {
				t.Fatalf("DATA frame %v", d)
			}
			got = append(got, d.Data()...)
			if d.StreamEnded() {
				break
			}
		}
		if !bytes.Equal(got, body) {
			t.Fatal("body differs")
		}
		if _, err := fr.ReadFrame(); err != io.EOF {
			t.Fatalf("after the request: %v", err)
		}
	}

	get := NewRequest("GET", "127.0.0.1", "/echo", nil)
	f, _ := http2.NewFramer(nil, bytes.NewReader(get.AppendTo(nil, 3))).ReadFrame()
	if h := f.(*http2.HeadersFrame); !h.StreamEnded() {
		t.Error("a GET without a body does not end its stream")
	}

	batch := r.Repeat(nil, 7, 3)
	if len(batch) != 3*r.Len() {
		t.Fatalf("Repeat is %d bytes", len(batch))
	}
	fr := http2.NewFramer(nil, bytes.NewReader(batch))
	var ids []uint32
	for {
		f, err := fr.ReadFrame()
		if err == io.EOF {
			break
		}
		if h, ok := f.(*http2.HeadersFrame); ok {
			ids = append(ids, h.StreamID)
		}
	}
	if len(ids) != 3 || ids[0] != 7 || ids[1] != 9 || ids[2] != 11 {
		t.Errorf("Repeat streams %v", ids)
	}
}

func TestBatchSize(t *testing.T) {
	if n, tick := BatchSize(1100, 200, 16*1024); n != 10 || tick != 20 {
		t.Errorf("BatchSize = %d per batch, %d a second", n, tick)
	}
	if n, tick := BatchSize(32*1024, 7, 16*1024); n != 1 || tick != 7 {
		t.Errorf("a request bigger than the batch: %d per batch, %d a second", n, tick)
	}
	for _, c := range []struct {
		batch, rate int
		ok          bool
	}{{0, 200, true}, {1, 200, true}, {25, 200, true}, {200, 200, true}, {-1, 200, false}, {3, 200, false}, {400, 200, false}} {
		if err := ValidateBatch(c.batch, c.rate); (err == nil) != c.ok {
			t.Errorf("ValidateBatch(%d, %d) = %v", c.batch, c.rate, err)
		}
	}
}

// h2cServer is net/http serving HTTP/2 in cleartext only, the way the
// benchmark's nethttp server does, echoing /echo.
func h2cServer(t *testing.T, maxStreams int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Protocols: &protocols,
		HTTP2:     &http.HTTP2Config{MaxConcurrentStreams: maxStreams},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Write(body)
		}),
	}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return ln.Addr().String()
}

func dialTest(t *testing.T, addr string) *ClientConn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	cc, err := NewClientConn(conn, 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	select {
	case <-cc.SettingsSeen():
	case <-time.After(5 * time.Second):
		t.Fatal("no SETTINGS from the server")
	}
	return cc
}

func TestClientConnDo(t *testing.T) {
	addr := h2cServer(t, 50)
	cc := dialTest(t, addr)
	if got := cc.MaxStreams(); got != 50 {
		t.Errorf("MaxStreams = %d, want the server's 50", got)
	}

	get := cc.Do(NewRequest("GET", "127.0.0.1", "/echo", nil), true, nil)
	if get.Err != nil || get.Status != 200 || get.N != 0 {
		t.Fatalf("GET = %+v", get)
	}

	// A body bigger than the server's stream window has to wait for its
	// WINDOW_UPDATEs, and one bigger than a frame goes in several.
	for _, size := range []int{1, 1024, 16385, 3 << 20} {
		body := make([]byte, size)
		rand.Read(body)
		res := cc.Do(NewRequest("POST", "127.0.0.1", "/echo", body), true, nil)
		if res.Err != nil || res.Status != 200 || res.N != size || !bytes.Equal(res.Body, body) {
			t.Fatalf("POST %d bytes = status %d, %d bytes, %v", size, res.Status, res.N, res.Err)
		}
		if res := cc.Do(NewRequest("POST", "127.0.0.1", "/echo", body), false, nil); res.Err != nil || res.N != size || res.Body != nil && len(res.Body) != 0 {
			t.Fatalf("POST %d bytes without keeping the body = %d bytes, %v", size, res.N, res.Err)
		}
	}

	// More at once than the server allows: the ones over the limit wait for
	// a slot rather than being refused.
	body := make([]byte, 2048)
	req := NewRequest("POST", "127.0.0.1", "/echo", body)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := cc.Do(req, false, nil); res.Err != nil || res.Status != 200 || res.N != len(body) {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Errorf("%d of 200 concurrent requests failed", failed.Load())
	}
}

func TestClientConnWriteBatch(t *testing.T) {
	addr := h2cServer(t, 64)
	cc := dialTest(t, addr)
	body := make([]byte, 1024)
	rand.Read(body)
	req := NewRequest("POST", "127.0.0.1", "/echo", body)

	var ok, bad atomic.Int64
	cc.OnResult = func(res Result) {
		if res.Err == nil && res.Status == 200 && bytes.Equal(res.Body, body) {
			ok.Add(1)
		} else {
			bad.Add(1)
		}
	}
	// Over the server's limit: refused whole, rather than half written.
	if n, err := cc.WriteBatch(req, 65, true); n != 0 || err != nil {
		t.Fatalf("WriteBatch over the stream limit = %d, %v", n, err)
	}
	sent := 0
	for sent < 1000 {
		n, err := cc.WriteBatch(req, 16, true)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			time.Sleep(time.Millisecond)
		}
		sent += n
	}
	deadline := time.Now().Add(5 * time.Second)
	for ok.Load()+bad.Load() < int64(sent) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if ok.Load() != int64(sent) || bad.Load() != 0 {
		t.Errorf("sent %d, %d answered right, %d wrong", sent, ok.Load(), bad.Load())
	}
}

// A connection that goes away fails what is in flight on it, and everything
// after, rather than leaving a caller waiting.
func TestClientConnClosed(t *testing.T) {
	addr := h2cServer(t, 10)
	cc := dialTest(t, addr)
	cc.Conn().Close()
	res := cc.Do(NewRequest("GET", "127.0.0.1", "/echo", nil), false, nil)
	if res.Err == nil {
		t.Fatalf("Do on a closed connection = %+v", res)
	}
	if cc.Err() == nil {
		t.Error("a closed connection reports no error")
	}
}
