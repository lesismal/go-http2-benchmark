package main

import (
	"net/http"

	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"

	"github.com/julienschmidt/httprouter"
)

func main() {
	frameworks.Init(config.HTTPRouter)
	control := frameworks.StartControlServer()
	defer control.Close()

	// httprouter routes by method and has no route for any of them, so /echo
	// is registered for the two the clients send: the GET that Connections
	// opens every connection with, and the benchmarks' POST.
	router := httprouter.New()
	router.HandlerFunc(http.MethodGet, config.EchoPath, frameworks.OnEcho)
	router.HandlerFunc(http.MethodPost, config.EchoPath, frameworks.OnEcho)

	frameworks.ServeHTTP2(router)
}
