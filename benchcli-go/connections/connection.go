package connections

import (
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-http2-benchmark/benchcli-go/protocol"
	"go-http2-benchmark/benchcli-go/report"
	"go-http2-benchmark/config"
	"go-http2-benchmark/logging"

	"github.com/lesismal/perf"
)

// Conn is one HTTP/2 connection to the server, which every benchmark shares:
// BenchEcho's workers take turns on it, as many at once as -es lets them, and
// BenchMultiplex writes its batches to it.
type Conn struct {
	Addr string

	cc atomic.Pointer[protocol.ClientConn]
	// mu makes one Redial at a time: when several workers find the
	// connection broken, the first replaces it and the rest use the new one.
	mu sync.Mutex
	cs *Connections
}

// Client is the HTTP/2 connection this Conn is currently.
func (c *Conn) Client() *protocol.ClientConn { return c.cc.Load() }

// Broken reports whether the connection can take no more requests: it
// failed, or the server sent GOAWAY. Redial replaces it.
func (c *Conn) Broken() bool { return c.Client().Err() != nil }

// Redial replaces a broken connection with a new one to the same address. A
// connection another caller has already replaced is left alone.
func (c *Conn) Redial(broken *protocol.ClientConn) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Client() != broken {
		return nil
	}
	broken.Close()
	cc, err := c.cs.dial(c.Addr)
	if err != nil {
		return err
	}
	c.cc.Store(cc)
	return nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.Client().Close() }

type Connections struct {
	Framework      string
	Ip             string
	Concurrency    int
	NumConnections int
	DialTimeout    time.Duration
	RetryInterval  time.Duration
	RetryTimes     int
	EnalbeTPN      bool
	Percents       []int
	// ReadBufferSize sizes each connection's reader, which the echo
	// benchmark wants big enough for a whole response.
	ReadBufferSize int
	// MaxStreams is the most concurrent streams any server connection
	// allowed, as its SETTINGS said, and MinMaxStreams the fewest.
	MaxStreams    int
	MinMaxStreams int

	// All connected connections
	conns []*Conn

	Calculator *perf.Calculator

	mux         sync.Mutex
	serverIdx   uint32
	serverAddrs []string
	request     *protocol.Request
}

func New(framework, ip string, numConns int) *Connections {
	return &Connections{
		Framework:      framework,
		Ip:             ip,
		NumConnections: numConns,
	}
}

// Run dials the connections, each one a TCP connection, the HTTP/2 preface and
// SETTINGS exchanged, and one GET answered on it: a connection the kernel
// accepted is not yet one the server is serving. What this measures is the
// rate the server takes new clients on at, the way the WebSocket benchmark's
// handshake does.
func (cs *Connections) Run() {
	cs.init()
	defer cs.clean()

	logging.Printf("Dial Connections: [%v]", cs.NumConnections)
	logging.Printf("Dial Concurrency: [%v]", cs.Concurrency)
	done := make(chan struct{})
	logDone := make(chan struct{})

	go func() {
		defer func() {
			logging.Printf("Connections done: %v Success, %v Failed",
				atomic.LoadInt64(&cs.Calculator.Success), atomic.LoadInt64(&cs.Calculator.Failed))
			close(logDone)
		}()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for i := 1; true; i++ {
			select {
			case <-done:
				return
			case <-ticker.C:
				logging.Printf("%03d seconds passed, %v Connected ...", i, atomic.LoadInt64(&cs.Calculator.Success))
			}
		}
	}()

	logging.Printf("Connections start ...")
	cs.Calculator.Benchmark(cs.Concurrency, cs.NumConnections, cs.doOnce, cs.Percents)

	close(done)
	<-logDone

	for _, c := range cs.conns {
		n := c.Client().MaxStreams()
		cs.MaxStreams = max(cs.MaxStreams, n)
		if cs.MinMaxStreams == 0 || n < cs.MinMaxStreams {
			cs.MinMaxStreams = n
		}
	}
}

func (cs *Connections) Conns() []*Conn {
	return cs.conns
}

func (cs *Connections) Stop() {
	for _, c := range cs.conns {
		c.Close()
	}
}

