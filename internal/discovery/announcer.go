package discovery

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/grandcat/zeroconf"
)

// AnnounceOptions define the metadata broadcast for this service.
type AnnounceOptions struct {
	Instance string
	Service  string
	Domain   string
	Port     int
	Text     map[string]string
}

// Announcer manages the lifetime of an mDNS advertisement.
type Announcer struct {
	server *zeroconf.Server
	once   sync.Once
}

// NewAnnouncer publishes an mDNS record for the agent and returns a controller.
func NewAnnouncer(opts AnnounceOptions) (*Announcer, error) {
	opts = opts.withDefaults()
	if opts.Port <= 0 {
		return nil, fmt.Errorf("invalid port %d", opts.Port)
	}

	text := make([]string, 0, len(opts.Text))
	for k, v := range opts.Text {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		value := strings.TrimSpace(v)
		text = append(text, fmt.Sprintf("%s=%s", key, value))
	}

	server, err := zeroconf.Register(opts.Instance, opts.Service, opts.Domain, opts.Port, text, nil)
	if err != nil {
		return nil, err
	}
	return &Announcer{
		server: server,
	}, nil
}

// Stop removes the advertisement.
func (a *Announcer) Stop() {
	a.once.Do(func() {
		if a.server != nil {
			a.server.Shutdown()
		}
	})
}

func (o AnnounceOptions) withDefaults() AnnounceOptions {
	if o.Service == "" {
		o.Service = defaultService
	}
	if o.Domain == "" {
		o.Domain = defaultDomain
	}
	if o.Instance == "" {
		if hostname, _ := os.Hostname(); hostname != "" {
			o.Instance = hostname
		} else {
			o.Instance = "mcp-agent"
		}
	}
	if o.Text == nil {
		o.Text = map[string]string{}
	}
	return o
}
