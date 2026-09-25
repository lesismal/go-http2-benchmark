package benchecho

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	mrand "math/rand"
	"runtime"
	"sync"
	"time"

	"go-http2-benchmark/benchcli-go/connections"
	"go-http2-benchmark/benchcli-go/protocol"
	"go-http2-benchmark/benchcli-go/report"
	"go-http2-benchmark/config"
	"go-http2-benchmark/logging"

	"github.com/lesismal/perf"
	"golang.org/x/time/rate"
)

// BenchEcho is request/response over HTTP/2 connections: a POST of Payload
// random bytes to the echo route on a stream of its own, and the same bytes
// read back. Concurrency is how many requests are in flight at once, over all
// connections, and Streams how many of them one connection carries at most;
// with Streams at 1, the default, it is one request per connection at a time,
// as in go-http1-benchmark.
type BenchEcho struct {
	Framework   string
	Ip          string
	WarmupTimes int
	Total       int
	Concurrency int
	Streams     int
	Payload     int
	Limit       int
	EnalbeTPN   bool
	Percents    []int
	PsInterval  time.Duration

	Calculator *perf.Calculator

	ServerPid int
	PsCounter *perf.PSCounter
	// Where the CPU and MEM columns come from: this machine's own sampling of
	// the server process on a single-node run, and the server's /ps route
	// otherwise. See config.SetupPS.
	PsSource config.PSSource

	Conns []*connections.Conn

	pbuffers [][]byte            // payloads
	requests []*protocol.Request // the requests that carry them

	chConns chan *connections.Conn

	limitFn func()

	checkValid bool

	rbufferPool *sync.Pool

	onWarmup     func()
	onBenchmark  func()
	pprofDataCPU []byte
	pprofDataMEM []byte
}

func New(framework string, serverPid int, benchmarkTimes int, ip string, conns []*connections.Conn, checkValid bool) *BenchEcho {
	be := &BenchEcho{
		Framework:  framework,
		Ip:         ip,
		Total:      benchmarkTimes,
		Conns:      conns,
		limitFn:    func() {},
		checkValid: checkValid,
		ServerPid:  serverPid,
	}
	return be
}

func (be *BenchEcho) Run() {
	be.init()
	defer be.clean()

	logging.Printf("BenchEcho Warmup for %d times ...", be.WarmupTimes)
	if be.onWarmup != nil {
		be.onWarmup()
	}
	be.Calculator.Warmup(be.Concurrency, be.WarmupTimes, be.doOnce)
	logging.Printf("BenchEcho Warmup for %d times done", be.WarmupTimes)

	logging.Printf("BenchEcho for %d times ...", be.Total)
	if be.onBenchmark != nil {
		be.onBenchmark()
	}
	be.Calculator.Benchmark(be.Concurrency, be.Total, be.doOnce, be.Percents)
	logging.Printf("BenchEcho for %d times done", be.Total)
	if len(be.Calculator.FailedErrors) > 0 {
		logging.Printf("BenchEcho errors: %v", be.Calculator.FailedErrors)
	}
}

func (be *BenchEcho) Stop() {

}

func (be *BenchEcho) OnWarmup(f func()) {
	be.onWarmup = f
}

func (be *BenchEcho) OnBenchmark(f func()) {
	be.onBenchmark = f
}

func (be *BenchEcho) SetPprofData(cpu, mem []byte) {
	be.pprofDataCPU = cpu
	be.pprofDataMEM = mem
}

func (be *BenchEcho) Report() *report.BenchEchoReport {
	r := &report.BenchEchoReport{
		BenchClient: "benchcli-go",
		Framework:   be.Framework,
		Lang:        config.FrameworkLang(be.Framework),
		Connections: len(be.Conns),
		Concurrency: be.Concurrency,
		Streams:     be.Streams,
		Payload:     be.Payload,
		Total:       be.Total,
		Success:     be.Calculator.Success,
		Failed:      be.Calculator.Failed,
		Used:        int64(be.Calculator.Used),

		TPS: be.Calculator.TPS(),
		Min: be.Calculator.Min,
		Avg: be.Calculator.Avg,
		Max: be.Calculator.Max,
	}
	if be.EnalbeTPN {
		r.TP50 = be.Calculator.TPN(50)
		r.TP75 = be.Calculator.TPN(75)
		r.TP90 = be.Calculator.TPN(90)
		r.TP95 = be.Calculator.TPN(95)
		r.TP99 = be.Calculator.TPN(99)
	}

	r.SetPprofData(be.pprofDataCPU, be.pprofDataMEM)

	var psErr error
	be.PsCounter, psErr = be.psInfo()
	if psErr != nil {
		logging.Printf("BenchEcho: resource statistics for %v incomplete, CPU EER and MEM EER will read 0: %v",
			be.Framework, psErr)
	}
	if be.PsCounter != nil {
		r.CPUMin = be.PsCounter.CPUMin()
		r.CPUAvg = be.PsCounter.CPUAvg()
		r.CPUMax = be.PsCounter.CPUMax()
		r.MEMRSSMin = be.PsCounter.MEMRSSMin()
		r.MEMRSSAvg = be.PsCounter.MEMRSSAvg()
		r.MEMRSSMax = be.PsCounter.MEMRSSMax()
		r.EER = report.EER(float64(r.TPS), r.CPUAvg)
		r.MEMEER = report.MEMEER(float64(r.TPS), r.MEMRSSAvg)
	}
	return r
}

