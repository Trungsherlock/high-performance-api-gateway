package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestShutdownClosesActiveRequestAfterDeadline(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer s.Close()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := s.Client().Get(s.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	done := make(chan struct{})
	go func() { shutdown(s.Config, 20*time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown hung")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("active request not canceled")
	}
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client hung")
	}
}
