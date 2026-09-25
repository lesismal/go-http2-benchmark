package benchrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"go-http2-benchmark/benchcli-go/connections"
	"go-http2-benchmark/benchcli-go/protocol"
	"go-http2-benchmark/benchcli-go/report"
	"go-http2-benchmark/config"
	"go-http2-benchmark/logging"

	"github.com/lesismal/perf"
	"golang.org/x/time/rate"
)

// BenchRate is HTTP/2 multiplexing at a rate the client sets: every connection
// is sent SendRate requests a second, a batch of them - each on a stream of
// its own - in one write, without waiting for the responses to the ones
// before, and each connection's reader counts the responses as they come. A
// connection with more than a few batches unanswered, or with no room under
// the server's stream limit or send window for another, is skipped until the
// server catches up, so a server slower than the rate is measured by what it
// answered rather than by how deep a queue the client could build in front of
// it.
type BenchRate struct {
	Framework   string
	Ip          string
	Duration    time.Duration
	Concurrency int
	SendRate    int
	BatchSize   int
	// Batch is how many requests go into one write, or 0 for as many as
	// fit in BatchSize bytes and divide SendRate.
	Batch      int
	Payload    int
	SendLimit  int
	PsInterval time.Duration

	ServerPid int
	PsCounter *perf.PSCounter
	// Where the CPU and MEM columns come from; see config.SetupPS.
	PsSource config.PSSource

	Conns []*connections.Conn

	wbuffer []byte
	request *protocol.Request

	limitFn    func()
	checkValid bool

	batch    int
	tickRate int

	sendTimes int64
	sendBytes int64
	recvTimes int64
	recvBytes int64
	// answered counts every response, whatever it said, where recvTimes
	// counts only the 200s with the body that was sent.
	answered int64
	// What recvTimes and recvBytes were when the run ended. The readers
	// keep counting what arrives later, which the report leaves out.
	endRecvTimes int64
	endRecvBytes int64

	onBenchmark  func()
	pprofDataCPU []byte
	pprofDataMEM []byte
}

type conn struct {
	*connections.Conn
	cc      *protocol.ClientConn
	sendCnt int64
	recvCnt int64
}

// maxBatchesInFlight is how many batches a connection may have unanswered
// before it is skipped for a tick.
const maxBatchesInFlight = 4

func New(framework string, serverPid int, ip string, conns []*connections.Conn, checkValid bool) *BenchRate {
	return &BenchRate{
		Framework:  framework,
		Ip:         ip,
		Conns:      conns,
		limitFn:    func() {},
		checkValid: checkValid,
		ServerPid:  serverPid,
	}
}

