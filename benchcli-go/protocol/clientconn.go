package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	ErrConnClosed       = errors.New("http2: connection closed")
	ErrGoAway           = errors.New("http2: server sent GOAWAY")
	ErrStreamReset      = errors.New("http2: stream reset by the server")
	ErrStreamsExhausted = errors.New("http2: connection has used up its stream identifiers")
	ErrNoStatus         = errors.New("http2: response has no :status")
	ErrBodyTooLarge     = errors.New("http2: response body larger than the client accepts")
	ErrPushPromise      = errors.New("http2: server sent PUSH_PROMISE with push disabled")
)

// MaxBodySize bounds a response body the client will keep, so that a broken
// server cannot make it allocate without limit.
const MaxBodySize = 64 << 20

// Result is how a stream ended: its status and body, or the error that ended
// it instead. Body is only kept for a stream that asked for it; N is its
// length either way.
type Result struct {
	Status int
	Body   []byte
	N      int
	Err    error
}

// stream is one request in flight. Only the connection's reader writes its
// response fields, until it hands the stream back through done or onResult.
type stream struct {
	id   uint32
	keep bool
	// sendWindow is how much more of the request body the server will take
	// on this stream, guarded by ClientConn.mu. Only a request whose body did
	// not fit in the window at once uses it.
	sendWindow int64
	status     int
	body       []byte
	n          int
	err        error
	// done is how a waiting Do learns its stream has ended. A stream written
	// by WriteBatch has none, and its result goes to ClientConn.OnResult.
	done chan struct{}
}

// Two pools, since what a stream ends through is fixed when it is made: a
// Do's has a done channel, and a batch stream must not.
var (
	doStreamPool    = sync.Pool{New: func() any { return &stream{done: make(chan struct{}, 1)} }}
	batchStreamPool = sync.Pool{New: func() any { return &stream{} }}
)

var writeBufPool = sync.Pool{New: func() any {
	buf := make([]byte, 0, 4096)
	return &buf
}}

// ClientConn is one HTTP/2 connection to the server, carrying as many
// requests at once as the server allows. A goroutine of its own reads every
// frame the server sends, answers SETTINGS and PING, keeps the flow-control
// windows, and hands each response to the request that is waiting for it.
type ClientConn struct {
	conn net.Conn
	fr   *http2.Framer
	dec  *hpack.Decoder

	// wmu serializes writes. Streams are numbered while it is held, since the
	// server refuses a stream whose identifier is lower than one it has
	// already seen.
	wmu sync.Mutex

	mu   sync.Mutex
	cond *sync.Cond
	// streams is every stream the server has not finished answering.
	streams map[uint32]*stream
	// active counts those, and the ones about to be opened.
	active int
	nextID uint32
	// The send side of flow control, and the server's limits, as its
	// SETTINGS and WINDOW_UPDATE frames have set them.
	connSendWindow    int64
	peerInitialWindow int64
	peerMaxStreams    int
	// err is why the connection can carry no more requests.
	err error

	settingsSeen chan struct{}
	settingsOnce sync.Once
	readerDone   chan struct{}

	// OnResult is given the result of every stream WriteBatch opens, on the
	// reader goroutine. It has to be set before the first WriteBatch.
	OnResult func(Result)

	// Reader-only state: the header block being decoded, and how much of the
	// connection's receive window has been used since it was last topped up.
	hdrStream    *stream
	hdrStatus    int
	hdrEnd       bool
	recvConsumed int64
}

