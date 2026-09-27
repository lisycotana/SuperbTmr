package fleet

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func host(name string, cards ...GPU) Host {
	return Host{Name: name, Reachable: true, GPUs: cards}
}

func card(idx int, mem int64) GPU {
	return GPU{Host: "", Index: idx, MemTotalMB: mem, MemUsedMB: 0}
}

func TestPlace_BestFitPrefersFewestSufficientCards(t *testing.T) {
	var m *Manager
	m = NewManager(DefaultLeaseOptions(), func() { _ = m.Summarize() })

	// gpu-big has 8 free, gpu-small has exactly 4. A 4-card job must take the
	// small one and leave the 8-card host intact for the job that needs all 8.
	m.Report(host("gpu-big", card(0, 80000), card(1, 80000), card(2, 80000), card(3, 80000),
		card(4, 80000), card(5, 80000), card(6, 80000), card(7, 80000)))
	m.Report(host("gpu-small", card(0, 80000), card(1, 80000), card(2, 80000), card(3, 80000)))

	d := m.Place("run-1", Requirement{GPUs: 4})
	if d.Queued {
		t.Fatalf("unexpected queue: %s", d.QueuedWhy)
	}
	if d.Host != "gpu-small" {
		t.Fatalf("best-fit picked %q, want gpu-small (why=%v)", d.Host, d.Why)
	}
	if len(d.Cards) != 4 {
		t.Fatalf("leased %d cards, want 4", len(d.Cards))
	}
	if len(d.Why) == 0 {
		t.Fatal("decision carries no reason")
	}
}

func TestPlace_NeverLeasesTheSameCardTwice(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	m.Report(host("gpu-a", card(0, 80000), card(1, 80000), card(2, 80000), card(3, 80000)))

	if d := m.Place("run-1", Requirement{GPUs: 4}); d.Queued {
		t.Fatalf("first placement queued: %s", d.QueuedWhy)
	}

	// The fleet is exhausted, so a second job must queue rather than double-book.
	d := m.Place("run-2", Requirement{GPUs: 1})
	if !d.Queued {
		t.Fatal("second placement succeeded on an exhausted fleet")
	}
	if d.Host != "" {
		t.Fatalf("queued decision named a host: %q", d.Host)
	}
	if !strings.Contains(d.QueuedWhy, "gpu-a") || !strings.Contains(d.QueuedWhy, "0 free") {
		t.Fatalf("queue reason is not actionable: %q", d.QueuedWhy)
	}

	// And the first holder still owns exactly four cards.
	if n := m.Heartbeat("run-1"); n != 4 {
		t.Fatalf("run-1 holds %d cards, want 4", n)
	}
}

func TestPlace_RespectsMemoryFloorAndCandidates(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	// gpu-a's cards are too small; gpu-b qualifies.
	m.Report(host("gpu-a", card(0, 16000), card(1, 16000)))
	m.Report(host("gpu-b", card(0, 80000), card(1, 80000)))

	if d := m.Place("r", Requirement{GPUs: 1, MinMemMB: 40000}); d.Queued || d.Host != "gpu-b" {
		t.Fatalf("memory floor not honoured: queued=%v host=%q why=%v", d.Queued, d.Host, d.Why)
	}

	// Restricting candidates to gpu-a, while still demanding 40 GB, leaves
	// nothing eligible — gpu-a's cards are 16 GB.
	if d := m.Place("r2", Requirement{GPUs: 1, MinMemMB: 40000, Candidates: []string{"gpu-a"}}); !d.Queued {
		t.Fatal("candidate restriction ignored")
	}
}
func TestPlace_GPULessJobNeedsOnlyAReachableHost(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	m.Report(Host{Name: "cpu-only", Reachable: true}) // no cards at all

	d := m.Place("r", Requirement{})
	if d.Queued {
		t.Fatalf("GPU-less job queued: %s", d.QueuedWhy)
	}
	if d.Host != "cpu-only" || len(d.Cards) != 0 {
		t.Fatalf("host=%q cards=%v, want cpu-only with no cards", d.Host, d.Cards)
	}
}

