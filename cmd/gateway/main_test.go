package main

import (
	"io"
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

func TestShutdownDrainsCompletedResponse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	draining := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			io.WriteString(w, "complete")
		case <-r.Context().Done():
			t.Error("shutdown canceled a drainable response")
		}
	}))
	defer s.Close()
	s.Config.RegisterOnShutdown(func() { close(draining) })
	response := make(chan string, 1)
	go func() {
		r, err := s.Client().Get(s.URL)
		if err != nil {
			response <- err.Error()
			return
		}
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			response <- err.Error()
			return
		}
		response <- string(body)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	done := make(chan struct{})
	go func() { defer close(done); shutdown(s.Config, time.Second) }()
	select {
	case <-draining:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not begin")
	}
	close(release)
	select {
	case body := <-response:
		if body != "complete" {
			t.Fatal(body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("response did not drain")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}
