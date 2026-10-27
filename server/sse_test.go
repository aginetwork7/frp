package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"
	"github.com/stretchr/testify/require"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

// startSSETestServer serves ss with short server timeouts, like the web server
// does with its 60s ReadTimeout and WriteTimeout.
func startSSETestServer(t *testing.T, ss *sse.Server, cfg sseConfig, serverTimeout time.Duration) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSSE(ss, cfg, w, r)
	}))
	ts.Config.ReadTimeout = serverTimeout
	ts.Config.WriteTimeout = serverTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL + "/api/sub?stream=" + sseName
}

// readSSELines sends each line of the response body to the returned channel
// and the final read error (nil on a clean end) to the error channel.
func readSSELines(resp *http.Response) (<-chan string, <-chan error) {
	lines := make(chan string, 1024)
	errCh := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		errCh <- scanner.Err()
	}()
	return lines, errCh
}

func TestServeSSEOutlivesServerTimeoutsAndEndsCleanly(t *testing.T) {
	require := require.New(t)
	const serverTimeout = 300 * time.Millisecond
	const lifetime = 1500 * time.Millisecond
	cfg := sseConfig{heartbeatInterval: 100 * time.Millisecond, maxConnLifetime: lifetime, writeTimeout: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ss := newSSEServer()
	defer ss.Close()
	go sseHeartbeat(ctx, ss, cfg.heartbeatInterval)
	url := startSSETestServer(t, ss, cfg, serverTimeout)

	start := time.Now()
	resp, err := http.Get(url)
	require.NoError(err)
	defer resp.Body.Close()
	require.Equal(http.StatusOK, resp.StatusCode)
	require.Equal("no", resp.Header.Get("X-Accel-Buffering"))
	lines, errCh := readSSELines(resp)

	// Publish after the server's read and write timeouts have passed.
	time.Sleep(2 * serverTimeout)
	ss.Publish(sseName, &sse.Event{Data: []byte("after-timeout")})

	var got []string
	for line := range lines {
		got = append(got, line)
	}
	elapsed := time.Since(start)

	require.NoError(<-errCh, "stream must end with a terminated response")
	require.Contains(got, "data: after-timeout")
	require.Contains(got, ": ping")
	require.GreaterOrEqual(elapsed, lifetime-100*time.Millisecond)
	require.Less(elapsed, lifetime+time.Second)
}

func TestSSEServerDoesNotReplayHistory(t *testing.T) {
	require := require.New(t)
	cfg := sseConfig{heartbeatInterval: time.Hour, maxConnLifetime: 5 * time.Second, writeTimeout: 5 * time.Second}

	ss := newSSEServer()
	defer ss.Close()
	url := startSSETestServer(t, ss, cfg, time.Minute)

	for i := 0; i < 100; i++ {
		ss.Publish(sseName, &sse.Event{Data: []byte("old")})
	}

	// Without Last-Event-ID a replaying server would send every event from 0.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	defer resp.Body.Close()
	lines, _ := readSSELines(resp)

	// The subscriber is registered before the response header is sent, so this
	// event is the first one it can receive.
	ss.Publish(sseName, &sse.Event{Data: []byte("live")})

	timeout := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			require.True(ok, "stream closed before the live event")
			require.NotEqual("data: old", line)
			if line == "data: live" {
				return
			}
		case <-timeout:
			require.Fail("live event not received")
		}
	}
}

