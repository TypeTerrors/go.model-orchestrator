package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grandcat/zeroconf"
)

// ServerInfo captures the metadata required by the mediator to connect to an MCP server.
type ServerInfo struct {
	Instance string            `json:"instance"`
	Host     string            `json:"host"`
	Port     int               `json:"port"`
	Address  string            `json:"address"`
	Kind     string            `json:"kind"`
	LastSeen time.Time         `json:"last_seen"`
	Text     map[string]string `json:"text"`
}

// EventType captures the type of change for a discovered server.
type EventType string

// Event types emitted to discovery subscribers.
const (
	EventAdded   EventType = "added"
	EventUpdated EventType = "updated"
	EventRemoved EventType = "removed"
)

// Event represents a change in the discovery set.
type Event struct {
	Type   EventType
	Server *ServerInfo
}

// Options control how discovery behaves at runtime.
type Options struct {
	Service       string
	Domain        string
	EntryTTL      time.Duration
	PruneInterval time.Duration
}

// Discovery maintains a continually refreshed snapshot of visible MCP servers.
type Discovery struct {
	opts     Options
	snapshot atomic.Value

	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex

	subMu       sync.RWMutex
	subscribers map[chan Event]struct{}
}

// Default constants for the mDNS discovery loop.
const (
	defaultService       = "_mcp-http._tcp"
	defaultDomain        = "local."
	defaultEntryTTL      = 45 * time.Second
	defaultPruneInterval = 15 * time.Second
)

// Known server kinds advertised via TXT records.
const (
	ServerKindTool         = "tool"
	ServerKindAgentWrapper = "agent-wrapper"
	ServerKindOrchestrator = "orchestrator"
	ServerKindUnknown      = "unknown"
)

// New returns a discovery instance ready to be started.
func New(opts Options) *Discovery {
	opts = opts.withDefaults()
	d := &Discovery{
		opts:        opts,
		subscribers: make(map[chan Event]struct{}),
	}
	d.snapshot.Store(make(map[string]*ServerInfo))
	return d
}

// Start launches the browsing and pruning goroutines. It is safe to call once.
func (d *Discovery) Start(parent context.Context) error {
	if parent == nil {
		return errors.New("nil context")
	}
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	entries := make(chan *zeroconf.ServiceEntry)

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		cancel()
		return fmt.Errorf("create resolver: %w", err)
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.consumeEntries(ctx, entries)
	}()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.pruneLoop(ctx)
	}()

	// Launch browse in its own goroutine to avoid blocking Start.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		_ = resolver.Browse(ctx, d.opts.Service, d.opts.Domain, entries)
		close(entries)
	}()

	return nil
}

// Stop terminates discovery and waits for goroutines to finish.
func (d *Discovery) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
	d.wg.Wait()

	d.subMu.Lock()
	for ch := range d.subscribers {
		close(ch)
		delete(d.subscribers, ch)
	}
	d.subMu.Unlock()
}

// ServersSnapshot returns a copy of the known servers map for safe iteration.
func (d *Discovery) ServersSnapshot() map[string]*ServerInfo {
	raw := d.snapshot.Load().(map[string]*ServerInfo)
	return cloneServers(raw)
}

// Subscribe registers a listener channel that will receive discovery events.
// The returned channel should be read until closed; when the discovery is stopped,
// all subscriber channels are closed automatically.
func (d *Discovery) Subscribe(buffer int) chan Event {
	if buffer <= 0 {
		buffer = 1
	}
	ch := make(chan Event, buffer)
	d.subMu.Lock()
	d.subscribers[ch] = struct{}{}
	d.subMu.Unlock()
	return ch
}

// Unsubscribe removes the provided channel from the subscriber list and closes it.
func (d *Discovery) Unsubscribe(ch chan Event) {
	d.subMu.Lock()
	if _, ok := d.subscribers[ch]; ok {
		delete(d.subscribers, ch)
		close(ch)
	}
	d.subMu.Unlock()
}

func (d *Discovery) consumeEntries(ctx context.Context, entries <-chan *zeroconf.ServiceEntry) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-entries:
			if !ok {
				return
			}
			if entry == nil {
				continue
			}
			d.observe(entry)
		}
	}
}

