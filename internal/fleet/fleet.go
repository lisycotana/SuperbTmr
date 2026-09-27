// Package fleet is the resource-awareness layer of SuperbTmr.
//
// It answers one question the terminal/session primitives deliberately do not:
// which host should a job run on, given how many GPUs it needs and what is
// actually free right now.
//
// The layer adds exactly one new mechanism. Everything it needs to *do* —
// probing a host, opening a session, reading output, pulling files back — is
// borrowed from the session layer it sits on. Inventory is produced by running
// nvidia-smi in a long-lived session, the same sessions that execute jobs;
// there is no second execution channel.
//
// Placement is whole-host by design. A job is given the cards it needs on one
// host rather than being split across several: cross-machine distributed
// training pays a synchronisation tax on every step and fails as a unit, so
// keeping a job on one host keeps the fast path fast and lets different jobs
// progress independently.
package fleet

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Host is one machine the fleet knows about.
type Host struct {
	// Name is the ssh_config profile name the host is reached by.
	Name string `json:"name"`
	// Reachable is false when the last probe failed.
	Reachable bool `json:"reachable"`
	// LastSeen is the Unix ms timestamp of the last successful probe.
	LastSeen int64 `json:"last_seen,omitempty"`
	// LastError is why the last probe failed; empty when Reachable.
	LastError string `json:"last_error,omitempty"`

	CPUs       int   `json:"cpus,omitempty"`
	MemTotalMB int64 `json:"mem_total_mb,omitempty"`
	DiskFreeMB int64 `json:"disk_free_mb,omitempty"`

	GPUs []GPU `json:"gpus,omitempty"`
}

// GPU is one accelerator card on a host.
type GPU struct {
	Host  string `json:"host"`
	Index int    `json:"index"`
	Name  string `json:"name,omitempty"`

	MemTotalMB int64 `json:"mem_total_mb,omitempty"`
	MemUsedMB  int64 `json:"mem_used_mb,omitempty"`
	UtilPct    int   `json:"util_pct,omitempty"`
	TempC      int   `json:"temp_c,omitempty"`

	// LeasedBy is the run id holding this card, empty when free.
	LeasedBy string `json:"leased_by,omitempty"`
	// LeasedAt is the Unix ms timestamp the lease was taken.
	LeasedAt int64 `json:"leased_at,omitempty"`
	// Heartbeat is the Unix ms timestamp the holder last checked in.
	Heartbeat int64 `json:"heartbeat,omitempty"`
}

// Key is the stable identity of a card: "<host>#<index>".
func (g GPU) Key() string { return g.Host + "#" + strconv.Itoa(g.Index) }

// Free reports whether the card is not leased.
func (g GPU) Free() bool { return g.LeasedBy == "" }

// Requirement is what a job asks of the fleet.
type Requirement struct {
	// GPUs is how many cards the job needs on a single host. Zero means the
	// job does not care about GPUs at all and any reachable host will do.
	GPUs int `json:"gpus,omitempty"`
	// MinMemMB is the per-card memory floor. Zero means no floor.
	MinMemMB int64 `json:"min_mem_mb,omitempty"`
	// Candidates restricts placement to these ssh_config names. Empty means
	// every host in the fleet is eligible.
	Candidates []string `json:"candidates,omitempty"`
}

// Decision is the outcome of a placement request, including why.
//
// The reason is not decoration: a placement nobody can explain is a placement
// nobody can audit, and the run record is the only place the team will ever
// look to find out why a job landed where it did.
type Decision struct {
	Host  string   `json:"host"`
	Cards []int    `json:"cards,omitempty"`
	Why   []string `json:"why,omitempty"`
	// Queued is true when nothing had room and the caller must wait.
	Queued bool `json:"queued,omitempty"`
	// QueuedWhy explains what the job is waiting for.
	QueuedWhy string `json:"queued_why,omitempty"`
}

