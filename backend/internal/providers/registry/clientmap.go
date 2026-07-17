package registry

import (
	"sync"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// ClientMap is a lazily-built, read-mostly map of registry clients
// keyed by registry id. It is constructed once at boot from the
// resolved registries config and shared by the HTTP layer (remote
// handler) and the task workers (retrieve_source, contribute,
// pull_all). Each entry is a *registry.Client built from the
// matching config.RegistryConfig.
//
// A nil ClientMap is safe to call — Client() returns (nil, false)
// and IsConfigured() returns false — so a deployment with no
// registries configured doesn't need nil guards at every call site.
type ClientMap struct {
	mu       sync.RWMutex
	clients  map[string]*Client
	configs  map[string]config.RegistryConfig
	ordering []string // stable list of ids for UI dropdowns
}

// NewClientMap builds a ClientMap from the resolved registries list.
// Entries with an empty URL are dropped (New returns a disabled
// client with an empty baseURL for them, which is why we filter
// here). The map is keyed by the registry id; the ordering slice
// preserves the configured order for the settings UI dropdown.
func NewClientMap(cfg config.ProvidersConfig) *ClientMap {
	regs := cfg.ResolveRegistries()
	m := &ClientMap{
		clients:  make(map[string]*Client, len(regs)),
		configs:  make(map[string]config.RegistryConfig, len(regs)),
		ordering: make([]string, 0, len(regs)),
	}
	for _, r := range regs {
		if r.URL == "" {
			continue
		}
		m.clients[r.ID] = New(r)
		m.configs[r.ID] = r
		m.ordering = append(m.ordering, r.ID)
	}
	return m
}

// Client returns the *Client for the given registry id and ok=false
// when no such registry is configured. A nil receiver returns
// (nil, false).
func (m *ClientMap) Client(id string) (*Client, config.RegistryConfig, bool) {
	if m == nil {
		return nil, config.RegistryConfig{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[id]
	if !ok {
		return nil, config.RegistryConfig{}, false
	}
	return c, m.configs[id], true
}

// Default returns the "default" registry client (the legacy single
// registry), or (nil, _, false) when no default is configured.
func (m *ClientMap) Default() (*Client, config.RegistryConfig, bool) {
	return m.Client("default")
}

// IDs returns the configured registry ids in stable order. Empty
// when no registries are configured.
func (m *ClientMap) IDs() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.ordering))
	copy(out, m.ordering)
	return out
}

// IsConfigured reports whether any registry is configured. The nil
// receiver returns false.
func (m *ClientMap) IsConfigured() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.clients) > 0
}

// IsConfigured reports whether this client points at a live registry
// (non-empty baseURL). The constructor returns a disabled client
// (empty baseURL) when the config URL is empty; this method lets the
// handlers/workers gate on "is this client actually wired" without
// special-casing the nil pointer.
func (c *Client) IsConfigured() bool {
	if c == nil {
		return false
	}
	return c.baseURL != ""
}