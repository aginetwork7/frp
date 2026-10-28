package server

import (
	"context"
	"net/http"
	"time"

	"github.com/r3labs/sse/v2"

	"github.com/fatedier/frp/pkg/util/log"
)

// sseConfig holds the timings of the proxy status stream.
type sseConfig struct {
	// heartbeatInterval keeps idle streams alive through proxies with a read
	// timeout (nginx defaults to 60s). Together with writeTimeout it also makes
	// a client that stops reading fail within writeTimeout.
	heartbeatInterval time.Duration
	// maxConnLifetime bounds a single stream connection. When it is reached
	// the response is finished cleanly and the client reconnects.
	maxConnLifetime time.Duration
	// writeTimeout bounds every single write to a stream connection. A stalled
	// client would otherwise block the stream's single fan-out goroutine, and
	// with it every subscriber and every Publish.
	writeTimeout time.Duration
}

func defaultSSEConfig() sseConfig {
	return sseConfig{
		heartbeatInterval: 15 * time.Second,
		maxConnLifetime:   30 * time.Minute,
		writeTimeout:      30 * time.Second,
	}
}

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

// sseResponseWriter extends the write deadline before every write and flush,
// so each write gets writeTimeout instead of sharing one deadline for the
// whole stream.
type sseResponseWriter struct {
	http.ResponseWriter
	rc           *http.ResponseController
	writeTimeout time.Duration
}

func (w *sseResponseWriter) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.writeTimeout))
	return w.ResponseWriter.Write(p)
}

func (w *sseResponseWriter) Flush() {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.writeTimeout))
	_ = w.rc.Flush()
}

func (w *sseResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// serveSSE serves a stream subscription on a connection of the web server.
//
// The web server's ReadTimeout and WriteTimeout are deadlines for the whole
// request. Left in place they cut the stream after 60s without terminating the
// chunked response, so the read deadline is lifted, every write gets its own
// cfg.writeTimeout, and the stream ends by itself after cfg.maxConnLifetime.
func serveSSE(ss *sse.Server, cfg sseConfig, w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		log.Warnf("failed to clear sse read deadline, err=%v", err)
	}
	if err := rc.SetWriteDeadline(time.Now().Add(cfg.writeTimeout)); err != nil {
		log.Warnf("failed to set sse write deadline, err=%v", err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), cfg.maxConnLifetime)
	defer cancel()
	sw := &sseResponseWriter{ResponseWriter: w, rc: rc, writeTimeout: cfg.writeTimeout}
	ss.ServeHTTP(sw, r.WithContext(ctx))
}

// sseHeartbeat publishes a comment event every interval until ctx is done.
// Comments are ignored by SSE clients.
func sseHeartbeat(ctx context.Context, ss *sse.Server, interval time.Duration) {
	ticker := time.NewTicker(interval)
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