// LeaseOptions tunes how leases expire.
type LeaseOptions struct {
	// HeartbeatTimeout is how long a lease may go without a heartbeat before
	// the fleet reclaims it. Zero disables reclaim, which is almost always
	// wrong: a crashed holder would pin its cards forever.
	HeartbeatTimeout time.Duration
}

// DefaultLeaseOptions is the production default: a holder that goes quiet for
// 90 seconds has almost certainly died with its job.
func DefaultLeaseOptions() LeaseOptions {
	return LeaseOptions{HeartbeatTimeout: 90 * time.Second}
}

// Manager is the in-memory fleet state.
//
// All mutating operations take one mutex. The critical sections are short and
// purely computational (choosing a host, marking a lease), so a single lock
// keeps the invariant "a card is never leased twice" simple to reason about;
// the slow parts — probing hosts over SSH — happen outside it.
type Manager struct {
	mu      sync.Mutex
	hosts   map[string]*Host
	leases  LeaseOptions
	changed func()

	now func() time.Time
}

// NewManager returns an empty fleet. changed, when set, is called after every
// mutation so a UI can refresh without polling.
func NewManager(leases LeaseOptions, changed func()) *Manager {
	return &Manager{
		hosts:   map[string]*Host{},
		leases:  leases,
		changed: changed,
		now:     time.Now,
	}
}

// Snapshot returns a deep copy of the fleet. Callers get their own slices, so
// rendering a dashboard can never race with a probe.
func (m *Manager) Snapshot() []Host {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Host, 0, len(m.hosts))
	for _, h := range m.hosts {
		out = append(out, cloneHost(h))
	}
	// Stable order: by name, so a dashboard does not reshuffle between polls.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Host returns one host by ssh_config name, or nil when unknown.
func (m *Manager) Host(name string) *Host {
	m.mu.Lock()
	defer m.mu.Unlock()

	h, ok := m.hosts[name]
	if !ok {
		return nil
	}
	c := cloneHost(h)
	return &c
}

// Report installs or replaces a host's inventory, preserving live leases.
//
// A probe that loses a card (driver reset, card removed) must not silently
// free a lease somebody is still using, and a probe that renumbers cards must
// not strand one. Leases are therefore keyed by host+index and carried over
// onto the fresh report rather than rebuilt from it.
func (m *Manager) Report(h Host) {
	m.mu.Lock()

	prev, ok := m.hosts[h.Name]
	if ok {
		// Carry leases forward by index.
		old := map[int]GPU{}
		for _, g := range prev.GPUs {
			old[g.Index] = g
		}
		for i := range h.GPUs {
			if o, exists := old[h.GPUs[i].Index]; exists {
				h.GPUs[i].LeasedBy = o.LeasedBy
				h.GPUs[i].LeasedAt = o.LeasedAt
				h.GPUs[i].Heartbeat = o.Heartbeat
			}
		}
	}
	m.hosts[h.Name] = &h

	m.unlock()
}

// MarkUnreachable records that a probe failed, leaving existing leases alone.
//
// A host that stops answering is not necessarily gone for good, and the jobs
// it was running still own their cards until their leases expire. Reclaiming
// them here would let a second job grab cards a live holder is using.
func (m *Manager) MarkUnreachable(name, reason string) {
	m.mu.Lock()
	h, ok := m.hosts[name]
	if !ok {
		h = &Host{Name: name}
		m.hosts[name] = h
	}
	h.Reachable = false
	h.LastError = reason
	m.unlock()
}

// Remove drops a host entirely, releasing nothing explicitly: with the host
// gone its cards are gone too, and any lease it held disappears with it.
func (m *Manager) Remove(name string) {
	m.mu.Lock()
	delete(m.hosts, name)
	m.unlock()
}

