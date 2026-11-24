package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestShutdownReleasesSSEStreams is the regression for the journal's
// "sentinel shutdown error: context deadline exceeded": an open event stream
// never became idle, so http.Server.Shutdown sat out its whole grace period on
// every restart with a tab open. Closing the server's shutdown channel must
// release the handler promptly instead.
func TestShutdownReleasesSSEStreams(t *testing.T) {
	st := NewStore(t.TempDir())
	shutdown := make(chan struct{})
	srv := &Server{store: st, hub: NewHub(), shutdownCh: shutdown}

	ts := httptest.NewServer(http.HandlerFunc(srv.handleEvents))
	defer ts.CloseClientConnections() // kill the SSE conn first, so Close below returns
	defer ts.Close()
	ts.Config.RegisterOnShutdown(func() { close(shutdown) })

	resp, err := http.Get(ts.URL) // no timeout on purpose: the point is that Shutdown does not need one
	if err != nil {
		t.Fatalf("connect SSE: %v", err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("stream produced nothing: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- ts.Config.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown stalled on the open SSE stream (would log 'context deadline exceeded')")
	}
}
