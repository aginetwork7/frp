package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"
	"github.com/stretchr/testify/require"
)

// startSSETestServer serves ss with short server timeouts, like the web server
// does with its 60s ReadTimeout and WriteTimeout.
func startSSETestServer(t *testing.T, ss *sse.Server, serverTimeout time.Duration) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSSE(ss, w, r)
	}))
	ts.Config.ReadTimeout = serverTimeout
	ts.Config.WriteTimeout = serverTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL + "/api/sub?stream=" + sseName
}

func setSSETestTimings(t *testing.T, heartbeat, lifetime time.Duration) {
	t.Helper()
	oldHeartbeat, oldLifetime, oldGrace := sseHeartbeatInterval, sseMaxConnLifetime, sseWriteGrace
	sseHeartbeatInterval, sseMaxConnLifetime, sseWriteGrace = heartbeat, lifetime, time.Second
	t.Cleanup(func() {
		sseHeartbeatInterval, sseMaxConnLifetime, sseWriteGrace = oldHeartbeat, oldLifetime, oldGrace
	})
}

// readSSELines sends each line of the response body to the returned channel
// and the final read error (nil on a clean end) to the error channel.
func readSSELines(resp *http.Response) (<-chan string, <-chan error) {
	lines := make(chan string, 1024)
	errCh := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
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
	setSSETestTimings(t, 100*time.Millisecond, lifetime)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ss := newSSEServer()
	defer ss.Close()
	go sseHeartbeat(ctx, ss)
	url := startSSETestServer(t, ss, serverTimeout)

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
	setSSETestTimings(t, time.Hour, 5*time.Second)

	ss := newSSEServer()
	defer ss.Close()
	url := startSSETestServer(t, ss, time.Minute)

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
