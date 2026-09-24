package main

import (
	"net/http"
	"strconv"

	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"

	"github.com/labstack/echo/v5"
)

func main() {
	frameworks.Init(config.Echo)
	control := frameworks.StartControlServer()
	defer control.Close()

	// echo.New adds no middleware of its own, so the numbers are echo's
	// router and context on top of net/http.
	router := echo.New()
	router.Any(config.EchoPath, onEcho)

	// Served by net/http's own HTTP/2 rather than by echo's StartH2CServer,
	// which wraps the handler in golang.org/x/net/http2/h2c.
	frameworks.ServeHTTP2(router)
}

func onEcho(c *echo.Context) error {
	req := c.Request()
	body, bufp, err := frameworks.ReadBody(req.Body, req.ContentLength)
	defer frameworks.BodyPool.Put(bufp)
	if err != nil {
		return c.String(http.StatusBadRequest, err.Error())
	}
	// echo writes through net/http, whose HTTP/2 server only declares the
	// length of a body it has buffered whole; see frameworks.OnEcho.
	c.Response().Header().Set("Content-Length", strconv.Itoa(len(body)))
	return c.Blob(http.StatusOK, "application/octet-stream", body)
}
