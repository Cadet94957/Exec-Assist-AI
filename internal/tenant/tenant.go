// Package tenant owns the registry of contractors using the product and hands
// out their two stores.
//
// Each tenant gets its own directory holding two SQLite files: ledger.db for
// operational records and memory.db for the memoryweb graph. That isolation is
// deliberate — it makes "export everything you know about my business" a file
// copy, and it means one tenant's data can never leak into another's recall.
package tenant

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	_ "github.com/mattn/go-sqlite3"
)

// ErrNotFound is returned when no tenant matches.
var ErrNotFound = errors.New("tenant: not found")

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// Tenant is one contractor's account.
type Tenant struct {
	ID        string
	Name      string
	Trade     string
	Timezone  string
	Phone     string
	CreatedAt time.Time
}

// Location resolves the tenant's timezone, falling back to UTC. Briefs are
// scheduled in local time, so this is load-bearing.
func (t Tenant) Location() *time.Location {
	if t.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// StartOfDay returns local midnight for the tenant on the day containing now.
//
// time.Truncate(24*time.Hour) is the tempting one-liner here and it is wrong:
// it truncates relative to the zero time in UTC, not to a wall-clock day. For a
// Los Angeles tenant that puts the start of "today" at 5pm yesterday, so the
// evening's stops land in the wrong brief. Constructing the date in the
// tenant's own location is the only version that survives DST.
func (t Tenant) StartOfDay(now time.Time) time.Time {
	loc := t.Location()
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// Stores is one tenant's pair of open databases.
type Stores struct {
	Ledger *ledger.Store
	Memory *memory.Store
}

// Manager is the tenant registry plus a cache of open store handles.
type Manager struct {
	root string
	reg  *sql.DB

	mu   sync.Mutex
	open map[string]*Stores
}

const registrySchema = `
CREATE TABLE IF NOT EXISTS tenants (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    trade       TEXT NOT NULL DEFAULT 'glazier',
    timezone    TEXT NOT NULL DEFAULT 'UTC',
    phone       TEXT,
    key_hash    TEXT NOT NULL,
    created_at  DATETIME NOT NULL
);`

// NewManager opens (creating if needed) the registry under root.
func NewManager(root string) (*Manager, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("tenant: create root %s: %w", root, err)
	}
	regPath := filepath.Join(root, "registry.db")
	reg, err := sql.Open("sqlite3", "file:"+regPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("tenant: open registry: %w", err)
	}
	if _, err := reg.Exec(registrySchema); err != nil {
		reg.Close()
		return nil, fmt.Errorf("tenant: migrate registry: %w", err)
	}
	return &Manager{root: root, reg: reg, open: make(map[string]*Stores)}, nil
}

// Close closes every open tenant store and the registry. Closing the tenant
// stores checkpoints their WALs, so the files on disk are self-sufficient.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for id, s := range m.open {
		if err := s.Ledger.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := s.Memory.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.open, id)
	}
	if err := m.reg.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Create registers a new tenant and returns it along with a one-time access
// key. The key is not recoverable — only its hash is stored.
func (m *Manager) Create(id, name, trade, timezone, phone string) (Tenant, string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !idPattern.MatchString(id) {
		return Tenant{}, "", fmt.Errorf("tenant: id %q must be 3-40 chars of a-z, 0-9 and dashes", id)
	}
	if strings.TrimSpace(name) == "" {
		return Tenant{}, "", errors.New("tenant: name is required")
	}
	if timezone == "" {
		timezone = "UTC"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return Tenant{}, "", fmt.Errorf("tenant: unknown timezone %q: %w", timezone, err)
	}
	if trade == "" {
		trade = "glazier"
	}

	key, err := newKey()
	if err != nil {
		return Tenant{}, "", err
	}
	now := time.Now().UTC()
	_, err = m.reg.Exec(
		`INSERT INTO tenants (id, name, trade, timezone, phone, key_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, name, trade, timezone, phone, hashKey(key), now)
	if err != nil {
		return Tenant{}, "", fmt.Errorf("tenant: create %s: %w", id, err)
	}

	// Touch the stores so the directory and both databases exist immediately —
	// a tenant that has never been opened should still be backed up tonight.
	if _, err := m.Stores(id); err != nil {
		return Tenant{}, "", err
	}

	return Tenant{ID: id, Name: name, Trade: trade, Timezone: timezone, Phone: phone, CreatedAt: now}, key, nil
}

// Get loads one tenant.
func (m *Manager) Get(id string) (Tenant, error) {
	var t Tenant
	err := m.reg.QueryRow(
		`SELECT id, name, trade, timezone, COALESCE(phone, ''), created_at FROM tenants WHERE id = ?`, id,
	).Scan(&t.ID, &t.Name, &t.Trade, &t.Timezone, &t.Phone, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant: get %s: %w", id, err)
	}
	return t, nil
}

// List returns every tenant, oldest first. The nightly jobs walk this.
func (m *Manager) List() ([]Tenant, error) {
	rows, err := m.reg.Query(
		`SELECT id, name, trade, timezone, COALESCE(phone, ''), created_at FROM tenants ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("tenant: list: %w", err)
	}
	defer rows.Close()

	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.Trade, &t.Timezone, &t.Phone, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("tenant: scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Authenticate returns the tenant if the access key matches. The comparison is
// constant-time so a wrong key leaks nothing by timing.
func (m *Manager) Authenticate(id, key string) (Tenant, error) {
	var stored string
	err := m.reg.QueryRow(`SELECT key_hash FROM tenants WHERE id = ?`, id).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		// Still hash, so a missing tenant costs the same as a wrong key.
		hashKey(key)
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant: authenticate %s: %w", id, err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(hashKey(key))) != 1 {
		return Tenant{}, ErrNotFound
	}
	return m.Get(id)
}

// Dir is the directory holding this tenant's databases.
func (m *Manager) Dir(id string) string { return filepath.Join(m.root, "tenants", id) }

// Stores returns this tenant's open ledger and memory stores, opening them on
// first use and caching the handles. Both SQLite handles are internally pooled
// and safe for concurrent use.
func (m *Manager) Stores(id string) (*Stores, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.open[id]; ok {
		return s, nil
	}

	dir := m.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tenant: create dir %s: %w", dir, err)
	}

	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		return nil, err
	}
	mem, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		led.Close()
		return nil, err
	}

	s := &Stores{Ledger: led, Memory: mem}
	m.open[id] = s
	return s, nil
}

func newKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tenant: generate key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