// NewClientConn opens HTTP/2 on conn - the preface and SETTINGS go out first -
// and starts reading it. readBufferSize sizes the reader the frames are read
// through.
func NewClientConn(conn net.Conn, readBufferSize int) (*ClientConn, error) {
	cc := &ClientConn{
		conn:              conn,
		streams:           make(map[uint32]*stream),
		nextID:            1,
		connSendWindow:    defaultWindow,
		peerInitialWindow: defaultWindow,
		// What a client may assume until the server says, as net/http's
		// does: RFC 9113 sets no initial limit, and a server that settles on
		// fewer refuses the streams over it.
		peerMaxStreams: 100,
		settingsSeen:   make(chan struct{}),
		readerDone:     make(chan struct{}),
	}
	cc.cond = sync.NewCond(&cc.mu)
	cc.fr = http2.NewFramer(nil, bufio.NewReaderSize(conn, readBufferSize))
	cc.fr.SetReuseFrames()
	cc.dec = hpack.NewDecoder(4096, cc.onHeaderField)
	cc.dec.SetMaxStringLength(MaxBodySize)
	if _, err := conn.Write(clientPreface); err != nil {
		conn.Close()
		return nil, err
	}
	go cc.readLoop()
	return cc, nil
}

// SettingsSeen is closed once the server's first SETTINGS frame has arrived.
func (cc *ClientConn) SettingsSeen() <-chan struct{} { return cc.settingsSeen }

// Conn is the connection the client speaks HTTP/2 on.
func (cc *ClientConn) Conn() net.Conn { return cc.conn }

// Err is why the connection can take no more requests, or nil while it can.
func (cc *ClientConn) Err() error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.err
}

// MaxStreams is how many streams the server lets the client have open at
// once, as its SETTINGS said.
func (cc *ClientConn) MaxStreams() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.peerMaxStreams
}

// InitialWindow is how much of a request body the server takes on a new
// stream before it has to send WINDOW_UPDATE for it.
func (cc *ClientConn) InitialWindow() int64 {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.peerInitialWindow
}

// Close closes the connection and waits for the reader, which fails whatever
// was still in flight, to exit.
func (cc *ClientConn) Close() error {
	err := cc.conn.Close()
	<-cc.readerDone
	return err
}

// Do sends r on a new stream and waits for the response. Its body is appended
// to dst[:0] when keep is set, so a caller that passes the same buffer back
// in reads every response without allocating; otherwise it is only counted.
func (cc *ClientConn) Do(r *Request, keep bool, dst []byte) Result {
	s := doStreamPool.Get().(*stream)
	s.keep, s.body = keep, dst[:0]

	// A slot first, without holding wmu: the reader may need wmu to answer a
	// PING before it can finish the stream this is waiting on.
	cc.mu.Lock()
	for cc.err == nil && cc.active >= cc.peerMaxStreams {
		cc.cond.Wait()
	}
	if cc.err != nil {
		err := cc.err
		cc.mu.Unlock()
		putStream(s)
		return Result{Err: err}
	}
	cc.active++
	cc.mu.Unlock()

	if err := cc.writeStream(s, r); err != nil {
		// The stream is in the map; the reader fails it once the connection
		// closed below ends its read.
		cc.conn.Close()
	}
	<-s.done
	res := Result{Status: s.status, Body: s.body, N: s.n, Err: s.err}
	putStream(s)
	return res
}

// writeStream numbers s, writes r's HEADERS on it and as much of the body as
// the flow-control windows let through, and waits for more window for the
// rest.
func (cc *ClientConn) writeStream(s *stream, r *Request) error {
	bufp := writeBufPool.Get().(*[]byte)
	defer writeBufPool.Put(bufp)

	cc.wmu.Lock()
	cc.mu.Lock()
	if cc.err != nil {
		err := cc.err
		cc.active--
		cc.mu.Unlock()
		cc.wmu.Unlock()
		s.err = err
		s.done <- struct{}{}
		return nil
	}
	if cc.nextID > maxStreamID {
		cc.active--
		cc.failLocked(ErrStreamsExhausted)
		cc.mu.Unlock()
		cc.wmu.Unlock()
		s.err = ErrStreamsExhausted
		s.done <- struct{}{}
		return ErrStreamsExhausted
	}
	s.id = cc.nextID
	cc.nextID += 2
	cc.streams[s.id] = s
	s.sendWindow = cc.peerInitialWindow
	body := r.Body
	take := cc.takeWindowLocked(s, len(body))
	cc.mu.Unlock()

	buf := (*bufp)[:0]
	if take == len(body) {
		buf = r.AppendTo(buf, s.id)
	} else {
		buf = appendHeaders(buf, s.id, r.Block, false)
		if take > 0 {
			buf = appendData(buf, s.id, body[:take], false)
		}
	}
	_, err := cc.conn.Write(buf)
	cc.wmu.Unlock()
	*bufp = buf[:0]
	if err != nil {
		return err
	}
	body = body[take:]

	for len(body) > 0 {
		cc.mu.Lock()
		for cc.err == nil && cc.streams[s.id] == s && min(cc.connSendWindow, s.sendWindow) <= 0 {
			cc.cond.Wait()
		}
		if cc.err != nil || cc.streams[s.id] != s {
			// The connection failed, or the server reset the stream or
			// answered it without the rest of the body: either way it has
			// ended, and the reader has said how.
			cc.mu.Unlock()
			return nil
		}
		take = cc.takeWindowLocked(s, len(body))
		cc.mu.Unlock()

		cc.wmu.Lock()
		buf = appendData((*bufp)[:0], s.id, body[:take], take == len(body))
		_, err = cc.conn.Write(buf)
		cc.wmu.Unlock()
		*bufp = buf[:0]
		if err != nil {
			return err
		}
		body = body[take:]
	}
	return nil
}