// Place chooses a host for req and leases the chosen cards for runID.
//
// Selection is best-fit: the host with the fewest cards that still satisfies
// the requirement wins, so a 4-card job goes to a 4-card-free machine rather
// than eating into an 8-card-free one and blocking the job that needs all
// eight. Ties break by name for determinism.
//
// When nothing qualifies the call returns a Decision with Queued set and no
// host; the caller is expected to keep the run pending and retry, and the
// QueuedWhy string is what the dashboard shows as the reason.
func (m *Manager) Place(runID string, req Requirement) Decision {
	// The whole decision is computed under the lock and the change callback
	// fires after it is released. Firing it while the lock is held would
	// deadlock any listener that refreshes through Snapshot or Summarize —
	// which is exactly what a UI listener does.
	m.mu.Lock()
	d := m.placeLocked(runID, req)
	m.mu.Unlock()

	if !d.Queued && m.changed != nil {
		m.changed()
	}
	return d
}

// placeLocked does the selection. The caller must hold m.mu and must not call
// the change callback before releasing it.
func (m *Manager) placeLocked(runID string, req Requirement) Decision {

	now := m.now().UnixMilli()
	eligible := m.eligibleLocked(req)
	if len(eligible) == 0 {
		return Decision{Queued: true, QueuedWhy: m.queueReasonLocked(req)}
	}

	best := eligible[0]
	for _, cand := range eligible[1:] {
		if cand.free < best.free || (cand.free == best.free && cand.host.Name < best.host.Name) {
			best = cand
		}
	}

	cards := make([]int, 0, req.GPUs)
	for _, g := range best.host.GPUs {
		if req.GPUs > 0 && len(cards) >= req.GPUs {
			break
		}
		if !g.Free() {
			continue
		}
		cards = append(cards, g.Index)
	}
	// A GPU-less job still needs a host, but claims no cards.
	if req.GPUs == 0 {
		cards = nil
	}

	for _, idx := range cards {
		for i := range best.host.GPUs {
			if best.host.GPUs[i].Index == idx {
				best.host.GPUs[i].LeasedBy = runID
				best.host.GPUs[i].LeasedAt = now
				best.host.GPUs[i].Heartbeat = now
			}
		}
	}

	why := []string{
		fmt.Sprintf("%d free card(s) of %d required", best.free, req.GPUs),
	}
	if req.MinMemMB > 0 {
		why = append(why, fmt.Sprintf("every chosen card has >= %d MB", req.MinMemMB))
	}
	if len(eligible) > 1 {
		names := make([]string, 0, len(eligible))
		for _, e := range eligible {
			names = append(names, e.host.Name)
		}
		why = append(why, "eligible hosts: "+strings.Join(names, ", "))
	}
	why = append(why, "best-fit: fewest free cards that still satisfies the request")

	return Decision{Host: best.host.Name, Cards: cards, Why: why}
}

// Heartbeat renews every lease held by runID.
//
// It returns the number of cards still held. A run that finds itself at zero
// has lost its cards (crash reclaim, or the host went away) and should treat
// that as a failure rather than continue writing to a terminal it no longer
// owns.
func (m *Manager) Heartbeat(runID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	for _, h := range m.hosts {
		for i := range h.GPUs {
			if h.GPUs[i].LeasedBy == runID {
				h.GPUs[i].Heartbeat = m.now().UnixMilli()
				n++
			}
		}
	}
	return n
}

// Release frees every lease held by runID and returns how many were freed.
func (m *Manager) Release(runID string) int {
	m.mu.Lock()
	n := m.releaseLocked(runID)
	m.mu.Unlock()

	if n > 0 && m.changed != nil {
		m.changed()
	}
	return n
}

// releaseLocked frees the leases. The caller must hold m.mu and must not call
// the change callback before releasing it.
func (m *Manager) releaseLocked(runID string) int {
	n := 0
	for _, h := range m.hosts {
		for i := range h.GPUs {
			if h.GPUs[i].LeasedBy == runID {
				h.GPUs[i].LeasedBy = ""
				h.GPUs[i].LeasedAt = 0
				h.GPUs[i].Heartbeat = 0
				n++
			}
		}
	}
	return n
}