func (cs *Connections) Report() report.Report {
	r := &report.ConnectionsReport{
		BenchClient: "benchcli-go",
		Framework:   cs.Framework,
		Lang:        config.FrameworkLang(cs.Framework),
		TPS:         cs.Calculator.TPS(),
		Min:         cs.Calculator.Min,
		Avg:         cs.Calculator.Avg,
		Max:         cs.Calculator.Max,

		Used:        int64(cs.Calculator.Used),
		Total:       cs.NumConnections,
		Success:     cs.Calculator.Success,
		Failed:      cs.Calculator.Failed,
		Concurrency: cs.Concurrency,
		MaxStreams:  cs.MinMaxStreams,
	}
	if cs.EnalbeTPN {
		r.TP50 = cs.Calculator.TPN(50)
		r.TP75 = cs.Calculator.TPN(75)
		r.TP90 = cs.Calculator.TPN(90)
		r.TP95 = cs.Calculator.TPN(95)
		r.TP99 = cs.Calculator.TPN(99)
	}
	return r
}

func (cs *Connections) init() {
	if cs.NumConnections <= 0 {
		cs.NumConnections = 1000
	}
	if cs.Concurrency <= 0 {
		cs.Concurrency = runtime.NumCPU() * 1000
	}
	if cs.Concurrency > cs.NumConnections {
		cs.Concurrency = cs.NumConnections
	}
	if cs.DialTimeout <= 0 {
		cs.DialTimeout = time.Second * 1
	}
	if cs.RetryInterval <= 0 {
		cs.RetryInterval = time.Second / 10
	}
	if cs.RetryTimes <= 0 {
		cs.RetryTimes = 3
	}
	if cs.ReadBufferSize <= 0 {
		cs.ReadBufferSize = 4096
	}
	if cs.EnalbeTPN {
		cs.Percents = []int{50, 75, 90, 95, 99}
	}

	cs.Calculator = perf.NewCalculator(fmt.Sprintf("%v-Connect", cs.Framework))

	addrs, err := config.GetFrameworkBenchmarkAddrs(cs.Framework, cs.Ip)
	if err != nil {
		logging.Fatalf("GetFrameworkBenchmarkAddrs(%v) failed: %v", cs.Framework, err)
	}
	cs.serverAddrs = addrs
	cs.request = protocol.NewRequest("GET", Host(cs.Ip), config.EchoPath, nil)
	cs.conns = make([]*Conn, 0, cs.NumConnections)
}

func (cs *Connections) clean() {
	cs.serverAddrs = nil
}

// Host is the Host header the clients send, for a server reached as ip: an
// IPv6 address in brackets, as it would be in a URL.
func Host(ip string) string {
	if strings.Contains(ip, ":") && !strings.HasPrefix(ip, "[") {
		return "[" + ip + "]"
	}
	return ip
}

// dial connects to addr, opens HTTP/2 on it, and has one request answered on
// the connection, which is what makes it one the server is serving. The
// server's SETTINGS have arrived by then too, so the connection knows how many
// streams it may open. -dt bounds all of it.
func (cs *Connections) dial(addr string) (*protocol.ClientConn, error) {
	conn, err := net.DialTimeout("tcp", addr, cs.DialTimeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(cs.DialTimeout))
	cc, err := protocol.NewClientConn(conn, cs.ReadBufferSize)
	if err != nil {
		return nil, err
	}
	res := cc.Do(cs.request, false, nil)
	err = res.Err
	if err == nil && res.Status != 200 {
		err = fmt.Errorf("GET %v: status %d", config.EchoPath, res.Status)
	}
	if err == nil {
		// The server sends SETTINGS before anything else, so this is only
		// ever a wait for one that answered without them, and the deadline
		// ends it: a reader that fails closes SettingsSeen too.
		<-cc.SettingsSeen()
		err = cc.Err()
	}
	if err != nil {
		cc.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return cc, nil
}

// doOnce dials one connection, retrying on failure.
func (cs *Connections) doOnce() error {
	var err error
	for i := 0; i < cs.RetryTimes; i++ {
		if i > 0 {
			time.Sleep(cs.RetryInterval)
		}
		addr := cs.serverAddrs[atomic.AddUint32(&cs.serverIdx, 1)%uint32(len(cs.serverAddrs))]
		var cc *protocol.ClientConn
		cc, err = cs.dial(addr)
		if err == nil {
			c := &Conn{Addr: addr, cs: cs}
			c.cc.Store(cc)
			cs.mux.Lock()
			cs.conns = append(cs.conns, c)
			cs.mux.Unlock()
			return nil
		}
	}
	return err
}