// takeWindowLocked takes up to n bytes of send window for s off both it and
// the connection.
func (cc *ClientConn) takeWindowLocked(s *stream, n int) int {
	take := int64(n)
	take = min(take, max(cc.connSendWindow, 0), max(s.sendWindow, 0))
	cc.connSendWindow -= take
	s.sendWindow -= take
	return int(take)
}

// WriteBatch writes r on n new streams in one write, without waiting for the
// responses, which go to OnResult as they arrive; each keeps its body for it
// when keep is set. It writes nothing, and returns 0, when the batch would
// take the connection over the server's stream limit or its send window:
// the caller is ahead of the server, and a batch that has to wait for it is
// one it is better off skipping.
func (cc *ClientConn) WriteBatch(r *Request, n int, keep bool) (int, error) {
	bodyLen := int64(len(r.Body))
	cc.mu.Lock()
	if cc.err != nil {
		err := cc.err
		cc.mu.Unlock()
		return 0, err
	}
	if cc.active+n > cc.peerMaxStreams || cc.connSendWindow < int64(n)*bodyLen || cc.peerInitialWindow < bodyLen {
		cc.mu.Unlock()
		return 0, nil
	}
	cc.active += n
	cc.connSendWindow -= int64(n) * bodyLen
	cc.mu.Unlock()

	bufp := writeBufPool.Get().(*[]byte)
	defer writeBufPool.Put(bufp)

	cc.wmu.Lock()
	cc.mu.Lock()
	if cc.err != nil || cc.nextID+uint32(2*(n-1)) > maxStreamID {
		if cc.err == nil {
			cc.failLocked(ErrStreamsExhausted)
		}
		err := cc.err
		cc.active -= n
		cc.mu.Unlock()
		cc.wmu.Unlock()
		return 0, err
	}
	first := cc.nextID
	cc.nextID += uint32(2 * n)
	for i := 0; i < n; i++ {
		s := batchStreamPool.Get().(*stream)
		s.id = first + uint32(2*i)
		s.keep = keep
		s.body = s.body[:0]
		s.sendWindow = cc.peerInitialWindow - bodyLen
		cc.streams[s.id] = s
	}
	cc.mu.Unlock()

	buf := r.Repeat((*bufp)[:0], first, n)
	_, err := cc.conn.Write(buf)
	cc.wmu.Unlock()
	*bufp = buf[:0]
	if err != nil {
		cc.conn.Close()
		return 0, err
	}
	return n, nil
}

// failLocked marks the connection unusable and wakes whatever waits on it.
// The streams still in flight are the reader's to fail, as it exits.
func (cc *ClientConn) failLocked(err error) {
	if cc.err == nil {
		cc.err = err
	}
	cc.cond.Broadcast()
}

