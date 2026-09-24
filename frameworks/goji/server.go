package main

import (
	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"

	"github.com/zenazn/goji/web"
)

func main() {
	frameworks.Init(config.Goji)
	control := frameworks.StartControlServer()
	defer control.Close()

	// goji's web.Mux on its own, not the goji package: that one adds a
	// logger and a recoverer to its default mux, defines a -bind flag the
	// servers are never given, and serves through a graceful server of its
	// own rather than net/http's HTTP/2.
	router := web.New()
	router.Handle(config.EchoPath, frameworks.OnEcho)

	frameworks.ServeHTTP2(router)
}
