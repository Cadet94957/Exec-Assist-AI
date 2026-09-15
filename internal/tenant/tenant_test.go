package tenant_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

func TestMain(m *testing.M) {
	os.Setenv("MEMORYWEB_OLLAMA_ENDPOINT", "disabled")
	os.Exit(m.Run())
}

func manager(t *testing.T) *tenant.Manager {
	t.Helper()
	m, err := tenant.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// One contractor must never be able to recall another's business. Because each
// tenant gets its own pair of database files this is structural rather than a
// query-level filter — this test is what proves the wiring actually honours it.
func TestTenantsAreIsolated(t *testing.T) {
	m := manager(t)

	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "UTC", ""); err != nil {
		t.Fatalf("create dave: %v", err)
	}
	if _, _, err := m.Create("kim-glazing", "Kim Glazing", "glazier", "UTC", ""); err != nil {
		t.Fatalf("create kim: %v", err)
	}

	dave, err := m.Stores("dave-glass")
	if err != nil {
		t.Fatalf("dave stores: %v", err)
	}
	kim, err := m.Stores("kim-glazing")
	if err != nil {
		t.Fatalf("kim stores: %v", err)
	}

	if _, err := dave.Memory().Record(memory.Fact{
		Label:      "Harborview gate code is 4417",
		Domain:     "properties",
		WhyMatters: "Without it the crew waits at the gate.",
		Kind:       memory.Reference,
		Status:     memory.Observed,
		Provenance: memory.Provenance{
			SourceType: "contractor",
			ObservedAt: time.Now(),
			Note:       "Told to us directly.",
			Sources:    []memory.SourceRef{{Kind: "appointment", RefID: "a1"}},
		},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	found, err := dave.Memory().Recall("Harborview", "", 10)
	if err != nil {
		t.Fatalf("dave recall: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("dave cannot recall his own fact")
	}

	leaked, err := kim.Memory().Recall("Harborview", "", 10)
	if err != nil {
		t.Fatalf("kim recall: %v", err)
	}
	if len(leaked) != 0 {
		t.Fatalf("kim recalled %d of dave's facts — tenants are not isolated", len(leaked))
	}

	if dave.Memory().Path() == kim.Memory().Path() {
		t.Error("both tenants share one memory database")
	}
	if dave.Ledger().Path() == kim.Ledger().Path() {
		t.Error("both tenants share one ledger database")
	}
}

func TestStoresAreCachedPerTenant(t *testing.T) {
	m := manager(t)
	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "UTC", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	a, err := m.Stores("dave-glass")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := m.Stores("dave-glass")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a != b {
		t.Error("repeated Stores calls opened the databases twice")
	}
}

func TestAuthenticate(t *testing.T) {
	m := manager(t)

	created, key, err := m.Create("dave-glass", "Dave's Glass", "glazier", "America/Los_Angeles", "+15550100")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := m.Authenticate("dave-glass", key)
	if err != nil {
		t.Fatalf("authenticate with the right key: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name {
		t.Errorf("authenticated as %+v, want %+v", got, created)
	}

	if _, err := m.Authenticate("dave-glass", "wrong"); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("wrong key: got %v, want ErrNotFound", err)
	}
	if _, err := m.Authenticate("nobody", key); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("unknown tenant: got %v, want ErrNotFound", err)
	}
}

func TestCreateValidatesInput(t *testing.T) {
	m := manager(t)

	for _, tc := range []struct{ name, id, tenantName, tz string }{
		{"empty id", "", "Dave's Glass", "UTC"},
		{"id too short", "ab", "Dave's Glass", "UTC"},
		{"id with spaces", "dave glass", "Dave's Glass", "UTC"},
		{"id with uppercase punctuation", "dave_glass", "Dave's Glass", "UTC"},
		{"empty name", "dave-glass", "", "UTC"},
		{"unknown timezone", "dave-glass", "Dave's Glass", "Mars/Olympus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := m.Create(tc.id, tc.tenantName, "glazier", tc.tz, ""); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestDuplicateTenantIsRejected(t *testing.T) {
	m := manager(t)
	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "UTC", ""); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, _, err := m.Create("dave-glass", "Someone Else", "glazier", "UTC", ""); err == nil {
		t.Error("expected an error creating a duplicate id")
	}
}

// Briefs fire at 6am local, so a tenant's timezone has to survive the round
// trip. A silent fallback to UTC would send a Los Angeles glazier his brief at
// 11pm the night before.
func TestTimezoneRoundTrips(t *testing.T) {
	m := manager(t)
	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "America/Los_Angeles", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := m.Get("dave-glass")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Timezone != "America/Los_Angeles" {
		t.Errorf("timezone = %q, want America/Los_Angeles", got.Timezone)
	}
	if got.Location().String() != "America/Los_Angeles" {
		t.Errorf("location = %q, want America/Los_Angeles", got.Location())
	}
}

func TestListAndGetMissing(t *testing.T) {
	m := manager(t)
	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "UTC", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	all, err := m.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list returned %d tenants, want 1", len(all))
	}
	if _, err := m.Get("nobody"); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("get missing: got %v, want ErrNotFound", err)
	}
}

// The whole product is a daily brief, so "which day is it for this contractor"
// has to be exactly right. time.Truncate(24*time.Hour) looks like the answer
// and silently starts the day at 5pm the night before in Los Angeles.
func TestStartOfDayIsLocalMidnight(t *testing.T) {
	m := manager(t)
	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "America/Los_Angeles", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	dave, err := m.Get("dave-glass")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	cases := []struct {
		name string
		now  time.Time
		// Local midnight as a UTC instant: PDT (UTC-7) in summer, PST (UTC-8) in winter.
		wantUTC time.Time
	}{
		{
			name:    "afternoon in summer",
			now:     time.Date(2026, 7, 15, 21, 30, 0, 0, time.UTC), // 2:30pm PDT
			wantUTC: time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC),
		},
		{
			name:    "late evening local is still the same local day",
			now:     time.Date(2026, 7, 16, 4, 30, 0, 0, time.UTC), // 9:30pm PDT on the 15th
			wantUTC: time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC),
		},
		{
			name:    "afternoon in winter",
			now:     time.Date(2026, 1, 15, 22, 30, 0, 0, time.UTC), // 2:30pm PST
			wantUTC: time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dave.StartOfDay(tc.now)
			if !got.UTC().Equal(tc.wantUTC) {
				t.Errorf("StartOfDay(%v) = %v (%v UTC), want %v UTC",
					tc.now, got, got.UTC(), tc.wantUTC)
			}
			if h, mi, sec := got.Clock(); h != 0 || mi != 0 || sec != 0 {
				t.Errorf("local clock = %02d:%02d:%02d, want midnight", h, mi, sec)
			}
			// A stop at 8am local must land inside the day's window.
			end := got.AddDate(0, 0, 1)
			eightAM := got.Add(8 * time.Hour)
			if eightAM.Before(got) || !eightAM.Before(end) {
				t.Errorf("8am local (%v) falls outside [%v, %v)", eightAM, got, end)
			}
		})
	}
}

// A tenant with no timezone must still get a coherent day rather than a panic
// or a zero time.
func TestStartOfDayFallsBackToUTC(t *testing.T) {
	m := manager(t)
	if _, _, err := m.Create("utc-shop", "UTC Shop", "glazier", "UTC", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	shop, err := m.Get("utc-shop")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	now := time.Date(2026, 7, 15, 21, 30, 0, 0, time.UTC)
	got := shop.StartOfDay(now)
	want := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("StartOfDay = %v, want %v", got, want)
	}
}