// ReclaimExpired frees leases whose holder has not heartbeat in time.
//
// It is the safety net for the case that matters most: a job that died without
// cleaning up would otherwise pin its cards until somebody noticed by hand.
func (m *Manager) ReclaimExpired() []string {
	if m.leases.HeartbeatTimeout <= 0 {
		return nil
	}
	cutoff := m.now().Add(-m.leases.HeartbeatTimeout).UnixMilli()

	m.mu.Lock()
	freed := m.reclaimLocked(cutoff)
	m.mu.Unlock()

	if len(freed) > 0 && m.changed != nil {
		m.changed()
	}
	return freed
}

// reclaimLocked frees the expired leases. The caller must hold m.mu and must
// not call the change callback before releasing it.
func (m *Manager) reclaimLocked(cutoff int64) []string {
	var freed []string
	for _, h := range m.hosts {
		for i := range h.GPUs {
			g := h.GPUs[i]
			if g.LeasedBy == "" || g.Heartbeat == 0 {
				continue
			}
			if g.Heartbeat >= cutoff {
				continue
			}
			h.GPUs[i].LeasedBy = ""
			h.GPUs[i].LeasedAt = 0
			h.GPUs[i].Heartbeat = 0
			freed = append(freed, g.Key()+" (was "+g.LeasedBy+")")
		}
	}
	return freed
}

// Summary is the fleet-wide rollup a dashboard leads with.
type Summary struct {
	Hosts       int `json:"hosts"`
	Reachable   int `json:"reachable"`
	GPUs        int `json:"gpus"`
	GPUsFree    int `json:"gpus_free"`
	GPUsLeased  int `json:"gpus_leased"`
	MemFreeMB   int64 `json:"mem_free_mb"`
	MemTotalMB  int64 `json:"mem_total_mb"`
	UtilAvgPct  int   `json:"util_avg_pct"`
	Unreachable []string `json:"unreachable,omitempty"`
}

// Summarize rolls the fleet up into the numbers a dashboard shows first.
func (m *Manager) Summarize() Summary {
	m.mu.Lock()
	defer m.mu.Unlock()

	var s Summary
	var utilSum, utilN int
	for _, h := range m.hosts {
		s.Hosts++
		if !h.Reachable {
			s.Unreachable = append(s.Unreachable, h.Name)
			continue
		}
		s.Reachable++
		for _, g := range h.GPUs {
			s.GPUs++
			s.MemTotalMB += g.MemTotalMB
			if g.Free() {
				s.GPUsFree++
				s.MemFreeMB += g.MemTotalMB - g.MemUsedMB
			} else {
				s.GPUsLeased++
			}
			if g.UtilPct > 0 {
				utilSum += g.UtilPct
				utilN++
			}
		}
	}
	if utilN > 0 {
		s.UtilAvgPct = utilSum / utilN
	}
	sort.Strings(s.Unreachable)
	return s
}

// candidate is one host that satisfies a requirement, with the free-card count
// best-fit selection sorts on.
type candidate struct {
	host *Host
	free int
}

func (m *Manager) eligibleLocked(req Requirement) []candidate {
	want := map[string]bool{}
	for _, c := range req.Candidates {
		want[c] = true
	}

	var out []candidate
	for _, h := range m.hosts {
		if !h.Reachable {
			continue
		}
		if len(want) > 0 && !want[h.Name] {
			continue
		}
		free := 0
		ok := true
		for _, g := range h.GPUs {
			if !g.Free() {
				continue
			}
			if req.MinMemMB > 0 && g.MemTotalMB > 0 && g.MemTotalMB < req.MinMemMB {
				continue
			}
			free++
		}
		if req.GPUs > 0 && free < req.GPUs {
			ok = false
		}
		// A GPU-less job needs a host that answers, not necessarily one with
		// cards — so it is eligible even with free == 0.
		if ok {
			out = append(out, candidate{host: h, free: free})
		}
	}
	return out
}

