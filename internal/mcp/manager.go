package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"frostgate/internal/config"
)

// client is one configured MCP server. A client is kept even when its
// connection fails so /mcp can report the server as disconnected; in that case
// tr is nil and connErr holds the failure.
type client struct {
	name    string
	spec    config.MCPServer // dial parameters, for reconnect
	tr      transport
	tools   []Tool // tools this server advertised on the last successful list
	connErr error  // last connect/handshake/list error, nil when connected
}

// initialize performs the MCP handshake.
func (c *client) initialize() error {
	_, err := c.tr.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "frostgate", "version": "1.0"},
	})
	return err
}

func (c *client) listTools() ([]Tool, error) {
	raw, err := c.tr.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res listToolsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

func (c *client) callTool(name string, args json.RawMessage) (string, error) {
	var argObj any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &argObj)
	}
	raw, err := c.tr.call("tools/call", map[string]any{"name": name, "arguments": argObj})
	if err != nil {
		return "", err
	}
	var res callToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range res.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	if res.IsError {
		return sb.String(), fmt.Errorf("tool %q reported error", name)
	}
	return sb.String(), nil
}

// openAITool is the function-tool shape models expect.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// Manager owns all MCP clients and the aggregated tool catalog.
type Manager struct {
	enabled  bool
	maxIter  int
	mu       sync.RWMutex
	order    []string // server names in config order, for stable listing
	clients  map[string]*client
	toolHome map[string]*client // tool name -> owning client
	catalog  []openAITool
}

// NewManager connects to every configured server, performs the handshake, and
// builds the tool catalog. Servers that fail to connect are skipped with a
// best-effort policy so one bad server doesn't sink the gateway.
func NewManager(cfg config.MCPConfig) (*Manager, []error) {
	m := &Manager{
		enabled:  cfg.Enabled,
		maxIter:  cfg.MaxToolIterations,
		clients:  map[string]*client{},
		toolHome: map[string]*client{},
	}
	var errs []error
	if !cfg.Enabled {
		return m, nil
	}
	for _, s := range cfg.Servers {
		c := &client{name: s.Name, spec: s}
		m.order = append(m.order, s.Name)
		m.clients[s.Name] = c
		if err := m.dial(c); err != nil {
			errs = append(errs, err)
		}
	}
	m.rebuildCatalog()
	return m, errs
}

// dial connects (or reconnects) a single client, performs the handshake, and
// records the tool names it advertises. On failure it sets c.connErr and clears
// the transport so the server shows as disconnected. m.mu must be held by the
// caller when invoked after construction (rebuildCatalog is the caller's job).
func (m *Manager) dial(c *client) error {
	c.tools = nil
	c.connErr = nil
	c.tr = nil
	conn, err := connect(c.spec)
	if err != nil {
		c.connErr = fmt.Errorf("mcp %q: %w", c.name, err)
		return c.connErr
	}
	c.tr = conn.tr
	if err := c.initialize(); err != nil {
		_ = c.tr.close()
		c.tr = nil
		c.connErr = fmt.Errorf("mcp %q initialize: %w", c.name, err)
		return c.connErr
	}
	tools, err := c.listTools()
	if err != nil {
		_ = c.tr.close()
		c.tr = nil
		c.connErr = fmt.Errorf("mcp %q tools/list: %w", c.name, err)
		return c.connErr
	}
	c.tools = tools
	return nil
}

// rebuildCatalog regenerates the aggregated tool catalog and toolHome map from
// the current set of connected clients, applying the first-server-wins rule for
// duplicate tool names. Callers must hold m.mu.
func (m *Manager) rebuildCatalog() {
	m.catalog = nil
	m.toolHome = map[string]*client{}
	for _, name := range m.order {
		c := m.clients[name]
		if c == nil || c.tr == nil {
			continue
		}
		for _, t := range c.tools {
			if _, dup := m.toolHome[t.Name]; dup {
				continue // first server to claim a tool name wins
			}
			m.toolHome[t.Name] = c
			m.catalog = append(m.catalog, toOpenAI(t))
		}
	}
	sort.Slice(m.catalog, func(i, j int) bool {
		return m.catalog[i].Function.Name < m.catalog[j].Function.Name
	})
}

func connect(s config.MCPServer) (*client, error) {
	switch s.Transport {
	case "http":
		return &client{name: s.Name, tr: newHTTPTransport(s.URL)}, nil
	case "stdio":
		tr, err := newStdioTransport(s.Command, s.Args)
		if err != nil {
			return nil, err
		}
		return &client{name: s.Name, tr: tr}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q", s.Transport)
	}
}

func toOpenAI(t Tool) openAITool {
	var o openAITool
	o.Type = "function"
	o.Function.Name = t.Name
	o.Function.Description = t.Description
	o.Function.Parameters = t.InputSchema
	return o
}

// Enabled reports whether MCP tool injection is active and any tools exist.
func (m *Manager) Enabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled && len(m.catalog) > 0
}

// MaxIterations is the agentic loop guard.
func (m *Manager) MaxIterations() int {
	if m.maxIter <= 0 {
		return 4
	}
	return m.maxIter
}

// ToolsJSON returns the catalog as an OpenAI "tools" array, or nil if empty.
func (m *Manager) ToolsJSON() json.RawMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.catalog) == 0 {
		return nil
	}
	b, _ := json.Marshal(m.catalog)
	return b
}

// ToolNames lists advertised tool names (for /status and tests).
func (m *Manager) ToolNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.catalog))
	for _, t := range m.catalog {
		out = append(out, t.Function.Name)
	}
	return out
}

// ServerInfo is a snapshot of one configured MCP server for /mcp.
type ServerInfo struct {
	Name      string
	Transport string
	Connected bool
	Err       string   // connection error message, empty when connected
	Tools     []string // advertised tool names (sorted)
}

// Servers returns a snapshot of every configured server in config order,
// including ones that failed to connect.
func (m *Manager) Servers() []ServerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServerInfo, 0, len(m.order))
	for _, name := range m.order {
		c := m.clients[name]
		if c == nil {
			continue
		}
		info := ServerInfo{
			Name:      c.name,
			Transport: c.spec.Transport,
			Connected: c.tr != nil,
		}
		if c.connErr != nil {
			info.Err = c.connErr.Error()
		}
		for _, t := range c.tools {
			info.Tools = append(info.Tools, t.Name)
		}
		sort.Strings(info.Tools)
		out = append(out, info)
	}
	return out
}

// Reconnect re-dials a single server by name and rebuilds the tool catalog. It
// returns an error if the server is unknown or fails to reconnect.
func (m *Manager) Reconnect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.clients[name]
	if c == nil {
		return fmt.Errorf("unknown MCP server %q", name)
	}
	if c.tr != nil {
		_ = c.tr.close()
		c.tr = nil
	}
	err := m.dial(c)
	m.rebuildCatalog()
	return err
}

// Call executes a tool by name against its owning server.
func (m *Manager) Call(name string, args json.RawMessage) (string, error) {
	m.mu.RLock()
	c := m.toolHome[name]
	m.mu.RUnlock()
	if c == nil {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return c.callTool(name, args)
}

// Close shuts down all server connections.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		if c.tr != nil {
			_ = c.tr.close()
		}
	}
}
