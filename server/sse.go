package server

import (
	"context"
	"net/http"
	"time"

	"github.com/r3labs/sse/v2"

	"github.com/fatedier/frp/pkg/util/log"
)

var (
	// sseHeartbeatInterval keeps idle streams alive through proxies with a read
	// timeout (nginx defaults to 60s) and lets dead clients be detected.
	sseHeartbeatInterval = 15 * time.Second
	// sseMaxConnLifetime bounds a single stream connection. When it is reached
	// the response is finished cleanly and the client reconnects.
	sseMaxConnLifetime = 30 * time.Minute
	// sseWriteGrace is how long writes may still block after the lifetime is
	// reached, so the response can be terminated properly.
	sseWriteGrace = time.Minute
)

// newSSEServer creates the server for the proxy status stream.
//
// Replay is disabled: every event is a periodic snapshot of a proxy's status,
// so subscribers don't need history, and with AutoReplay enabled r3labs/sse
// keeps every published event in memory forever.
func newSSEServer() *sse.Server {
	s := sse.New()
	// AutoReplay is copied into the stream on creation, so set it first.
	s.AutoReplay = false
	// Tell nginx not to buffer the stream, otherwise events are held back.
	s.Headers["X-Accel-Buffering"] = "no"
	s.CreateStream(sseName)
	return s
}

// serveSSE serves a stream subscription on a connection of the web server.
//
// The web server's ReadTimeout and WriteTimeout are deadlines for the whole
// request. Left in place they cut the stream after 60s without terminating the
// chunked response, so they are lifted here and the stream instead ends by
// itself after sseMaxConnLifetime.
func serveSSE(ss *sse.Server, w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		log.Warnf("failed to clear sse read deadline, err=%v", err)
	}
	if err := rc.SetWriteDeadline(time.Now().Add(sseMaxConnLifetime + sseWriteGrace)); err != nil {
		log.Warnf("failed to extend sse write deadline, err=%v", err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), sseMaxConnLifetime)
	defer cancel()
	ss.ServeHTTP(w, r.WithContext(ctx))
}

// sseHeartbeat publishes a comment event periodically until ctx is done.
// Comments are ignored by SSE clients.
func sseHeartbeat(ctx context.Context, ss *sse.Server) {
	ticker := time.NewTicker(sseHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ss.Publish(sseName, &sse.Event{Comment: []byte("ping")})
		}
	}
}
