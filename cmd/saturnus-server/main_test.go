package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestServeUntilShutdown(t *testing.T) {
	srv := newFakeHTTPServer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveUntilShutdown(ctx, srv, time.Second)
	}()

	<-srv.started
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveUntilShutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveUntilShutdown did not return")
	}

	select {
	case <-srv.shutdownCalled:
	default:
		t.Fatal("Shutdown was not called")
	}
	select {
	case <-srv.closeCalled:
		t.Fatal("Close was called after a successful graceful shutdown")
	default:
	}
}

func TestServeUntilShutdownForcesCloseAfterShutdownFailure(t *testing.T) {
	srv := newFakeHTTPServer()
	srv.shutdownErr = errors.New("shutdown failed")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveUntilShutdown(ctx, srv, time.Second)
	}()

	<-srv.started
	cancel()

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, srv.shutdownErr) {
			t.Fatalf("serveUntilShutdown() error = %v, want shutdown error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveUntilShutdown did not return")
	}

	select {
	case <-srv.closeCalled:
	default:
		t.Fatal("Close was not called after Shutdown failed")
	}
}

type fakeHTTPServer struct {
	started        chan struct{}
	stopped        chan struct{}
	shutdownCalled chan struct{}
	closeCalled    chan struct{}
	shutdownErr    error
	stopOnce       sync.Once
}

func newFakeHTTPServer() *fakeHTTPServer {
	return &fakeHTTPServer{
		started:        make(chan struct{}),
		stopped:        make(chan struct{}),
		shutdownCalled: make(chan struct{}),
		closeCalled:    make(chan struct{}),
	}
}

func (s *fakeHTTPServer) ListenAndServe() error {
	close(s.started)
	<-s.stopped
	return http.ErrServerClosed
}

func (s *fakeHTTPServer) Shutdown(context.Context) error {
	close(s.shutdownCalled)
	if s.shutdownErr != nil {
		return s.shutdownErr
	}
	s.stopOnce.Do(func() { close(s.stopped) })
	return nil
}

func (s *fakeHTTPServer) Close() error {
	close(s.closeCalled)
	s.stopOnce.Do(func() { close(s.stopped) })
	return nil
}