func TestPlace_UnreachableHostIsNeverEligible(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	m.Report(host("gpu-a", card(0, 80000)))
	m.MarkUnreachable("gpu-a", "ssh: connection refused")

	d := m.Place("r", Requirement{GPUs: 1})
	if !d.Queued {
		t.Fatal("placed a job on an unreachable host")
	}
	if !strings.Contains(d.QueuedWhy, "unreachable") {
		t.Fatalf("reason does not mention reachability: %q", d.QueuedWhy)
	}
}

func TestReport_PreservesLiveLeasesAcrossProbes(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	m.Report(host("gpu-a", card(0, 80000), card(1, 80000)))
	if d := m.Place("run-1", Requirement{GPUs: 2}); d.Queued {
		t.Fatalf("placement queued: %s", d.QueuedWhy)
	}

	// A fresh probe reports new utilisation but the same cards. The lease must
	// survive, or a refresh would silently hand the cards to a second job.
	fresh := host("gpu-a", card(0, 80000), card(1, 80000))
	fresh.GPUs[0].UtilPct = 87
	fresh.GPUs[1].UtilPct = 91
	m.Report(fresh)

	h := m.Host("gpu-a")
	if h == nil {
		t.Fatal("host vanished after re-report")
	}
	for _, g := range h.GPUs {
		if g.LeasedBy != "run-1" {
			t.Fatalf("card %d lost its lease after a probe (leased_by=%q)", g.Index, g.LeasedBy)
		}
	}
	if h.GPUs[0].UtilPct != 87 {
		t.Fatal("fresh utilisation was not applied")
	}
}

func TestRelease_FreesCardsForTheNextJob(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	m.Report(host("gpu-a", card(0, 80000), card(1, 80000)))

	m.Place("run-1", Requirement{GPUs: 2})
	if n := m.Release("run-1"); n != 2 {
		t.Fatalf("released %d cards, want 2", n)
	}
	if s := m.Summarize(); s.GPUsFree != 2 || s.GPUsLeased != 0 {
		t.Fatalf("summary after release: free=%d leased=%d", s.GPUsFree, s.GPUsLeased)
	}
	if d := m.Place("run-2", Requirement{GPUs: 2}); d.Queued {
		t.Fatalf("next job still queued after release: %s", d.QueuedWhy)
	}
}

func TestReclaimExpired_FreesCardsOfDeadHolders(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	now := base
	m := NewManager(LeaseOptions{HeartbeatTimeout: 90 * time.Second}, nil)
	m.now = func() time.Time { return now }

	m.Report(host("gpu-a", card(0, 80000), card(1, 80000), card(2, 80000)))
	m.Place("alive", Requirement{GPUs: 1})
	m.Place("dead", Requirement{GPUs: 2})

	// "alive" keeps heartbeating; "dead" went quiet 5 minutes ago.
	now = base.Add(4 * time.Minute)
	m.Heartbeat("alive")
	now = base.Add(5 * time.Minute)

	freed := m.ReclaimExpired()
	if len(freed) != 2 {
		t.Fatalf("reclaimed %d cards (%v), want the 2 held by the dead run", len(freed), freed)
	}
	if !strings.Contains(strings.Join(freed, " "), "dead") {
		t.Fatalf("reclaimed the wrong holder: %v", freed)
	}
	if n := m.Heartbeat("alive"); n != 1 {
		t.Fatalf("live run lost its card: holds %d", n)
	}

	// The freed cards are immediately placeable again.
	if d := m.Place("next", Requirement{GPUs: 2}); d.Queued {
		t.Fatalf("reclaimed cards not reusable: %s", d.QueuedWhy)
	}
}

