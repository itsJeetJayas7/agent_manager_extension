package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-conductor/internal/providers"
)

type countedProbe struct {
	*providers.Fake
	calls atomic.Int32
}

func (p *countedProbe) Capabilities(ctx context.Context) providers.Caps {
	p.calls.Add(1)
	time.Sleep(20 * time.Millisecond)
	return p.Fake.Capabilities(ctx)
}

func TestCapabilityBurstSpawnsOnlyOneProbe(t *testing.T) {
	p := &countedProbe{Fake: providers.NewFakeNamed("claude")}
	r := providers.NewRegistry()
	r.Register(p)
	s := &Server{registry: r}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.capabilities() }()
	}
	wg.Wait()
	if n := p.calls.Load(); n != 1 {
		t.Fatalf("cold burst spawned %d probes", n)
	}
	s.capsMu.Lock()
	s.capsAt = time.Now().Add(-time.Hour)
	s.capsMu.Unlock()
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.capabilities() }()
	}
	wg.Wait()
	s.capsMu.Lock()
	done := s.capsRefresh
	s.capsMu.Unlock()
	if done != nil {
		<-done
	}
	if n := p.calls.Load(); n != 2 {
		t.Fatalf("stale burst spawned %d probes instead of one refresh", n)
	}
}

func BenchmarkWarmCapabilities(b *testing.B) {
	r := providers.NewRegistry()
	r.Register(providers.NewFakeNamed("claude"))
	s := &Server{registry: r}
	s.capabilities()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.capabilities()
	}
}
