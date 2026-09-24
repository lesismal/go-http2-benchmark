package main

import (
	"net/http"
	"strconv"

	"go-http2-benchmark/config"
	"go-http2-benchmark/frameworks"

	"github.com/gin-gonic/gin"
)

func main() {
	frameworks.Init(config.Gin)
	control := frameworks.StartControlServer()
	defer control.Close()

	// gin.New rather than gin.Default: no logger and no recovery middleware,
	// so the numbers are gin's router and context on top of net/http rather
	// than a line of log per request.
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Any(config.EchoPath, onEcho)

	// Served by net/http's own HTTP/2, as nethttp is, rather than by
	// gin's UseH2C, which wraps the handler in golang.org/x/net/http2/h2c:
	// the difference between the two rows is then gin and nothing else.
	frameworks.ServeHTTP2(router)
}

func onEcho(c *gin.Context) {
	body, bufp, err := frameworks.ReadBody(c.Request.Body, c.Request.ContentLength)
	defer frameworks.BodyPool.Put(bufp)
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}
	// gin writes through net/http, whose HTTP/2 server only declares the
	// length of a body it has buffered whole; see frameworks.OnEcho.
	c.Header("Content-Length", strconv.Itoa(len(body)))
	c.Data(http.StatusOK, "application/octet-stream", body)
}