func TestReclaimExpired_DisabledWhenTimeoutZero(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	m := NewManager(LeaseOptions{HeartbeatTimeout: 0}, nil)
	m.now = func() time.Time { return base }
	m.Report(host("gpu-a", card(0, 80000)))
	m.Place("run-1", Requirement{GPUs: 1})

	if freed := m.ReclaimExpired(); freed != nil {
		t.Fatalf("reclaim ran with a zero timeout: %v", freed)
	}
}

func TestConcurrentPlace_IsExclusive(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	for i := 0; i < 8; i++ {
		m.Report(host("gpu-"+string(rune('a'+i)), card(0, 80000)))
	}

	var wg sync.WaitGroup
	seen := make([]string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := m.Place("run-"+string(rune('a'+i)), Requirement{GPUs: 1})
			seen[i] = d.Host
		}(i)
	}
	wg.Wait()

	counts := map[string]int{}
	for _, h := range seen {
		if h != "" {
			counts[h]++
		}
	}
	for h, n := range counts {
		if n > 1 {
			t.Fatalf("host %s was handed to %d runs concurrently", h, n)
		}
	}
	if len(counts) != 8 {
		t.Fatalf("%d distinct hosts used, want 8", len(counts))
	}
}

func TestSnapshot_IsADeepCopy(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	m.Report(host("gpu-a", card(0, 80000)))

	s := m.Snapshot()
	s[0].GPUs[0].LeasedBy = "mutated-by-caller"
	s[0].Reachable = false

	if got := m.Host("gpu-a"); got.GPUs[0].LeasedBy != "" || !got.Reachable {
		t.Fatal("mutating a snapshot leaked into the fleet")
	}
}

func TestSummarize_RollsUpTheFleet(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	m.Report(host("gpu-a", card(0, 80000), card(1, 80000)))
	m.Report(host("gpu-b", card(0, 80000)))
	m.MarkUnreachable("gpu-b", "timeout")

	m.Place("run-1", Requirement{GPUs: 1, Candidates: []string{"gpu-a"}})

	s := m.Summarize()
	// gpu-b is unreachable, so its single card is not counted at all: the
	// fleet cannot claim to know the state of a host it could not interrogate.
	if s.Hosts != 2 || s.Reachable != 1 {
		t.Fatalf("hosts=%d reachable=%d", s.Hosts, s.Reachable)
	}
	if s.GPUs != 2 || s.GPUsFree != 1 || s.GPUsLeased != 1 {
		t.Fatalf("gpus=%d free=%d leased=%d, want 2/1/1", s.GPUs, s.GPUsFree, s.GPUsLeased)
	}
	if len(s.Unreachable) != 1 || s.Unreachable[0] != "gpu-b" {
		t.Fatalf("unreachable=%v", s.Unreachable)
	}
}

func TestParseNvidiaSmi(t *testing.T) {
	out := "0, NVIDIA A100-SXM4-80GB, 81920, 1024, 87, 54\n" +
		"1, NVIDIA A100-SXM4-80GB, 81920, 40960, 12, 49\n" +
		"\n" +
		"garbage line\n"
	gpus := ParseNvidiaSmi("gpu-a", out)
	if len(gpus) != 2 {
		t.Fatalf("parsed %d cards, want 2 (garbage and blank lines must be skipped)", len(gpus))
	}
	if gpus[0].Index != 0 || gpus[0].MemTotalMB != 81920 || gpus[0].MemUsedMB != 1024 || gpus[0].UtilPct != 87 || gpus[0].TempC != 54 {
		t.Fatalf("card 0 parsed wrong: %+v", gpus[0])
	}
	if gpus[0].Host != "gpu-a" {
		t.Fatalf("host not stamped: %q", gpus[0].Host)
	}
	if gpus[1].MemUsedMB != 40960 || gpus[1].UtilPct != 12 {
		t.Fatalf("card 1 parsed wrong: %+v", gpus[1])
	}
}