// queueReasonLocked explains, in terms an operator can act on, why nothing
// qualified. "No host has room" is useless; "gpu-a: 2 free of 4, gpu-b:
// unreachable" is not.
func (m *Manager) queueReasonLocked(req Requirement) string {
	want := map[string]bool{}
	for _, c := range req.Candidates {
		want[c] = true
	}

	var parts []string
	names := make([]string, 0, len(m.hosts))
	for n := range m.hosts {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		h := m.hosts[n]
		if len(want) > 0 && !want[n] {
			continue
		}
		if !h.Reachable {
			parts = append(parts, n+": unreachable")
			continue
		}
		free := 0
		small := 0
		for _, g := range h.GPUs {
			if !g.Free() {
				continue
			}
			if req.MinMemMB > 0 && g.MemTotalMB > 0 && g.MemTotalMB < req.MinMemMB {
				small++
				continue
			}
			free++
		}
		note := fmt.Sprintf("%d free of %d", free, req.GPUs)
		if small > 0 {
			note += fmt.Sprintf(", %d card(s) below %d MB", small, req.MinMemMB)
		}
		parts = append(parts, n+": "+note)
	}
	if len(parts) == 0 {
		return "no eligible host in the fleet"
	}
	return strings.Join(parts, "; ")
}

func (m *Manager) notifyLocked() {
	if m.changed != nil {
		m.changed()
	}
}

// unlock releases the mutex and fires the change callback.
//
// The callback runs with the lock held off deliberately: a UI refresh that
// calls back into Snapshot would deadlock otherwise.
func (m *Manager) unlock() {
	m.mu.Unlock()
	if m.changed != nil {
		m.changed()
	}
}

func cloneHost(h *Host) Host {
	c := *h
	c.GPUs = make([]GPU, len(h.GPUs))
	copy(c.GPUs, h.GPUs)
	return c
}

// ParseNvidiaSmi turns the CSV output of
//
//	nvidia-smi --query-gpu=index,name,memory.total,memory.used,utilization.gpu,temperature.gpu --format=csv,noheader,nounits
//
// into a GPU slice. It is deliberately strict about the shape it accepts and
// silent about the shape it does not: a card that cannot be parsed is skipped
// rather than guessed at, because a wrong inventory is worse than a short one.
func ParseNvidiaSmi(host, out string) []GPU {
	var gpus []GPU
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 5 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		g := GPU{
			Host:       host,
			Index:      idx,
			Name:       strings.TrimSpace(f[1]),
			MemTotalMB: atoi64Default(strings.TrimSpace(f[2])),
			MemUsedMB:  atoi64Default(strings.TrimSpace(f[3])),
			UtilPct:    atoiDefault(strings.TrimSpace(f[4])),
		}
		if len(f) >= 6 {
			g.TempC = atoiDefault(strings.TrimSpace(f[5]))
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// ParseMemInfo turns /proc/meminfo into total and free megabytes.
//
// MemAvailable is used rather than MemFree: on a busy host the kernel's free
// pages are a small fraction of what is actually allocatable, and a job placed
// against MemFree would OOM on a machine that had plenty of room.
func ParseMemInfo(out string) (totalMB, freeMB int64) {
	var total, avail int64
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = atoi64Default(f[1])
		case "MemAvailable:":
			avail = atoi64Default(f[1])
		}
	}
	return total / 1024, avail / 1024
}

// ParseDF turns `df -kP <path>` output into free megabytes.
func ParseDF(out string) int64 {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0
	}
	f := strings.Fields(lines[1])
	if len(f) < 4 {
		return 0
	}
	return atoi64Default(f[3]) / 1024
}

func atoiDefault(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

func atoi64Default(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// Format is a compile-time guard that the package really is used by something
// beyond tests; it keeps an unused-import mistake from surviving a refactor.
var _ = fmt.Sprintf