func (br *BenchRate) Run() {
	br.init()
	defer br.clean()

	conns := make([]*conn, 0, len(br.Conns))
	for _, c := range br.Conns {
		if cc := c.Client(); cc.Err() != nil {
			if err := c.Redial(cc); err != nil {
				logging.Printf("%v: redial %v failed, leaving the connection out: %v", report.BenchMultiplexName, c.Addr, err)
				continue
			}
		}
		rc := &conn{Conn: c, cc: c.Client()}
		// Every response comes back through here, on the connection's
		// reader. Unanswered is unanswered whatever the response said, so
		// the in-flight count goes down either way; only a 200 with the body
		// that was sent counts as a response in the report.
		rc.cc.OnResult = func(res protocol.Result) {
			atomic.AddInt64(&rc.recvCnt, 1)
			atomic.AddInt64(&br.answered, 1)
			if res.Err != nil || res.Status != 200 || res.N != br.Payload {
				return
			}
			if br.checkValid && !bytes.Equal(res.Body, br.wbuffer) {
				return
			}
			atomic.AddInt64(&br.recvTimes, 1)
			atomic.AddInt64(&br.recvBytes, int64(res.N))
		}
		conns = append(conns, rc)
	}
	if len(conns) == 0 {
		logging.Fatalf("%v: no connections to run on", report.BenchMultiplexName)
	}
	br.checkLimits(conns)

	connTeams := make([][]*conn, br.Concurrency)
	for i, c := range conns {
		connTeams[i%len(connTeams)] = append(connTeams[i%len(connTeams)], c)
	}

	logging.Printf("%v for %.2f seconds, %d requests multiplexed per write ...", report.BenchMultiplexName, br.Duration.Seconds(), br.batch)

	if br.onBenchmark != nil {
		br.onBenchmark()
	}

	done := make(chan struct{})
	time.AfterFunc(br.Duration, func() {
		close(done)
	})

	writers := sync.WaitGroup{}
	for _, team := range connTeams {
		writers.Add(1)
		go func() {
			defer writers.Done()
			ticker := time.NewTicker(time.Second / time.Duration(br.tickRate))
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					br.doOnce(team)
				}
			}
		}()
	}
	writers.Wait()

	// The last batch written is still on its way back when the duration is
	// up, and a batch can be many requests. Give the server one tick more to
	// answer what it was sent, which is how long it would have had before the
	// next batch, and no longer: a server slower than the rate is measured by
	// what it answered in time, not by how long it took to drain. Then
	// snapshot the counters; whatever arrives later is not counted.
	grace := time.NewTimer(time.Second / time.Duration(br.tickRate))
	for atomic.LoadInt64(&br.answered) < atomic.LoadInt64(&br.sendTimes) {
		select {
		case <-grace.C:
			goto snapshot
		case <-time.After(time.Millisecond):
		}
	}
	grace.Stop()
snapshot:
	br.endRecvTimes, br.endRecvBytes = atomic.LoadInt64(&br.recvTimes), atomic.LoadInt64(&br.recvBytes)

	logging.Printf("%v for %.2f seconds done", report.BenchMultiplexName, br.Duration.Seconds())
}

// checkLimits stops a run no connection could send a batch in: one with
// more streams than the server allows open at once, or a request body larger
// than the window the server gives a new stream. A batch is written whole or
// not at all, so either would leave every tick skipped and the run reading 0.
func (br *BenchRate) checkLimits(conns []*conn) {
	maxStreams, window := conns[0].cc.MaxStreams(), conns[0].cc.InitialWindow()
	for _, c := range conns[1:] {
		maxStreams = min(maxStreams, c.cc.MaxStreams())
		window = min(window, c.cc.InitialWindow())
	}
	if br.batch > maxStreams {
		logging.Fatalf("%v: a batch of %d requests is more streams than the server allows open"+
			" on a connection (%d); lower -rpl, or raise the servers' -maxstreams",
			report.BenchMultiplexName, br.batch, maxStreams)
	}
	if int64(br.Payload) > window {
		logging.Fatalf("%v: a %d-byte request body is larger than the %d-byte window the server"+
			" gives a new stream, so no request fits in one write; lower -b",
			report.BenchMultiplexName, br.Payload, window)
	}
}

func (br *BenchRate) Stop() {

}

func (br *BenchRate) OnBenchmark(f func()) {
	br.onBenchmark = f
}

func (br *BenchRate) SetPprofData(cpu, mem []byte) {
	br.pprofDataCPU = cpu
	br.pprofDataMEM = mem
}

