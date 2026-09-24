package main

import (
	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"

	"github.com/gorilla/mux"
)

func main() {
	frameworks.Init(config.GorillaMux)
	control := frameworks.StartControlServer()
	defer control.Close()

	router := mux.NewRouter()
	router.HandleFunc(config.EchoPath, frameworks.OnEcho)

	frameworks.ServeHTTP2(router)
}
