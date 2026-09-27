package fleet

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lisycotana/SuperbTmr/internal/session"
)

// NvidiaSmiQuery is the probe command. It is a plain argv run inside a
// long-lived session on the target host — the same kind of session that
// executes jobs. There is deliberately no second execution path.
var NvidiaSmiQuery = []string{
	"nvidia-smi",
	"--query-gpu=index,name,memory.total,memory.used,utilization.gpu,temperature.gpu",
	"--format=csv,noheader,nounits",
}

// Prober refreshes one host's inventory.
//
// It is an interface rather than a concrete type so the fleet can be tested
// without a GPU, and so a host without nvidia-smi can still be probed for CPU,
// memory and disk — an inventory that is short but honest is more useful than
// one that claims cards that do not exist.
type Prober interface {
	// Probe returns the host's current inventory. An error means the host
	// could not be reached or interrogated at all.
	Probe(ctx context.Context, name string) (Host, error)
}

// SessionProber probes hosts through the session layer.
//
// Each probe opens a session to the profile, runs the inventory commands in a
// pipe shell, reads the output, and terminates the session. Sessions are
// cheap and short-lived here; the long-lived variant used for job execution is
// a different concern and would keep idle connections open for no benefit.
type SessionProber struct {
	Sessions *session.Manager
	SSH      interface {
		Load(name string) (interface{}, error)
	}
	// Timeout bounds a single probe. A host that does not answer within it is
	// marked unreachable rather than blocking the fleet's refresh loop.
	Timeout time.Duration
}

// Probe runs the inventory commands on one host.
func (p *SessionProber) Probe(ctx context.Context, name string) (Host, error) {
	h := Host{Name: name, Reachable: true}

	gpuOut, err := p.run(ctx, name, NvidiaSmiQuery)
	if err != nil {
		// A missing nvidia-smi is not a dead host. Distinguish "cannot reach"
		// from "no GPU tooling" so a CPU-only machine stays eligible for
		// GPU-less jobs instead of disappearing from the fleet.
		if !isMissingTool(err) {
			return h, err
		}
	} else {
		h.GPUs = ParseNvidiaSmi(name, gpuOut)
	}

	memOut, err := p.run(ctx, name, []string{"cat", "/proc/meminfo"})
	if err == nil {
		h.MemTotalMB, _ = ParseMemInfo(memOut)
	}

	diskOut, err := p.run(ctx, name, []string{"df", "-kP", "."})
	if err == nil {
		h.DiskFreeMB = ParseDF(diskOut)
	}

	nprocOut, err := p.run(ctx, name, []string{"nproc"})
	if err == nil {
		h.CPUs = atoiDefault(strings.TrimSpace(nprocOut))
	}

	return h, nil
}

func (p *SessionProber) run(ctx context.Context, name string, argv []string) (string, error) {
	if p.Sessions == nil {
		return "", fmt.Errorf("fleet: no session manager configured")
	}
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	sess, err := p.Sessions.Create(session.Config{
		Name:   "fleet-probe:" + name,
		Mode:   "pipe",
		Remote: nil,
	})
	if err != nil {
		return "", err
	}
	defer sess.Terminate(false, 0)

	// The shell handle is not needed afterwards: ReadOutput reads through the
	// session's default reader, which sees everything this shell prints.
	if _, err := sess.CreateChildShell(argv[0], argv[1:], false, 24, 80, "probe"); err != nil {
		return "", err
	}

	// Read until the shell exits or the context expires. A pipe shell ends on
	// its own, so this is bounded by the command, not by a timeout we invent.
	out, err := sess.ReadOutput(ctx, p.Timeout, true, 0, 1<<20)
	if err != nil {
		return "", err
	}
	return out, nil
}

// isMissingTool reports whether an error is the target host saying "I do not
// have that program" rather than "I could not be reached".
func isMissingTool(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "no such file") ||
		strings.Contains(s, "command not found") ||
		strings.Contains(s, "executable file not found")
}

// Refresher keeps the fleet current on a schedule.
type Refresher struct {
	Fleet  *Manager
	Prober Prober
	// Every is the probe interval.
	Every time.Duration
	// Hosts is the list of ssh_config names to probe. Empty means "probe
	// whatever the fleet already knows about".
	Hosts []string

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	done    chan struct{}
}

// Start begins refreshing on the interval. Calling Start twice is a no-op.
func (r *Refresher) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running || r.Every <= 0 {
		return
	}
	r.running = true
	r.stop = make(chan struct{})
	r.done = make(chan struct{})

	go func() {
		defer close(r.done)
		t := time.NewTicker(r.Every)
		defer t.Stop()
		for {
			r.RefreshOnce(context.Background())
			select {
			case <-t.C:
			case <-r.stop:
				return
			}
		}
	}()
}

// Stop halts refreshing and waits for the in-flight probe to finish.
func (r *Refresher) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	close(r.stop)
	done := r.done
	r.mu.Unlock()

	if done != nil {
		<-done
	}
}

// RefreshOnce probes every host once. It never returns an error: a host that
// fails is recorded as unreachable, which is exactly the information the fleet
// needs in order to stop placing jobs on it.
func (r *Refresher) RefreshOnce(ctx context.Context) {
	names := r.Hosts
	if len(names) == 0 {
		for _, h := range r.Fleet.Snapshot() {
			names = append(names, h.Name)
		}
	}

	for _, n := range names {
		h, err := r.Prober.Probe(ctx, n)
		if err != nil {
			r.Fleet.MarkUnreachable(n, err.Error())
			continue
		}
		h.LastSeen = time.Now().UnixMilli()
		r.Fleet.Report(h)
	}
}
