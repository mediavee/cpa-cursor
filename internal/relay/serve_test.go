package relay

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// h2cClient speaks cleartext HTTP/2 with prior knowledge, the way a reverse proxy does.
func h2cClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: &p}}
}

// serve starts a relay on its own listener over an env's upstreams and returns its address.
func serve(t *testing.T, e *env, drain time.Duration) (addr string, stop context.CancelFunc, done <-chan error) {
	t.Helper()
	cfg := e.relay.cfg
	cfg.DrainTimeout = drain
	r, err := New(cfg, e.relay.log)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- r.Serve(ctx, ln) }()
	return ln.Addr().String(), cancel, errc
}

func openRun(t *testing.T, addr, token string) (*io.PipeWriter, *bufio.Reader, func()) {
	t.Helper()
	pr, pw := io.Pipe()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/agent.v1.AgentService/Run", pr)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/connect+proto")
	resp, err := h2cClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody := func() { _ = resp.Body.Close() }
	if resp.ProtoMajor != 2 {
		closeBody()
		t.Fatalf("served %s, want cleartext HTTP/2", resp.Proto)
	}
	return pw, bufio.NewReader(resp.Body), closeBody
}

func TestShutdownLetsOpenStreamsFinish(t *testing.T) {
	e := newEnv(t, envOpts{agent: echoStream})
	token := e.token()
	addr, stop, done := serve(t, e, 10*time.Second)

	pw, rd, closeBody := openRun(t, addr, token)
	defer closeBody()
	_, _ = io.WriteString(pw, "one\n")
	if line, _ := rd.ReadString('\n'); line != "echo:one\n" {
		t.Fatalf("read %q", line)
	}
	stop()
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) while a stream was open", err)
	case <-time.After(300 * time.Millisecond):
	}
	_, _ = io.WriteString(pw, "two\n")
	if line, _ := rd.ReadString('\n'); line != "echo:two\n" {
		t.Errorf("read %q: the stream should keep working while draining", line)
	}
	_ = pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return once the stream ended")
	}
}

func TestShutdownClosesStreamsThatOutlastTheDrainTimeout(t *testing.T) {
	e := newEnv(t, envOpts{agent: echoStream})
	token := e.token()
	addr, stop, done := serve(t, e, 200*time.Millisecond)

	pw, rd, closeBody := openRun(t, addr, token)
	defer closeBody()
	defer func() { _ = pw.Close() }()
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve waited for a stream that never ends")
	}
	if _, err := rd.ReadString('\n'); err == nil {
		t.Error("the stream is still open after the drain timeout")
	}
}
