package tenant_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

func prov() memory.Provenance {
	return memory.Provenance{
		SourceType: "contractor",
		ObservedAt: time.Now().UTC(),
		Note:       "Told to us directly.",
		Sources:    []memory.SourceRef{{Kind: "appointment", RefID: "appt-1"}},
	}
}

// §24's required operational test, run for real: back up, restore into a clean
// environment, open both stores, read the ledger, query memory.
//
// A backup that has never been restored is not a backup — it is a file with a
// reassuring name. This test is the only thing that makes the claim true.
func TestBackupRestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	m, err := tenant.NewManager(root)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer m.Close()

	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "America/Los_Angeles", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	stores, err := m.Stores("dave-glass")
	if err != nil {
		t.Fatalf("stores: %v", err)
	}

	// Something in each store, so the restore has to carry both.
	start := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	if err := stores.Ledger().UpsertAppointment(ledger.Appointment{
		ID: "appt-1", Title: "Harborview — storefront reglaze", StartsAt: start,
		Location: sql.NullString{String: "88 Harbor Way", Valid: true}, Source: "manual",
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	node, err := stores.Memory().Record(memory.Fact{
		Label: "Harborview gate code is 4417", Domain: "properties",
		WhyMatters: "Without it the crew waits at the gate.",
		Kind:       memory.Reference, Status: memory.Observed, Provenance: prov(),
	})
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if err := stores.Memory().Confirm(node.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	backups := t.TempDir()
	snap, err := m.Backup("dave-glass", backups)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if snap.Bytes == 0 {
		t.Fatal("backup wrote nothing")
	}
	for _, name := range []string{"ledger.db", "memory.db"} {
		if _, err := os.Stat(filepath.Join(snap.Dir, name)); err != nil {
			t.Fatalf("snapshot is missing %s: %v", name, err)
		}
	}
	// The snapshot must not leave scratch files behind in the live directory.
	live, err := os.ReadDir(m.Dir("dave-glass"))
	if err != nil {
		t.Fatalf("read live dir: %v", err)
	}
	for _, e := range live {
		if strings.Contains(e.Name(), ".bak") {
			t.Errorf("backup left a scratch file behind: %s", e.Name())
		}
	}

	// Restore into a clean environment, exactly as a real recovery would.
	clean := t.TempDir()
	if err := tenant.Restore(snap.Dir, clean, "dave-glass"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	dir := filepath.Join(clean, "tenants", "dave-glass")
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("open restored ledger: %v", err)
	}
	defer led.Close()
	mem, err := memory.Open(filepath.Join(dir, "memory.db"), led.DB())
	if err != nil {
		t.Fatalf("open restored memory: %v", err)
	}
	defer mem.Close()

	appts, err := led.Between(start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("read restored ledger: %v", err)
	}
	if len(appts) != 1 || appts[0].Title != "Harborview — storefront reglaze" {
		t.Fatalf("restored ledger has %d appointments: %+v", len(appts), appts)
	}

	got, err := mem.Recall("Harborview", "", 10)
	if err != nil {
		t.Fatalf("query restored memory: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("restored memory recalled nothing")
	}

	// Status and provenance must survive too, or the restored assistant knows
	// things it can no longer explain or justify trusting.
	if got[0].Status != memory.Confirmed {
		t.Errorf("restored status = %q, want confirmed", got[0].Status)
	}
	if len(got[0].Provenance.Sources) == 0 {
		t.Error("provenance did not survive the restore")
	}
}

// Restoring over live data would turn a recoverable incident into an
// unrecoverable one.
func TestRestoreRefusesToOverwrite(t *testing.T) {
	root := t.TempDir()
	m, err := tenant.NewManager(root)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer m.Close()

	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", "UTC", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Stores("dave-glass"); err != nil {
		t.Fatalf("stores: %v", err)
	}

	backups := t.TempDir()
	snap, err := m.Backup("dave-glass", backups)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	if err := tenant.Restore(snap.Dir, root, "dave-glass"); err == nil {
		t.Error("restore overwrote a live tenant directory")
	}
}

func TestBackupAllCoversEveryTenant(t *testing.T) {
	root := t.TempDir()
	m, err := tenant.NewManager(root)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer m.Close()

	for _, id := range []string{"dave-glass", "kim-glazing"} {
		if _, _, err := m.Create(id, id, "glazier", "UTC", ""); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	snaps, err := m.BackupAll(t.TempDir())
	if err != nil {
		t.Fatalf("backup all: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("backed up %d tenants, want 2", len(snaps))
	}
}
