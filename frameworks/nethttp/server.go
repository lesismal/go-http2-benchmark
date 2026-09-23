package main

import (
	"net/http"
	"strconv"

	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"
	"go-http2-benchmark/logging"
)

func main() {
	frameworks.Init(config.NetHTTP)
	control := frameworks.StartControlServer()
	defer control.Close()

	mux := http.NewServeMux()
	mux.HandleFunc(config.EchoPath, onEcho)

	// One http.Server per port, all on one mux: net/http serves one listener
	// per Serve call, and every call can share the handler.
	var servers []*http.Server
	for _, ln := range frameworks.ListenAll() {
		server := frameworks.NewHTTP2Server(mux)
		servers = append(servers, server)
		go func() {
			if err := server.Serve(ln); err != http.ErrServerClosed {
				logging.Printf("server exit: %v", err)
			}
		}()
	}

	frameworks.WaitSignal()
	for _, server := range servers {
		server.Close()
	}
}

func onEcho(w http.ResponseWriter, r *http.Request) {
	body, bufp, err := frameworks.ReadBody(r.Body, r.ContentLength)
	defer frameworks.BodyPool.Put(bufp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Set, not left to net/http: its HTTP/2 server only works a
	// content-length out for a body that fits in the one chunk it buffers
	// before the handler returns, and ends a longer one with END_STREAM alone.
	header := w.Header()
	header["Content-Type"] = contentType
	header["Content-Length"] = []string{strconv.Itoa(len(body))}
	w.Write(body)
}

var contentType = []string{"application/octet-stream"}
