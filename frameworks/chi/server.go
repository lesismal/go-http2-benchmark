package main

import (
	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"

	"github.com/go-chi/chi/v5"
)

func main() {
	frameworks.Init(config.Chi)
	control := frameworks.StartControlServer()
	defer control.Close()

	// chi.NewRouter with no middleware: the numbers are chi's router on top
	// of net/http.
	router := chi.NewRouter()
	router.HandleFunc(config.EchoPath, frameworks.OnEcho)

	frameworks.ServeHTTP2(router)
}