// psInfo reads the server's resource samples from wherever this run takes
// them. A client that was given no source falls back to asking the server.
func (be *BenchEcho) psInfo() (*perf.PSCounter, error) {
	if be.PsSource != nil {
		return be.PsSource.PsInfo()
	}
	return config.GetFrameworkPsInfo(be.Framework, be.Ip)
}

func (be *BenchEcho) init() {
	if be.WarmupTimes <= 0 {
		be.WarmupTimes = len(be.Conns) * 5
		if be.WarmupTimes > 2000000 {
			be.WarmupTimes = 2000000
		}
	}
	if be.Concurrency <= 0 {
		be.Concurrency = runtime.NumCPU() * 1000
	}
	if be.Streams <= 0 {
		be.Streams = 1
	}
	if be.Concurrency > len(be.Conns)*be.Streams {
		be.Concurrency = len(be.Conns) * be.Streams
	}
	if be.Concurrency <= 0 {
		logging.Fatalf("BenchEcho: no connections to run on")
	}
	if be.Payload <= 0 {
		be.Payload = 1024
	}
	be.rbufferPool = &sync.Pool{
		New: func() any {
			buf := make([]byte, 0, be.Payload)
			return &buf
		},
	}
	if be.PsInterval <= 0 {
		be.PsInterval = time.Second
	}
	if be.EnalbeTPN {
		be.Percents = []int{50, 75, 90, 95, 99}
	}

	if be.Limit > 0 {
		limiter := rate.NewLimiter(rate.Every(1*time.Second), be.Limit)
		be.limitFn = func() {
			limiter.Wait(context.Background())
		}
	}

	host := connections.Host(be.Ip)
	be.pbuffers = make([][]byte, 1024)
	be.requests = make([]*protocol.Request, 1024)
	for i := 0; i < len(be.pbuffers); i++ {
		buffer := make([]byte, be.Payload)
		rand.Read(buffer)
		be.pbuffers[i] = buffer
		be.requests[i] = protocol.NewRequest("POST", host, config.EchoPath, buffer)
	}

	// Each connection is in the channel once for every stream it may carry,
	// so that a worker taking one from it takes one of that connection's
	// streams. They go in round by round, so that the streams in flight are
	// spread over every connection before any carries a second.
	be.chConns = make(chan *connections.Conn, len(be.Conns)*be.Streams)
	for i := 0; i < be.Streams; i++ {
		for _, c := range be.Conns {
			be.chConns <- c
		}
	}

	be.Calculator = perf.NewCalculator(fmt.Sprintf("%v-TPS", be.Framework))
}

func (be *BenchEcho) clean() {
	be.chConns = nil
	be.requests = nil
	be.limitFn = func() {}
}

func (be *BenchEcho) getBuffers() ([]byte, *protocol.Request) {
	idx := mrand.Intn(len(be.requests))
	return be.pbuffers[idx], be.requests[idx]
}

func (be *BenchEcho) doOnce() error {
	conn := <-be.chConns
	defer func() {
		be.chConns <- conn
	}()

	cc := conn.Client()
	if cc.Err() != nil {
		if err := conn.Redial(cc); err != nil {
			return fmt.Errorf("redial: %w", err)
		}
		cc = conn.Client()
	}

	be.limitFn()

	pbuffer, request := be.getBuffers()
	var rbuffer *[]byte
	var dst []byte
	if be.checkValid {
		rbuffer = be.rbufferPool.Get().(*[]byte)
		defer be.rbufferPool.Put(rbuffer)
		dst = *rbuffer
	}
	res := cc.Do(request, be.checkValid, dst)
	if rbuffer != nil {
		*rbuffer = res.Body[:0]
	}
	if res.Err != nil {
		return res.Err
	}
	if res.Status != 200 {
		return fmt.Errorf("status %d", res.Status)
	}

	if be.checkValid && !bytes.Equal(pbuffer, res.Body) {
		return errors.New("response body is not equal to the request's")
	}
	if res.N != len(pbuffer) {
		return fmt.Errorf("response body is %d bytes, want %d", res.N, len(pbuffer))
	}

	return nil
}
