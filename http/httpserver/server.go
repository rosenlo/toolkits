package httpserver

import (
	"context"
	"errors"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/rosenlo/toolkits/log"
)

const (
	DefaultGracefulShutdownDuration = 5 * time.Second
)

type options struct {
	gracefulShutdownDuration time.Duration
}

type Option func(*options)

func SetGracefulShutdownDuration(d time.Duration) Option {
	return func(o *options) {
		o.gracefulShutdownDuration = d
	}
}

type Server struct {
	options    options
	httpServer *http.Server
}

func NewServer(addr string, handler http.Handler, opts ...Option) *Server {
	options := options{}
	for _, fn := range opts {
		fn(&options)
	}
	return &Server{
		options:    options,
		httpServer: &http.Server{Addr: addr, Handler: handler},
	}
}

func (s *Server) Start() {
	err := s.httpServer.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		log.Error("Http Server stopped unexpected")
		s.Shutdown()
	} else {
		log.Error("Http Server stopped")
	}
}

func (s *Server) Shutdown() {
	if s.httpServer == nil {
		return
	}

	var gracefulShutdownDuration time.Duration
	if s.options.gracefulShutdownDuration > 0 {
		gracefulShutdownDuration = s.options.gracefulShutdownDuration
	} else {
		gracefulShutdownDuration = DefaultGracefulShutdownDuration
	}

	ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownDuration)
	defer cancel()

	err := s.httpServer.Shutdown(ctx)
	if err != nil {
		log.Error("Failed to shutdown http server gracefully")
	} else {
		s.httpServer = nil
	}
}