func (br *BenchRate) Report() *report.BenchRateReport {
	r := &report.BenchRateReport{
		BenchClient: "benchcli-go",
		Framework:   br.Framework,
		Lang:        config.FrameworkLang(br.Framework),
		Duration:    br.Duration.Nanoseconds(),
		Connections: len(br.Conns),
		Concurrency: br.Concurrency,
		SendRate:    br.SendRate,
		Batch:       br.batch,
		Payload:     br.Payload,
		SendTimes:   br.sendTimes,
		SendBytes:   br.sendBytes,
		RecvTimes:   br.endRecvTimes,
		RecvBytes:   br.endRecvBytes,
	}
	r.SetPprofData(br.pprofDataCPU, br.pprofDataMEM)
	var psErr error
	br.PsCounter, psErr = br.psInfo()
	if psErr != nil {
		logging.Printf("%v: resource statistics for %v incomplete, CPU EER and MEM EER will read 0: %v",
			report.BenchMultiplexName, br.Framework, psErr)
	}
	if br.PsCounter != nil {
		r.CPUMin = br.PsCounter.CPUMin()
		r.CPUAvg = br.PsCounter.CPUAvg()
		r.CPUMax = br.PsCounter.CPUMax()
		r.MEMRSSMin = br.PsCounter.MEMRSSMin()
		r.MEMRSSAvg = br.PsCounter.MEMRSSAvg()
		r.MEMRSSMax = br.PsCounter.MEMRSSMax()
		r.EchoEER = report.EER(report.RateTPS(r.RecvTimes, r.Duration), r.CPUAvg)
		r.MEMEER = report.MEMEER(report.RateTPS(r.RecvTimes, r.Duration), r.MEMRSSAvg)
	}
	r.TPS = int64(math.Floor(report.RateTPS(r.RecvTimes, r.Duration)))
	return r
}

// psInfo reads the server's resource samples from wherever this run takes
// them; see BenchEcho.psInfo.
func (br *BenchRate) psInfo() (*perf.PSCounter, error) {
	if br.PsSource != nil {
		return br.PsSource.PsInfo()
	}
	return config.GetFrameworkPsInfo(br.Framework, br.Ip)
}

func (br *BenchRate) init() {
	if br.Duration <= 0 {
		br.Duration = time.Second * 10
	}
	if br.Concurrency <= 0 {
		br.Concurrency = 50000
	}
	if br.Concurrency > len(br.Conns) {
		br.Concurrency = len(br.Conns)
	}
	if br.Concurrency <= 0 {
		logging.Fatalf("%v: no connections to run on", report.BenchMultiplexName)
	}
	if br.SendRate <= 0 {
		br.SendRate = 1
	}
	if br.Payload <= 0 {
		br.Payload = 1024
	}

	br.wbuffer = make([]byte, br.Payload)
	rand.Read(br.wbuffer)
	br.request = protocol.NewRequest("POST", connections.Host(br.Ip), config.EchoPath, br.wbuffer)
	if err := protocol.ValidateBatch(br.Batch, br.SendRate); err != nil {
		logging.Fatalf("%v: %v", report.BenchMultiplexName, err)
	}
	if br.Batch > 0 {
		br.batch, br.tickRate = br.Batch, br.SendRate/br.Batch
	} else {
		br.batch, br.tickRate = protocol.BatchSize(br.request.Len(), br.SendRate, br.BatchSize)
	}
	if br.tickRate <= 0 || br.batch <= 0 {
		logging.Fatalf("%v got a wrong tickRate: %v, or batch: %v", report.BenchMultiplexName, br.tickRate, br.batch)
	}

	if br.PsInterval <= 0 {
		br.PsInterval = time.Second
	}

	if br.SendLimit > 0 {
		limiter := rate.NewLimiter(rate.Every(1*time.Second), br.SendLimit)
		br.limitFn = func() {
			limiter.WaitN(context.Background(), br.batch)
		}
	}
}

func (br *BenchRate) clean() {
	br.limitFn = func() {}
}

func (br *BenchRate) doOnce(conns []*conn) {
	for _, c := range conns {
		if atomic.LoadInt64(&c.sendCnt)-atomic.LoadInt64(&c.recvCnt) >= int64(br.batch*maxBatchesInFlight) {
			continue
		}
		br.limitFn()
		// Nothing written, and no error, is a connection that has no room
		// for the batch under the server's stream limit or send window yet:
		// skipped for this tick like one with too many batches unanswered.
		n, err := c.cc.WriteBatch(br.request, br.batch, br.checkValid)
		if err == nil && n > 0 {
			atomic.AddInt64(&br.sendTimes, int64(n))
			atomic.AddInt64(&br.sendBytes, int64(n*br.Payload))
			atomic.AddInt64(&c.sendCnt, int64(n))
		}
	}
}