func TestSSEStalledClientDoesNotBlockOthers(t *testing.T) {
	require := require.New(t)
	// The lifetime is long so only the per-write timeout can unblock the stream.
	cfg := sseConfig{heartbeatInterval: time.Hour, maxConnLifetime: time.Minute, writeTimeout: 300 * time.Millisecond}
	const bound = 5 * time.Second

	ss := newSSEServer()
	defer ss.Close()
	url := startSSETestServer(t, ss, cfg, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	defer resp.Body.Close()
	lines, _ := readSSELines(resp)

	// The stalled client reads the response header and then stops reading.
	stalled, err := net.Dial("tcp", strings.TrimPrefix(strings.Split(url, "/api/")[0], "http://"))
	require.NoError(err)
	defer stalled.Close()
	require.NoError(stalled.(*net.TCPConn).SetReadBuffer(4096))
	_, err = fmt.Fprintf(stalled, "GET /api/sub?stream=%s HTTP/1.1\r\nHost: localhost\r\nAccept: text/event-stream\r\n\r\n", sseName)
	require.NoError(err)
	require.NoError(stalled.SetReadDeadline(time.Now().Add(bound)))
	var header []byte
	buf := make([]byte, 1)
	for !bytes.HasSuffix(header, []byte("\r\n\r\n")) {
		_, err := stalled.Read(buf)
		require.NoError(err)
		header = append(header, buf[0])
	}
	require.Contains(string(header), "200 OK")

	// Far more data than the subscriber channel and socket buffers can hold.
	data := bytes.Repeat([]byte("x"), 256*1024)
	published := make(chan struct{})
	go func() {
		defer close(published)
		for i := 0; i < 200; i++ {
			ss.Publish(sseName, &sse.Event{Data: data})
		}
		ss.Publish(sseName, &sse.Event{Data: []byte("marker")})
	}()

	timeout := time.After(bound)
	for received := false; !received; {
		select {
		case line, ok := <-lines:
			require.True(ok, "stream closed before the marker")
			received = line == "data: marker"
		case <-timeout:
			require.Fail("marker not received, the stalled client blocks the stream")
		}
	}
	select {
	case <-published:
	case <-timeout:
		require.Fail("publish blocked by the stalled client")
	}
}

func TestSSEUnknownStream(t *testing.T) {
	require := require.New(t)
	ss := newSSEServer()
	defer ss.Close()
	url := startSSETestServer(t, ss, defaultSSEConfig(), time.Minute)

	resp, err := http.Get(strings.Replace(url, "stream="+sseName, "stream=unknown", 1))
	require.NoError(err)
	resp.Body.Close()
	// r3labs/sse answers a missing stream with 500 when AutoStream is off.
	require.Equal(http.StatusInternalServerError, resp.StatusCode)
	require.False(ss.StreamExists("unknown"))
}

func TestSSEHeartbeatStopsOnContextCancel(t *testing.T) {
	ss := newSSEServer()
	defer ss.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sseHeartbeat(ctx, ss, 10*time.Millisecond)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "heartbeat did not stop after cancel")
	}
}

// raceEnabled reports whether the test binary is built with -race.
func raceEnabled() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, s := range info.Settings {
		if s.Key == "-race" {
			return s.Value == "true"
		}
	}
	return false
}

func TestServiceSSEHeartbeatStopsOnClose(t *testing.T) {
	if raceEnabled() {
		// checkProxyStatusTimer, started by NewService, reads svr.ctx while Run
		// assigns it, and Close writes svr.listener while Run reads it.
		t.Skip("NewService and Run race on svr.ctx and svr.listener")
	}
	require := require.New(t)
	cfg := &v1.ServerConfig{BindAddr: "127.0.0.1"}
	cfg.Complete()
	cfg.BindPort = 0 // listen on a random port
	svr, err := NewService(cfg)
	require.NoError(err)
	const interval = 20 * time.Millisecond
	svr.sseCfg.heartbeatInterval = interval

	go svr.Run(context.Background())

	url := startSSETestServer(t, svr.ss, defaultSSEConfig(), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	defer resp.Body.Close()
	lines, _ := readSSELines(resp)

	waitPing := func(d time.Duration) bool {
		timeout := time.After(d)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					return false
				}
				if line == ": ping" {
					return true
				}
			case <-timeout:
				return false
			}
		}
	}
	require.True(waitPing(3*time.Second), "no heartbeat while running")

	require.NoError(svr.Close())
	// Drain pings that were already in flight.
	time.Sleep(5 * interval)
	for drained := false; !drained; {
		select {
		case <-lines:
		default:
			drained = true
		}
	}
	require.False(waitPing(10*interval), "heartbeat still running after close")
}
