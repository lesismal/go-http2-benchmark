package main

import (
	"net/http"

	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"
)

func main() {
	frameworks.Init(config.NetHTTP)
	control := frameworks.StartControlServer()
	defer control.Close()

	mux := http.NewServeMux()
	mux.HandleFunc(config.EchoPath, frameworks.OnEcho)

	frameworks.ServeHTTP2(mux)
}
