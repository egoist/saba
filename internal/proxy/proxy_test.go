package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func upstream(t *testing.T, body string) (addr string, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s host=%s xff=%s proto=%s", body, r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), srv
}

func get(t *testing.T, front *httptest.Server, host string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", front.URL+"/", nil)
	req.Host = host
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRoutingAndForwardedHeaders(t *testing.T) {
	p := New(nil)
	front := httptest.NewServer(p)
	defer front.Close()
	a, _ := upstream(t, "A")
	b, _ := upstream(t, "B")
	p.SetApp("a", 1, []string{"a.example.com", "*.wild.example.com"}, []string{a})
	p.SetApp("b", 1, []string{"b.example.com"}, []string{b})

	code, body := get(t, front, "A.example.com:8080", "X-Forwarded-For", "6.6.6.6")
	if code != 200 || !strings.HasPrefix(body, "A host=A.example.com:8080") {
		t.Fatalf("got %d %q", code, body)
	}
	// Client-supplied forwarding headers must not be trusted.
	if strings.Contains(body, "6.6.6.6") || !strings.Contains(body, "xff=127.0.0.1") || !strings.Contains(body, "proto=http") {
		t.Fatalf("forwarded headers wrong: %q", body)
	}
	if _, body := get(t, front, "x.wild.example.com"); !strings.HasPrefix(body, "A ") {
		t.Fatalf("wildcard host not routed: %q", body)
	}
	if _, body := get(t, front, "b.example.com"); !strings.HasPrefix(body, "B ") {
		t.Fatalf("got %q", body)
	}
	if code, _ := get(t, front, "nope.example.com"); code != 404 {
		t.Fatalf("unknown host: %d", code)
	}
	// The TLS host policy: certificates are only requested for routed hosts.
	if !p.HasHost("a.example.com") || !p.HasHost("x.wild.example.com") || p.HasHost("evil.com") || p.HasHost("a.b.wild.example.com") {
		t.Fatal("HasHost does not match the routing table")
	}

	// Re-pointing an app drops hosts it no longer has.
	p.SetApp("a", 2, []string{"new.example.com"}, []string{a})
	if code, _ := get(t, front, "a.example.com"); code != 404 {
		t.Fatalf("stale host still routed: %d", code)
	}
	p.RemoveApp("b")
	if code, _ := get(t, front, "b.example.com"); code != 404 {
		t.Fatalf("removed app still routed: %d", code)
	}
}

func TestNoUpstreamsAndDeadUpstream(t *testing.T) {
	p := New(nil)
	front := httptest.NewServer(p)
	defer front.Close()
	p.SetApp("a", 1, []string{"a.test"}, nil)
	if code, _ := get(t, front, "a.test"); code != 503 {
		t.Fatalf("no upstreams: got %d, want 503", code)
	}

	// One dead and one live replica: every request must still succeed.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	live, _ := upstream(t, "LIVE")
	p.SetApp("a", 2, []string{"a.test"}, []string{dead, live})
	for i := 0; i < 20; i++ {
		if code, body := get(t, front, "a.test"); code != 200 || !strings.HasPrefix(body, "LIVE") {
			t.Fatalf("request %d: %d %q", i, code, body)
		}
	}
	p.SetApp("a", 3, []string{"a.test"}, []string{dead})
	if code, _ := get(t, front, "a.test"); code != 502 {
		t.Fatalf("all upstreams dead: got %d, want 502", code)
	}
	if s, _ := p.Stats("a"); s.Requests != 22 || s.Errors != 2 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestSwapDrainsOldPool(t *testing.T) {
	p := New(nil)
	front := httptest.NewServer(p)
	defer front.Close()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		fmt.Fprint(w, "old")
	}))
	defer slow.Close()
	fresh, _ := upstream(t, "new")
	p.SetApp("a", 1, []string{"a.test"}, []string{strings.TrimPrefix(slow.URL, "http://")})

	done := make(chan string, 1)
	go func() {
		_, body := get(t, front, "a.test")
		done <- body
	}()
	<-started
	old := p.SetApp("a", 2, []string{"a.test"}, []string{fresh})
	if old == nil || old.InFlight() != 1 {
		t.Fatalf("old pool should report 1 in-flight request, got %v", old)
	}
	// New requests already go to the new version.
	if _, body := get(t, front, "a.test"); !strings.HasPrefix(body, "new") {
		t.Fatalf("got %q", body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if left := old.Drain(ctx); left != 1 {
		t.Fatalf("drain should time out with 1 request left, got %d", left)
	}
	cancel()
	close(release)
	if body := <-done; body != "old" {
		t.Fatalf("in-flight request got %q, want it to finish on the old version", body)
	}
	if left := old.Drain(context.Background()); left != 0 {
		t.Fatalf("drain left %d", left)
	}
	old.Close()
}

func TestPauseHoldsRequests(t *testing.T) {
	p := New(nil)
	p.PauseTimeout = 150 * time.Millisecond
	front := httptest.NewServer(p)
	defer front.Close()
	v1, _ := upstream(t, "v1")
	v2, _ := upstream(t, "v2")
	p.SetApp("a", 1, []string{"a.test"}, []string{v1})

	p.Pause("a")
	var wg sync.WaitGroup
	bodies := make([]string, 5)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, bodies[i] = get(t, front, "a.test")
		}(i)
	}
	time.Sleep(30 * time.Millisecond)
	p.SetApp("a", 2, []string{"a.test"}, []string{v2})
	p.Resume("a")
	wg.Wait()
	for _, b := range bodies {
		if !strings.HasPrefix(b, "v2") {
			t.Fatalf("held request was served by %q, want v2", b)
		}
	}

	// A pause that outlives the timeout fails with 503 rather than hanging.
	p.Pause("a")
	if code, _ := get(t, front, "a.test"); code != 503 {
		t.Fatalf("got %d, want 503 after pause timeout", code)
	}
	p.Resume("a")
	if code, _ := get(t, front, "a.test"); code != 200 {
		t.Fatalf("got %d after resume", code)
	}
}

