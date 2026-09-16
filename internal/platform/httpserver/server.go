package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

type Settings struct {
	Name              string
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

type Server struct {
	settings Settings
	server   *http.Server
	logger   *slog.Logger

	mu       sync.Mutex
	listener net.Listener
	serveErr chan error
}

func New(s Settings, handler http.Handler, logger *slog.Logger) *Server {
	return &Server{
		settings: s,
		logger:   logger.With(slog.String("server", s.Name)),
		server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: s.ReadHeaderTimeout,
			ReadTimeout:       s.ReadTimeout,
			WriteTimeout:      s.WriteTimeout,
			IdleTimeout:       s.IdleTimeout,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
	}
}

func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", s.settings.Addr)
	if err != nil {
		return fmt.Errorf("%s server: listen on %s: %w", s.settings.Name, s.settings.Addr, err)
	}
	s.mu.Lock()
	s.listener = listener
	s.serveErr = make(chan error, 1)
	s.mu.Unlock()

	go func() {
		err := s.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		if err != nil {
			s.logger.Error("server stopped unexpectedly", slog.Any("error", err))
		}
		s.serveErr <- err
	}()
	s.logger.Info("server listening", slog.String("addr", listener.Addr().String()))
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	serveErr := s.serveErr
	s.mu.Unlock()
	if serveErr == nil {
		return nil
	}

	s.logger.Info("server shutting down")
	shutdownErr := s.server.Shutdown(ctx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(fmt.Errorf("%s server: graceful shutdown: %w", s.settings.Name, shutdownErr), s.server.Close())
	}
	select {
	case err := <-serveErr:
		return errors.Join(shutdownErr, err)
	case <-ctx.Done():
		return errors.Join(shutdownErr, fmt.Errorf("%s server: serve loop did not exit: %w", s.settings.Name, ctx.Err()))
	}
}

func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return s.settings.Addr
	}
	return s.listener.Addr().String()
}
