// Package frameworks is what every benchmark server shares: the flags
// script/benchmark.sh forwards to them, the listeners on the framework's
// benchmark ports, and the control server that the clients read the server's
// pid, CPU and memory from.
//
// Every server speaks HTTP/2 in cleartext with prior knowledge (h2c, RFC 9113
// section 3.3) on its benchmark ports, and nothing else: no HTTP/1, no
// Upgrade, no TLS. That measures each framework's HTTP/2 stack rather than a
// TLS library they would all share, and a client that sends anything but the
// connection preface is refused.
package frameworks

import (
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"go-http2-benchmark/config"
	"go-http2-benchmark/logging"

	"github.com/libp2p/go-reuseport"
)

// The flags script/benchmark.sh forwards to every server. Each server defines
// all of them, whether or not it has a use for each, since one it did not
// define would stop it with "flag provided but not defined".
var (
	Nodelay  = flag.Bool("nodelay", true, `tcp nodelay`)
	reuse    = flag.Bool("reuseport", true, `reuse port`)
	Payload  = flag.Int("b", 1024, `expected request body size, which sizes the servers' read buffers`)
	memLimit = flag.Int64("m", 1024*1024*1024*2, `memory limit`)
	// MaxStreams is the SETTINGS_MAX_CONCURRENT_STREAMS every server
	// advertises, so that a client may keep as many requests open on a
	// connection to any of them. 250 is what net/http and fib both default
	// to.
	MaxStreams = flag.Int("maxstreams", 250, `HTTP/2 max concurrent streams a client may open on one connection`)
	framework  string
)

// Init parses the flags and applies the ones every server shares. framework
// is the server's name as config knows it.
func Init(name string) {
	flag.Parse()
	framework = name
	debug.SetMemoryLimit(*memLimit)
	if *MaxStreams <= 0 {
		logging.Fatalf("-maxstreams=%v: want at least 1", *MaxStreams)
	}
	logging.Printf("%v server: nodelay=%v, reuseport=%v, payload=%v, max streams=%v, memory limit=%v",
		name, *Nodelay, *reuse, *Payload, *MaxStreams, *memLimit)
}

// ServerAddrs is the addresses the framework's server listens on for the
// benchmark.
func ServerAddrs() []string {
	addrs, err := config.GetFrameworkServerAddrs(framework)
	if err != nil {
		logging.Fatalf("GetFrameworkServerAddrs(%v) failed: %v", framework, err)
	}
	return addrs
}

// Listen listens on addr, with SO_REUSEPORT unless -reuseport=false, and sets
// -nodelay on every connection it accepts. Go sets TCP_NODELAY on every TCP
// connection itself, so the wrapper only has anything to do for
// -nodelay=false; it is there for every framework that serves a net.Listener
// so that the flag means the same thing to all of them.
func Listen(network, addr string) (net.Listener, error) {
	ln, err := ListenSocket(network, addr)
	if err != nil {
		return nil, err
	}
	if *Nodelay {
		return ln, nil
	}
	return &noDelayListener{Listener: ln}, nil
}

// ListenSocket listens on addr, with SO_REUSEPORT unless -reuseport=false,
// and returns the net package's own listener, with no -nodelay wrapper: for a
// framework that takes the socket over from it (hertz's netpoll accepts on
// the descriptor itself, and only takes a *net.TCPListener), and so has to
// apply -nodelay to its connections itself.
func ListenSocket(network, addr string) (net.Listener, error) {
	if *reuse {
		return reuseport.Listen(network, addr)
	}
	return net.Listen(network, addr)
}

// ListenAll listens on every one of the framework's benchmark ports.
func ListenAll() []net.Listener {
	addrs := ServerAddrs()
	lns := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := Listen("tcp", addr)
		if err != nil {
			logging.Fatalf("listen %v failed: %v", addr, err)
		}
		lns = append(lns, ln)
	}
	logging.Printf("%v server: listening on %d ports, %v to %v",
		framework, len(addrs), addrs[0], addrs[len(addrs)-1])
	return lns
}

type noDelayListener struct {
	net.Listener
}

func (l *noDelayListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	SetNoDelay(c, false)
	return c, nil
}

func SetNoDelay(c net.Conn, nodelay bool) {
	cc, ok := c.(interface{ SetNoDelay(bool) error })
	if ok {
		cc.SetNoDelay(nodelay)
	}
}

// NewHTTP2Server is the http.Server every net/http based framework serves
// each benchmark port with: HTTP/2 in cleartext with prior knowledge and
// nothing else, since Protocols leaves HTTP/1 out, advertising -maxstreams.
func NewHTTP2Server(handler http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:   handler,
		Protocols: &protocols,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: *MaxStreams,
		},
	}
}

// ServeHTTP2 serves handler on every one of the framework's benchmark ports,
// one http.Server from NewHTTP2Server per port, all sharing handler: net/http
// serves one listener per Serve call, and every call can share the handler.
// It returns once the server is told to stop, with the servers closed.
//
// Every router that is an http.Handler is served this way, by net/http's own
// HTTP/2, rather than by an h2c wrapper of its own where it has one
// (golang.org/x/net/http2/h2c): the difference between its row and nethttp's
// is then the router and nothing else.
func ServeHTTP2(handler http.Handler) {
	var servers []*http.Server
	for _, ln := range ListenAll() {
		server := NewHTTP2Server(handler)
		servers = append(servers, server)
		go func() {
			if err := server.Serve(ln); err != http.ErrServerClosed {
				logging.Printf("server exit: %v", err)
			}
		}()
	}

	WaitSignal()
	for _, server := range servers {
		server.Close()
	}
}

// StartControlServer serves the control routes on the framework's control
// port, on a net/http server of its own; see
// config.GetFrameworkControlServerAddr.
func StartControlServer() *http.Server {
	addr, err := config.GetFrameworkControlServerAddr(framework)
	if err != nil {
		logging.Fatalf("GetFrameworkControlServerAddr(%v) failed: %v", framework, err)
	}
	mux := http.NewServeMux()
	HandleCommon(mux)
	server := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Fatalf("control server on %v exit: %v", addr, err)
		}
	}()
	return server
}

// WaitSignal blocks until the server is told to stop: SIGINT, which
// script/killone.sh sends, or SIGTERM.
func WaitSignal() {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	<-interrupt
	logging.Printf("%v server: exit", framework)
}
