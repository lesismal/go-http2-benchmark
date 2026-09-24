package main

import (
	"context"
	"net"
	"syscall"

	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"
	"go-http2-benchmark/logging"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/cloudwego/hertz/pkg/network"
	hertznetpoll "github.com/cloudwego/hertz/pkg/network/netpoll"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/protocol/suite"
	http2config "github.com/hertz-contrib/http2/config"
	http2factory "github.com/hertz-contrib/http2/factory"
)

func main() {
	frameworks.Init(config.Hertz)
	control := frameworks.StartControlServer()
	defer control.Close()

	// hertz logs every engine's start at Info; there are fifty of them.
	hlog.SetLevel(hlog.LevelWarn)

	// One hertz engine per port: an engine serves one listener. They all run
	// on netpoll, hertz's default transport, whose pollers are shared by
	// every engine in the process, so fifty engines are still one set of
	// event loops, the way fib's one engine is.
	addrs := frameworks.ServerAddrs()
	engines := make([]*server.Hertz, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := frameworks.ListenSocket("tcp", addr)
		if err != nil {
			logging.Fatalf("listen %v failed: %v", addr, err)
		}
		h := newEngine(ln)
		engines = append(engines, h)
		go func() {
			if err := h.Run(); err != nil {
				logging.Printf("server exit: %v", err)
			}
		}()
	}
	logging.Printf("%v server: listening on %d ports, %v to %v", config.Hertz, len(addrs), addrs[0], addrs[len(addrs)-1])

	frameworks.WaitSignal()
	for _, h := range engines {
		h.Close()
	}
}

func newEngine(ln net.Listener) *server.Hertz {
	// server.New, not server.Default, which adds the recovery middleware.
	h := server.New(
		server.WithListener(ln),
		// netpoll's transporter, which WithListener would otherwise leave to
		// hertz's per-platform default, named so that it is not the standard
		// library's by accident. netpoll accepts on ln's descriptor itself.
		server.WithTransport(hertznetpoll.NewTransporter),
		// Sniff each connection for the HTTP/2 preface and hand it to the h2
		// server added below.
		server.WithH2C(true),
		// No read timeout: netpoll applies it to every read on the
		// connection, not to a request, so it would close an HTTP/2
		// connection that sat idle for that long, which net/http's HTTP/2
		// does not do by default and nor does any other server here.
		server.WithReadTimeout(0),
		server.WithDisablePrintRoute(true),
		server.WithOnAccept(onAccept),
	)
	// hertz's HTTP/2 is hertz-contrib/http2, a port of golang.org/x/net/http2
	// onto hertz's own connection and request types, which is what makes it
	// run on netpoll's connections rather than net.Conns. Its receive windows
	// default to net/http's 1MB, as they are.
	h.AddProtocol(suite.HTTP2, http2factory.NewServerFactory(
		http2config.WithMaxConcurrentStreams(uint32(*frameworks.MaxStreams)),
		// It defaults to closing a connection with no open stream after
		// hertz's client idle time, 10 seconds; net/http's HTTP/2, with no
		// IdleTimeout set, never does.
		http2config.WithIdleTimeout(0),
	))
	// In the place of hertz's HTTP/1 server, which it would otherwise add,
	// and which is where a connection without the preface goes: HTTP/2 only,
	// as every server here serves it.
	h.AddProtocol(suite.HTTP1, refuseFactory{})
	h.Any(config.EchoPath, onEcho)
	return h
}

func onEcho(_ context.Context, c *app.RequestContext) {
	// Read off the stream into hertz's own pooled body buffer, and copied by
	// Data into the response's, so neither costs an allocation per request.
	body := c.Request.Body()
	// hertz-contrib/http2, like net/http, only declares the length of a body
	// that fits in the one chunk it buffers before the handler returns; see
	// frameworks.OnEcho.
	c.Response.Header.SetContentLength(len(body))
	c.Data(consts.StatusOK, "application/octet-stream", body)
}

// onAccept applies -nodelay=false. netpoll sets TCP_NODELAY on every
// connection it accepts, before it gets here, so there is only anything to do
// for false.
func onAccept(conn net.Conn) context.Context {
	if !*frameworks.Nodelay {
		if c, ok := conn.(*hertznetpoll.Conn); ok {
			if fd, ok := c.Conn.(interface{ Fd() int }); ok {
				syscall.SetsockoptInt(fd.Fd(), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 0)
			}
		}
	}
	return context.Background()
}

// refuseFactory is the HTTP/1 server that refuses everything: hertz closes a
// connection once its server returns.
type refuseFactory struct{}

func (refuseFactory) New(suite.Core) (protocol.Server, error) {
	return refuseServer{}, nil
}

type refuseServer struct{}

func (refuseServer) Serve(context.Context, network.Conn) error {
	return nil
}
