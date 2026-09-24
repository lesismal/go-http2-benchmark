package main

import (
	"net/http"
	"strconv"

	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"

	"github.com/beego/beego/v2/server/web"
	"github.com/beego/beego/v2/server/web/context"
)

func main() {
	frameworks.Init(config.Beego)
	control := frameworks.StartControlServer()
	defer control.Close()

	// beego's router on its own, without web.Run's server. In prod mode, so
	// that it does not add a Server header to every response the way dev
	// mode does. Its defaults already leave out the access log, sessions and
	// copying the request body into memory before the handler reads it.
	web.BConfig.RunMode = web.PROD
	router := web.NewControllerRegister()
	router.Any(config.EchoPath, onEcho)
	router.Init()

	// Served by net/http's own HTTP/2, as nethttp is: beego's own server is
	// net/http's too, and only speaks HTTP/2 over TLS.
	frameworks.ServeHTTP2(router)
}

func onEcho(ctx *context.Context) {
	body, bufp, err := frameworks.ReadBody(ctx.Request.Body, ctx.Request.ContentLength)
	defer frameworks.BodyPool.Put(bufp)
	if err != nil {
		http.Error(ctx.ResponseWriter, err.Error(), http.StatusBadRequest)
		return
	}
	// beego writes through net/http, whose HTTP/2 server only declares the
	// length of a body it has buffered whole; see frameworks.OnEcho.
	header := ctx.ResponseWriter.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	ctx.ResponseWriter.WriteHeader(http.StatusOK)
	ctx.ResponseWriter.Write(body)
}
