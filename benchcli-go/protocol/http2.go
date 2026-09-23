// Package protocol is the HTTP/2 the benchmark client speaks: cleartext with
// prior knowledge (h2c, RFC 9113 section 3.3), requests encoded once, up
// front, and written as they are with only their stream identifiers patched
// in, and a connection that multiplexes them and reads the responses back
// without building an http.Response for every one.
//
// The client is the other half of every number in the reports, and on a
// single-node run it shares the machine with the server, so what it spends on
// a request is what the server does not get. net/http's Transport would spend
// a goroutine, a Request, a Response and a header map on each.
package protocol

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
)

// The frame types and flags the client writes (RFC 9113 section 6).
const (
	frameData         = 0x0
	frameHeaders      = 0x1
	frameSettings     = 0x4
	framePing         = 0x6
	frameWindowUpdate = 0x8

	flagEndStream  = 0x1
	flagAck        = 0x1
	flagEndHeaders = 0x4

	frameHeaderLen = 9

	// defaultMaxFrameSize is the largest frame payload a peer has to accept
	// before it says otherwise, and the size the client splits DATA at.
	defaultMaxFrameSize = 16384
	// defaultWindow is the flow-control window every stream and the
	// connection start with in each direction.
	defaultWindow = 65535
	// maxStreamID is the last stream identifier a connection can use.
	maxStreamID = 1<<31 - 1
)

// The client's receive windows. A stream carries one response and a response
// is one echoed body, so no stream's window is ever used up; the connection's
// is topped up by WINDOW_UPDATE whenever half of it has been consumed. Big
// enough that flow control never holds a server back, which is a property of
// the client, not of the server being measured.
const (
	ClientStreamWindow = 1 << 30
	ClientConnWindow   = 1 << 30
)

// clientPreface is what the client writes before anything else: the
// connection preface, a SETTINGS frame - no server push, and the stream
// window above - and the WINDOW_UPDATE that takes the connection's window
// from the default to ClientConnWindow.
var clientPreface = func() []byte {
	buf := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	settings := []byte{}
	settings = binary.BigEndian.AppendUint16(settings, 0x2) // SETTINGS_ENABLE_PUSH
	settings = binary.BigEndian.AppendUint32(settings, 0)
	settings = binary.BigEndian.AppendUint16(settings, 0x4) // SETTINGS_INITIAL_WINDOW_SIZE
	settings = binary.BigEndian.AppendUint32(settings, ClientStreamWindow)
	buf = appendFrameHeader(buf, len(settings), frameSettings, 0, 0)
	buf = append(buf, settings...)
	return appendWindowUpdate(buf, 0, ClientConnWindow-defaultWindow)
}()

// ClientPreface returns a copy of what a connection opens with.
func ClientPreface() []byte {
	return append([]byte(nil), clientPreface...)
}

func appendFrameHeader(dst []byte, length int, typ, flags byte, streamID uint32) []byte {
	return append(dst, byte(length>>16), byte(length>>8), byte(length), typ, flags,
		byte(streamID>>24)&0x7f, byte(streamID>>16), byte(streamID>>8), byte(streamID))
}

func appendWindowUpdate(dst []byte, streamID uint32, increment uint32) []byte {
	dst = appendFrameHeader(dst, 4, frameWindowUpdate, 0, streamID)
	return binary.BigEndian.AppendUint32(dst, increment&0x7fffffff)
}

func appendSettingsAck(dst []byte) []byte {
	return appendFrameHeader(dst, 0, frameSettings, flagAck, 0)
}

func appendPingAck(dst []byte, data [8]byte) []byte {
	dst = appendFrameHeader(dst, 8, framePing, flagAck, 0)
	return append(dst, data[:]...)
}

// appendHeaders is one HEADERS frame carrying the whole of block, which the
// client's header blocks are always small enough for.
func appendHeaders(dst []byte, streamID uint32, block []byte, endStream bool) []byte {
	flags := byte(flagEndHeaders)
	if endStream {
		flags |= flagEndStream
	}
	dst = appendFrameHeader(dst, len(block), frameHeaders, flags, streamID)
	return append(dst, block...)
}

// appendData is data as DATA frames of at most defaultMaxFrameSize bytes, the
// last one ending the stream when endStream is set.
func appendData(dst []byte, streamID uint32, data []byte, endStream bool) []byte {
	for {
		n := min(len(data), defaultMaxFrameSize)
		flags := byte(0)
		if endStream && n == len(data) {
			flags = flagEndStream
		}
		dst = appendFrameHeader(dst, n, frameData, flags, streamID)
		dst = append(dst, data[:n]...)
		data = data[n:]
		if len(data) == 0 {
			return dst
		}
	}
}