func (d *Discovery) observe(entry *zeroconf.ServiceEntry) {
	now := time.Now()
	host := entry.HostName
	address := host
	if len(entry.AddrIPv4) > 0 {
		address = net.JoinHostPort(entry.AddrIPv4[0].String(), fmt.Sprint(entry.Port))
	} else if len(entry.AddrIPv6) > 0 {
		address = net.JoinHostPort(entry.AddrIPv6[0].String(), fmt.Sprint(entry.Port))
	} else {
		address = net.JoinHostPort(entry.HostName, fmt.Sprint(entry.Port))
	}

	textMap := make(map[string]string, len(entry.Text))
	for _, txt := range entry.Text {
		if kv := parseTxtRecord(txt); len(kv) == 2 {
			textMap[kv[0]] = kv[1]
		}
	}

	srv := &ServerInfo{
		Instance: entry.Instance,
		Host:     host,
		Port:     entry.Port,
		Address:  address,
		Kind:     classifyKind(textMap),
		LastSeen: now,
		Text:     textMap,
	}

	d.updateSnapshot(func(current map[string]*ServerInfo) map[string]*ServerInfo {
		_, exists := current[entry.Instance]
		clone := cloneServers(current)
		clone[entry.Instance] = srv
		if exists {
			d.broadcast(Event{Type: EventUpdated, Server: cloneServerInfo(srv)})
		} else {
			d.broadcast(Event{Type: EventAdded, Server: cloneServerInfo(srv)})
		}
		return clone
	})
}

func (d *Discovery) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(d.opts.PruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.pruneStale()
		}
	}
}

func (d *Discovery) pruneStale() {
	threshold := time.Now().Add(-d.opts.EntryTTL)
	d.updateSnapshot(func(current map[string]*ServerInfo) map[string]*ServerInfo {
		if len(current) == 0 {
			return current
		}
		clone := cloneServers(current)
		for key, info := range clone {
			if info.LastSeen.Before(threshold) {
				d.broadcast(Event{Type: EventRemoved, Server: cloneServerInfo(info)})
				delete(clone, key)
			}
		}
		return clone
	})
}

func (d *Discovery) updateSnapshot(modifier func(map[string]*ServerInfo) map[string]*ServerInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	current := d.snapshot.Load().(map[string]*ServerInfo)
	updated := modifier(current)
	d.snapshot.Store(updated)
}

func parseTxtRecord(txt string) []string {
	for i := 0; i < len(txt); i++ {
		if txt[i] == '=' {
			return []string{txt[:i], txt[i+1:]}
		}
	}
	return nil
}

func cloneServers(in map[string]*ServerInfo) map[string]*ServerInfo {
	clone := make(map[string]*ServerInfo, len(in))
	for k, v := range in {
		clone[k] = cloneServerInfo(v)
	}
	return clone
}

func (o Options) withDefaults() Options {
	if o.Service == "" {
		o.Service = defaultService
	}
	if o.Domain == "" {
		o.Domain = defaultDomain
	}
	if o.EntryTTL == 0 {
		o.EntryTTL = defaultEntryTTL
	}
	if o.PruneInterval == 0 {
		o.PruneInterval = defaultPruneInterval
	}
	return o
}

func classifyKind(text map[string]string) string {
	if text == nil {
		return ServerKindTool
	}
	role := strings.ToLower(strings.TrimSpace(text["role"]))
	switch role {
	case "":
		return ServerKindTool
	case ServerKindTool:
		return ServerKindTool
	case ServerKindAgentWrapper:
		return ServerKindAgentWrapper
	case ServerKindOrchestrator:
		return ServerKindOrchestrator
	default:
		return role
	}
}

func cloneServerInfo(in *ServerInfo) *ServerInfo {
	if in == nil {
		return nil
	}
	info := *in
	if in.Text != nil {
		textCopy := make(map[string]string, len(in.Text))
		for tk, tv := range in.Text {
			textCopy[tk] = tv
		}
		info.Text = textCopy
	}
	return &info
}

func (d *Discovery) broadcast(event Event) {
	if event.Server == nil {
		return
	}
	d.subMu.RLock()
	defer d.subMu.RUnlock()
	for ch := range d.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}
