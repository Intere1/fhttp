package http_test

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	stdtest "net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// streamPeer is a real HTTP/2 wire peer that rejects the first complete
// request. Receiving the body before rejection makes replay measurable.
type streamPeer struct {
	listener net.Listener
	scenario streamScenario
	done     chan struct{}
	stopped  chan struct{}
	workers  sync.WaitGroup
	mu       sync.Mutex
	conns    []net.Conn
	headers  int
	bodies   []string
	failures []error
}

type streamScenario struct {
	signal string
	method string
	order  []string
}

func listenPeer(t *testing.T, scenario streamScenario) *streamPeer {
	t.Helper()
	cert := stdtest.NewTLSServer(stdhttp.HandlerFunc(func(stdhttp.ResponseWriter, *stdhttp.Request) {}))
	certificates := cert.TLS.Certificates
	cert.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: certificates, NextProtos: []string{"h2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &streamPeer{listener: listener, scenario: scenario, done: make(chan struct{}), stopped: make(chan struct{})}
	go p.accept()
	t.Cleanup(func() {
		close(p.done)
		if err := p.listener.Close(); err != nil {
			t.Error(err)
		}
		<-p.stopped
		p.mu.Lock()
		conns := append([]net.Conn(nil), p.conns...)
		p.mu.Unlock()
		for _, conn := range conns {
			p.disconnect(conn)
		}
		p.workers.Wait()
		for _, err := range p.failures {
			t.Errorf("HTTP/2 peer: %v", err)
		}
	})
	return p
}

func (p *streamPeer) accept() {
	defer close(p.stopped)
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			p.record(err)
			return
		}
		p.mu.Lock()
		p.conns = append(p.conns, conn)
		p.mu.Unlock()
		p.workers.Add(1)
		go func() {
			defer p.workers.Done()
			defer p.disconnect(conn)
			p.record(p.serve(conn))
		}()
	}
}

func (p *streamPeer) disconnect(conn net.Conn) {
	// Cleanup also visits connections already closed after GOAWAY.
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		p.mu.Lock()
		p.failures = append(p.failures, fmt.Errorf("close peer connection: %w", err))
		p.mu.Unlock()
	}
}

func (p *streamPeer) record(err error) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	p.mu.Lock()
	p.failures = append(p.failures, err)
	p.mu.Unlock()
}

func (p *streamPeer) serve(conn net.Conn) error {
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		return err
	}
	if string(preface) != http2.ClientPreface {
		return fmt.Errorf("unexpected client preface %q", preface)
	}
	framer := http2.NewFramer(conn, conn)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := framer.WriteSettings(); err != nil {
		return err
	}
	pending := make(map[uint32]*bytes.Buffer)
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return err
		}
		var completed uint32
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := framer.WriteSettingsAck(); err != nil {
					return err
				}
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				if err := framer.WritePing(true, f.Data); err != nil {
					return err
				}
			}
		case *http2.MetaHeadersFrame:
			fields := f.PseudoFields()
			if len(fields) != len(p.scenario.order) {
				return fmt.Errorf("received %d pseudo-headers; want %d", len(fields), len(p.scenario.order))
			}
			for i, field := range fields {
				if field.Name != p.scenario.order[i] {
					return fmt.Errorf("pseudo-header %d = %q; want %q", i, field.Name, p.scenario.order[i])
				}
			}
			for name, want := range map[string]string{
				"method": p.scenario.method, "scheme": "https",
				"authority": p.listener.Addr().String(), "path": "/probe",
			} {
				if got := f.PseudoValue(name); got != want {
					return fmt.Errorf("pseudo-header :%s = %q; want %q", name, got, want)
				}
			}
			p.mu.Lock()
			p.headers++
			p.mu.Unlock()
			pending[f.StreamID] = new(bytes.Buffer)
			if f.StreamEnded() {
				completed = f.StreamID
			}
		case *http2.DataFrame:
			body, ok := pending[f.StreamID]
			if !ok {
				return fmt.Errorf("DATA before HEADERS on stream %d", f.StreamID)
			}
			body.Write(f.Data())
			if f.StreamEnded() {
				completed = f.StreamID
			}
		}
		if completed == 0 {
			continue
		}
		p.mu.Lock()
		p.bodies = append(p.bodies, pending[completed].String())
		first := len(p.bodies) == 1
		p.mu.Unlock()
		delete(pending, completed)
		if first {
			if p.scenario.signal == "GOAWAY" {
				return framer.WriteGoAway(0, http2.ErrCodeNo, nil)
			}
			if err := framer.WriteRSTStream(completed, http2.ErrCodeRefusedStream); err != nil {
				return err
			}
			continue
		}
		var block bytes.Buffer
		if err := hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "200"}); err != nil {
			return err
		}
		if err := framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID: completed, BlockFragment: block.Bytes(), EndHeaders: true, EndStream: true,
		}); err != nil {
			return err
		}
	}
}

func (p *streamPeer) check(t *testing.T, requests, connections int, body string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.headers != requests || len(p.bodies) != requests || len(p.conns) != connections {
		t.Fatalf("wire: HEADERS=%d bodies=%d connections=%d; want %d requests on %d connections",
			p.headers, len(p.bodies), len(p.conns), requests, connections)
	}
	for i, got := range p.bodies {
		if got != body {
			t.Errorf("wire body %d = %q; want %q", i, got, body)
		}
	}
}