func TestParseNvidiaSmi_EmptyOnNoCards(t *testing.T) {
	// A host with the tool but no visible cards yields nothing, not an error.
	if gpus := ParseNvidiaSmi("gpu-a", "No devices were found\n"); len(gpus) != 0 {
		t.Fatalf("got %d cards from an empty host", len(gpus))
	}
}

func TestParseMemInfo_UsesAvailableNotFree(t *testing.T) {
	out := "MemTotal:       262144 kB\nMemFree:          8192 kB\nMemAvailable:   196608 kB\n"
	total, free := ParseMemInfo(out)
	if total != 256 {
		t.Fatalf("total=%d MB, want 256", total)
	}
	if free != 192 {
		t.Fatalf("available=%d MB, want 192 (MemAvailable, not MemFree=8)", free)
	}
}

func TestParseDF(t *testing.T) {
	out := "Filesystem     1024-blocks      Used Available Capacity Mounted on\n" +
		"/dev/sda1        209715200 104857600  104857600      50% /\n"
	if got := ParseDF(out); got != 102400 {
		t.Fatalf("free=%d MB, want 102400", got)
	}
}

// fakeProber lets the refresher be tested without a session manager.
type fakeProber struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]bool
}

func (f *fakeProber) Probe(_ context.Context, name string) (Host, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	if f.fail[name] {
		return Host{}, context.DeadlineExceeded
	}
	return host(name, card(0, 80000)), nil
}

func TestRefresher_RecordsUnreachableInsteadOfFailing(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	p := &fakeProber{fail: map[string]bool{"gpu-b": true}}

	// A fresh fleet knows nothing yet, so the refresher must be seeded with the
	// configured host list — which is how it is wired in production.
	r := &Refresher{Fleet: m, Prober: p, Every: time.Hour, Hosts: []string{"gpu-a", "gpu-b"}}
	r.RefreshOnce(context.Background())

	if p.calls == nil {
		t.Fatal("refresher probed nothing")
	}
	s := m.Summarize()
	if s.Hosts != 2 || s.Reachable != 1 {
		t.Fatalf("hosts=%d reachable=%d, want 2 and 1", s.Hosts, s.Reachable)
	}
	if len(s.Unreachable) != 1 || s.Unreachable[0] != "gpu-b" {
		t.Fatalf("unreachable=%v", s.Unreachable)
	}
}

func TestRefresher_StartStopIsClean(t *testing.T) {
	m := NewManager(DefaultLeaseOptions(), nil)
	p := &fakeProber{}
	r := &Refresher{Fleet: m, Prober: p, Every: 5 * time.Millisecond, Hosts: []string{"gpu-a"}}
	r.Start()
	r.Start() // must be a no-op, not a second goroutine
	time.Sleep(40 * time.Millisecond)
	r.Stop()

	// Stop must be idempotent and must not hang.
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second Stop blocked")
	}

	if len(m.Snapshot()) == 0 {
		t.Fatal("refresher never populated the fleet")
	}
}

// TestChangeCallback_MayCallBackIntoTheManager guards the deadlock that a
// fleet listener would otherwise hit: the callback fires with the lock
// released, so a UI refresh that calls Snapshot/Summarize from inside it does
// not block on the very lock the mutator is holding.
func TestChangeCallback_MayCallBackIntoTheManager(t *testing.T) {
	var m *Manager
	m = NewManager(DefaultLeaseOptions(), func() {
		// Exactly what a UI listener does on a fleet change.
		_ = m.Snapshot()
		_ = m.Summarize()
	})

	m.Report(host("gpu-a", card(0, 80000), card(1, 80000), card(2, 80000)))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if d := m.Place("run-1", Requirement{GPUs: 2}); d.Queued {
			t.Errorf("placement queued: %s", d.QueuedWhy)
		}
		m.Heartbeat("run-1")
		m.Release("run-1")
		m.ReclaimExpired()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: a fleet change callback blocked on the manager lock")
	}
}
