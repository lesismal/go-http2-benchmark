package main

import (
	stdhttp "net/http"
	"syscall"

	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"
	"go-http2-benchmark/logging"

	fib "github.com/lesismal/fib"
	fibhttp "github.com/lesismal/fib/http"
)

func main() {
	frameworks.Init(config.Fib)
	control := frameworks.StartControlServer()
	defer control.Close()

	addrs := frameworks.ServerAddrs()

	httpConfig := fibhttp.DefaultConfig()
	// HTTP/2 only, in cleartext with prior knowledge, as the others serve
	// it here: a connection that does not open with the preface is ended with
	// GOAWAY rather than answered in HTTP/1.
	httpConfig.HTTP2Only = true
	httpConfig.MaxConcurrentStreams = uint32(*frameworks.MaxStreams)
	// StreamPool is left at its default, which serves the streams open on one
	// connection concurrently, on a pool shared by every port, the way
	// net/http gives each stream a goroutine of its own. The Reuse options
	// are left off: they recycle what an HTTP/1 request is served with and do
	// nothing for HTTP/2.
	handler := &serverHandler{
		ServerHandler: fibhttp.NewHandlerWithConfig(httpConfig, fibhttp.HandlerFunc(onRequest)),
		nodelay:       *frameworks.Nodelay,
	}

	// One engine bound to every benchmark port. A server per port would give
	// each one its own event loop but also its own descriptor table, buffer
	// pools and outbound budget, none of which the ports have any reason not
	// to share.
	serverConfig := fib.DefaultConfig()
	serverConfig.Network = "tcp4"
	serverConfig.Addrs = addrs
	engine, err := fib.Bind(serverConfig, handler)
	if err != nil {
		logging.Fatalf("bind %d addresses failed: %v", len(addrs), err)
	}
	logging.Printf("%v server: listening on %d ports, %v to %v", config.Fib, len(addrs), addrs[0], addrs[len(addrs)-1])
	go func() {
		if err := engine.Run(); err != nil {
			logging.Printf("server exit: %v", err)
		}
	}()

	frameworks.WaitSignal()
	engine.Stop()
}

func onRequest(c *fibhttp.Context, r *stdhttp.Request) {
	if r.URL.Path != config.EchoPath {
		_ = c.Respond(stdhttp.StatusNotFound, "text/plain; charset=utf-8", []byte("404 page not found\n"))
		return
	}
	// The body has already been read off the connection whole, so this never
	// waits on the network; and Respond copies it into the frames it sends,
	// with a content-length, so the buffer can go back to the pool as soon as
	// it returns.
	body, bufp, err := frameworks.ReadBody(r.Body, r.ContentLength)
	defer frameworks.BodyPool.Put(bufp)
	if err != nil {
		_ = c.Respond(stdhttp.StatusBadRequest, "text/plain; charset=utf-8", []byte(err.Error()))
		return
	}
	_ = c.Respond(stdhttp.StatusOK, "application/octet-stream", body)
}

// serverHandler sets TCP_NODELAY to -nodelay on each connection before fib's
// HTTP handler sees it, whichever way -nodelay points: fib turns the option on
// for every TCP connection it accepts, and Go's net package, which applies it
// for the Go ones, is not involved.
type serverHandler struct {
	*fibhttp.ServerHandler
	nodelay bool
}

func (h *serverHandler) OnOpen(c *fib.Connection) {
	nodelay := 0
	if h.nodelay {
		nodelay = 1
	}
	if err := syscall.SetsockoptInt(c.FD(), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, nodelay); err != nil {
		c.Close()
		return
	}
	h.ServerHandler.OnOpen(c)
}