func putStream(s *stream) {
	s.id, s.keep, s.status, s.n, s.err, s.sendWindow = 0, false, 0, 0, nil, 0
	if s.done != nil {
		// The body is its caller's.
		s.body = nil
		doStreamPool.Put(s)
		return
	}
	// A batch stream keeps its own buffer for the next one, if it is small.
	if cap(s.body) > 64<<10 {
		s.body = nil
	}
	batchStreamPool.Put(s)
}

// writeControl writes a frame the protocol owes the server: a SETTINGS or
// PING acknowledgement, or a WINDOW_UPDATE.
func (cc *ClientConn) writeControl(frame []byte) error {
	cc.wmu.Lock()
	_, err := cc.conn.Write(frame)
	cc.wmu.Unlock()
	return err
}

func (cc *ClientConn) readLoop() {
	defer close(cc.readerDone)
	err := cc.read()
	cc.conn.Close()

	cc.mu.Lock()
	if err == nil {
		err = ErrConnClosed
	}
	cc.failLocked(err)
	err = cc.err
	streams := cc.streams
	cc.streams = map[uint32]*stream{}
	cc.active -= len(streams)
	cc.mu.Unlock()

	for _, s := range streams {
		s.err = fmt.Errorf("%w: %w", ErrConnClosed, err)
		cc.finish(s)
	}
	cc.settingsOnce.Do(func() { close(cc.settingsSeen) })
}

// read reads frames until the connection fails.
func (cc *ClientConn) read() error {
	for {
		f, err := cc.fr.ReadFrame()
		if err != nil {
			return err
		}
		switch f := f.(type) {
		case *http2.DataFrame:
			if err := cc.onData(f); err != nil {
				return err
			}
		case *http2.HeadersFrame:
			cc.startHeaders(f.StreamID, f.StreamEnded())
			if err := cc.onHeaderBlock(f.HeaderBlockFragment(), f.HeadersEnded()); err != nil {
				return err
			}
		case *http2.ContinuationFrame:
			if err := cc.onHeaderBlock(f.HeaderBlockFragment(), f.HeadersEnded()); err != nil {
				return err
			}
		case *http2.SettingsFrame:
			if f.IsAck() {
				continue
			}
			if err := cc.onSettings(f); err != nil {
				return err
			}
		case *http2.WindowUpdateFrame:
			cc.mu.Lock()
			if f.StreamID == 0 {
				cc.connSendWindow += int64(f.Increment)
			} else if s := cc.streams[f.StreamID]; s != nil {
				s.sendWindow += int64(f.Increment)
			}
			cc.cond.Broadcast()
			cc.mu.Unlock()
		case *http2.PingFrame:
			if !f.IsAck() {
				if err := cc.writeControl(appendPingAck(nil, f.Data)); err != nil {
					return err
				}
			}
		case *http2.RSTStreamFrame:
			if s := cc.take(f.StreamID); s != nil {
				s.err = fmt.Errorf("%w: %v", ErrStreamReset, f.ErrCode)
				cc.finish(s)
			}
		case *http2.GoAwayFrame:
			cc.onGoAway(f)
		case *http2.PushPromiseFrame:
			return ErrPushPromise
		}
	}
}

func (cc *ClientConn) onSettings(f *http2.SettingsFrame) error {
	cc.mu.Lock()
	err := f.ForeachSetting(func(setting http2.Setting) error {
		switch setting.ID {
		case http2.SettingInitialWindowSize:
			// A change applies to every stream open, by the difference
			// (RFC 9113 section 6.9.2).
			delta := int64(setting.Val) - cc.peerInitialWindow
			cc.peerInitialWindow = int64(setting.Val)
			for _, s := range cc.streams {
				s.sendWindow += delta
			}
		case http2.SettingMaxConcurrentStreams:
			cc.peerMaxStreams = int(min(setting.Val, 1<<20))
		}
		return nil
	})
	cc.cond.Broadcast()
	cc.mu.Unlock()
	if err != nil {
		return err
	}
	if err := cc.writeControl(appendSettingsAck(nil)); err != nil {
		return err
	}
	cc.settingsOnce.Do(func() { close(cc.settingsSeen) })
	return nil
}