// EncodeHeaderBlock is a request's header block in HPACK (RFC 7541): method,
// scheme, path and authority, and - for a body, even an empty one on a method
// that carries one - its content-type and content-length.
//
// Every field is either indexed from the static table or a literal that is
// not added to the dynamic table, so the block leaves the table as it found
// it: the same bytes decode the same way on every stream of every connection,
// which is what lets a request be encoded once and written again and again.
// It is what the Huffman-free, index-free end of HPACK looks like, and it
// costs a server's decoder no more than any other encoding would.
func EncodeHeaderBlock(method, authority, path string, body []byte) []byte {
	block := make([]byte, 0, 64+len(authority)+len(path))
	switch method {
	case "GET":
		block = append(block, 0x82) // static 2, :method GET
	case "POST":
		block = append(block, 0x83) // static 3, :method POST
	default:
		block = appendLiteral(block, 2, method)
	}
	block = append(block, 0x86) // static 6, :scheme http
	block = appendLiteral(block, 4, path)
	block = appendLiteral(block, 1, authority)
	if body != nil || method == "POST" || method == "PUT" {
		block = appendLiteral(block, 31, "application/octet-stream")
		block = appendLiteral(block, 28, strconv.Itoa(len(body)))
	}
	return block
}

// appendLiteral is a literal header field without indexing whose name is
// static table entry nameIndex (RFC 7541 section 6.2.2), its value a plain
// string literal.
func appendLiteral(dst []byte, nameIndex int, value string) []byte {
	dst = appendHpackInt(dst, 4, 0x00, uint64(nameIndex))
	dst = appendHpackInt(dst, 7, 0x00, uint64(len(value)))
	return append(dst, value...)
}

// appendHpackInt is an HPACK integer with an n-bit prefix, the bits above
// the prefix in the first byte taken from first (RFC 7541 section 5.1).
func appendHpackInt(dst []byte, n uint, first byte, v uint64) []byte {
	limit := uint64(1)<<n - 1
	if v < limit {
		return append(dst, first|byte(v))
	}
	dst = append(dst, first|byte(limit))
	v -= limit
	for v >= 128 {
		dst = append(dst, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// Request is one request, encoded: its HEADERS frame followed by its body's
// DATA frames, with stream identifier 0 wherever a frame header carries one.
// AppendTo writes it on a stream by patching those in.
type Request struct {
	Block []byte
	Body  []byte

	encoded []byte
	// idOffsets are where in encoded each frame's stream identifier is.
	idOffsets []int
}

// NewRequest encodes a request once for every stream it will be sent on.
func NewRequest(method, authority, path string, body []byte) *Request {
	r := &Request{Block: EncodeHeaderBlock(method, authority, path, body), Body: body}
	hasBody := len(body) > 0
	r.idOffsets = append(r.idOffsets, 5)
	r.encoded = appendHeaders(nil, 0, r.Block, !hasBody)
	if hasBody {
		for off := 0; off < len(body); off += defaultMaxFrameSize {
			r.idOffsets = append(r.idOffsets, len(r.encoded)+5)
			end := min(off+defaultMaxFrameSize, len(body))
			r.encoded = appendData(r.encoded, 0, body[off:end], end == len(body))
		}
	}
	return r
}

// Len is how many bytes the request is on the wire.
func (r *Request) Len() int { return len(r.encoded) }

// AppendTo is the whole request, on streamID.
func (r *Request) AppendTo(dst []byte, streamID uint32) []byte {
	start := len(dst)
	dst = append(dst, r.encoded...)
	for _, off := range r.idOffsets {
		binary.BigEndian.PutUint32(dst[start+off:], streamID&0x7fffffff)
	}
	return dst
}

// BatchSize is how many requests of reqLen bytes fit in maxLen, and no fewer
// than one - fewer if that many would not divide rate - with how many times a
// second a batch has to be written to send rate requests a second. The
// multiplexing benchmark writes its requests this way.
func BatchSize(reqLen, rate, maxLen int) (int, int) {
	batch := maxLen / reqLen
	if batch < 1 {
		batch = 1
	}
	for (batch > 1) && (rate%batch != 0) {
		batch--
	}
	return batch, rate / batch
}

// ValidateBatch checks a requested batch against the send rate: 0 asks for
// the batch BatchSize works out, and anything else has to divide rate, since
// the batch is written a whole number of times a second.
func ValidateBatch(batch, rate int) error {
	switch {
	case batch < 0:
		return fmt.Errorf("batch %d: want 0, which fits as many requests as -rbs holds, or more", batch)
	case batch > 0 && rate%batch != 0:
		return fmt.Errorf("batch %d does not divide the send rate %d, so no whole number of"+
			" writes a second sends it; pick a divisor of %d", batch, rate, rate)
	}
	return nil
}

// Repeat is r on n consecutive streams from firstID, as one buffer.
func (r *Request) Repeat(dst []byte, firstID uint32, n int) []byte {
	dst = slices.Grow(dst, n*len(r.encoded))
	for i := 0; i < n; i++ {
		dst = r.AppendTo(dst, firstID+uint32(2*i))
	}
	return dst
}
