//go:build e2e

package e2e_test

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

type faultProxy struct {
	target   string
	listener net.Listener
	down     atomic.Bool
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{target: target, listener: l, conns: map[net.Conn]struct{}{}}
	p.wg.Go(p.accept)
	t.Cleanup(func() {
		_ = l.Close()
		p.closeAll()
		p.wg.Wait()
	})
	return p
}

func (p *faultProxy) addr() string {
	return p.listener.Addr().String()
}

func (p *faultProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		if p.down.Load() {
			_ = client.Close()
			continue
		}
		p.wg.Go(func() { p.pipe(client) })
	}
}

func (p *faultProxy) pipe(client net.Conn) {
	server, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	if !p.track(client, server) {
		return
	}
	var wg sync.WaitGroup
	copyAndClose := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
	}
	wg.Go(func() { copyAndClose(server, client) })
	wg.Go(func() { copyAndClose(client, server) })
	wg.Wait()
	p.untrack(client, server)
}

func (p *faultProxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down.Load() {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	for _, c := range conns {
		p.conns[c] = struct{}{}
	}
	return true
}

func (p *faultProxy) untrack(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		delete(p.conns, c)
	}
}

func (p *faultProxy) cut() {
	p.mu.Lock()
	p.down.Store(true)
	p.mu.Unlock()
	p.closeAll()
}

func (p *faultProxy) restore() {
	p.down.Store(false)
}

func (p *faultProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
}