// Upgraded connections (WebSocket) must pass through and count as in flight,
// so that a deploy waits for them (up to the drain timeout).
func TestUpgradeAndStreaming(t *testing.T) {
	p := New(nil)
	front := httptest.NewServer(p)
	defer front.Close()
	next := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "echo" {
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
			rw.Flush()
			io.Copy(conn, rw)
			return
		}
		// Server-sent-events style: the first chunk must arrive before the second is written.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "first\n")
		w.(http.Flusher).Flush()
		<-next
		fmt.Fprint(w, "second\n")
	}))
	defer up.Close()
	p.SetApp("a", 1, []string{"a.test"}, []string{strings.TrimPrefix(up.URL, "http://")})

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: a.test\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 101") {
		t.Fatalf("upgrade response: %q %v", buf[:n], err)
	}
	fmt.Fprint(conn, "ping\n")
	n, err = conn.Read(buf)
	if err != nil || string(buf[:n]) != "ping\n" {
		t.Fatalf("echo over upgraded connection: %q %v", buf[:n], err)
	}
	if got := p.Pool("a").InFlight(); got != 1 {
		t.Fatalf("an open upgraded connection should count as in flight, got %d", got)
	}
	conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if left := p.Pool("a").Drain(ctx); left != 0 {
		t.Fatalf("closed connection still counted: %d", left)
	}

	req, _ := http.NewRequest("GET", front.URL+"/events", nil)
	req.Host = "a.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	chunk := make([]byte, 64)
	n, _ = resp.Body.Read(chunk)
	if string(chunk[:n]) != "first\n" {
		t.Fatalf("streamed response was buffered: got %q before the second chunk was sent", chunk[:n])
	}
	close(next)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "second\n" {
		t.Fatalf("rest = %q", rest)
	}
}