func (cc *ClientConn) onGoAway(f *http2.GoAwayFrame) {
	cc.mu.Lock()
	cc.failLocked(fmt.Errorf("%w: %v", ErrGoAway, f.ErrCode))
	// Streams past the last one the server says it took were never
	// processed, and will never be answered.
	var refused []*stream
	for id, s := range cc.streams {
		if id > f.LastStreamID {
			refused = append(refused, s)
			delete(cc.streams, id)
		}
	}
	cc.active -= len(refused)
	cc.mu.Unlock()
	for _, s := range refused {
		s.err = cc.Err()
		cc.finish(s)
	}
}

func (cc *ClientConn) onData(f *http2.DataFrame) error {
	// Every byte of a DATA frame, padding included, counts against the
	// window, whichever stream it is on, answered or not.
	cc.recvConsumed += int64(f.Length)
	if cc.recvConsumed >= ClientConnWindow/2 {
		if err := cc.writeControl(appendWindowUpdate(nil, 0, uint32(cc.recvConsumed))); err != nil {
			return err
		}
		cc.recvConsumed = 0
	}

	cc.mu.Lock()
	s := cc.streams[f.StreamID]
	cc.mu.Unlock()
	if s == nil {
		// Reset or failed already.
		return nil
	}
	data := f.Data()
	s.n += len(data)
	if s.keep {
		if s.n > MaxBodySize {
			if s.err == nil {
				s.err = ErrBodyTooLarge
			}
		} else {
			s.body = append(s.body, data...)
		}
	}
	if f.StreamEnded() {
		cc.end(s)
	}
	return nil
}

// startHeaders begins a header block, which is decoded whether or not its
// stream is still wanted: the HPACK dynamic table is the connection's, and a
// block skipped would leave the decoder out of step with the server's encoder.
func (cc *ClientConn) startHeaders(id uint32, endStream bool) {
	cc.mu.Lock()
	cc.hdrStream = cc.streams[id]
	cc.mu.Unlock()
	cc.hdrStatus = 0
	cc.hdrEnd = endStream
}

func (cc *ClientConn) onHeaderField(field hpack.HeaderField) {
	if field.Name == ":status" {
		if status, err := strconv.Atoi(field.Value); err == nil {
			cc.hdrStatus = status
		}
	}
}

func (cc *ClientConn) onHeaderBlock(fragment []byte, ended bool) error {
	if _, err := cc.dec.Write(fragment); err != nil {
		return err
	}
	if !ended {
		return nil
	}
	if err := cc.dec.Close(); err != nil {
		return err
	}
	s := cc.hdrStream
	cc.hdrStream = nil
	if s == nil {
		return nil
	}
	switch {
	case cc.hdrStatus >= 200 && s.status == 0:
		s.status = cc.hdrStatus
	case cc.hdrStatus >= 100 && cc.hdrStatus < 200:
		// Informational: the final response is still to come.
	case cc.hdrStatus == 0 && s.status == 0 && s.err == nil:
		// Only trailers, which follow a status, may leave it out.
		s.err = ErrNoStatus
	}
	if cc.hdrEnd {
		cc.end(s)
	}
	return nil
}

// take removes a stream from the connection, or returns nil when it is gone
// already.
func (cc *ClientConn) take(id uint32) *stream {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	s := cc.streams[id]
	if s != nil {
		delete(cc.streams, id)
		cc.active--
		// Broadcast, since a Do waiting for a slot and one waiting for send
		// window wait on the same cond.
		cc.cond.Broadcast()
	}
	return s
}

// end is a stream the server has finished answering.
func (cc *ClientConn) end(s *stream) {
	if cc.take(s.id) == s {
		cc.finish(s)
	}
}

// finish hands a stream's result to whoever is waiting for it.
func (cc *ClientConn) finish(s *stream) {
	if s.done != nil {
		s.done <- struct{}{}
		return
	}
	if cc.OnResult != nil {
		cc.OnResult(Result{Status: s.status, Body: s.body, N: s.n, Err: s.err})
	}
	putStream(s)
}
