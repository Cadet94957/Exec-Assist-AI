package tenant

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
)

// Snapshot is one tenant's backup on disk.
type Snapshot struct {
	TenantID string
	Dir      string
	Taken    time.Time
	Bytes    int64
}

// Backup writes a consistent snapshot of one tenant into destRoot.
//
// Both databases are copied through SQLite's own snapshot mechanism rather
// than by copying files: a live database has writes sitting in its WAL, so a
// naive file copy can produce something that opens fine and is missing the
// last hour of work.
func (m *Manager) Backup(id, destRoot string) (Snapshot, error) {
	if _, err := m.Get(id); err != nil {
		return Snapshot{}, err
	}

	src := m.Dir(id)
	stamp := time.Now().UTC()
	dest := filepath.Join(destRoot, id, stamp.Format("20060102T150405Z"))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("tenant: create backup directory: %w", err)
	}

	var total int64
	for _, name := range []string{"ledger.db", "memory.db"} {
		// memoryweb's Backup refuses a destination outside the source
		// directory, so the snapshot is taken beside the live file, copied
		// out, and removed.
		local, err := memory.Backup(filepath.Join(src, name))
		if err != nil {
			return Snapshot{}, err
		}
		n, err := copyFile(local, filepath.Join(dest, name))
		os.Remove(local) //nolint:errcheck
		if err != nil {
			return Snapshot{}, err
		}
		total += n
	}

	return Snapshot{TenantID: id, Dir: dest, Taken: stamp, Bytes: total}, nil
}

// BackupAll snapshots every tenant, returning what it managed and the first
// failure. A single broken tenant must not stop the rest being backed up.
func (m *Manager) BackupAll(destRoot string) ([]Snapshot, error) {
	all, err := m.List()
	if err != nil {
		return nil, err
	}
	var (
		out      []Snapshot
		firstErr error
	)
	for _, t := range all {
		snap, err := m.Backup(t.ID, destRoot)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", t.ID, err)
			}
			continue
		}
		out = append(out, snap)
	}
	return out, firstErr
}

// Restore rebuilds a tenant's data directory from a snapshot into destRoot.
// It refuses to overwrite: a restore that silently replaces live data is a way
// to turn a recoverable incident into an unrecoverable one.
func Restore(snapshotDir, destRoot, id string) error {
	target := filepath.Join(destRoot, "tenants", id)
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("tenant: %s already exists — restore into a clean directory", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("tenant: check restore target: %w", err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("tenant: create restore target: %w", err)
	}

	for _, name := range []string{"ledger.db", "memory.db"} {
		src := filepath.Join(snapshotDir, name)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("tenant: snapshot is missing %s: %w", name, err)
		}
		if _, err := copyFile(src, filepath.Join(target, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dest string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("tenant: open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("tenant: create %s: %w", dest, err)
	}
	defer out.Close()

	n, err := io.Copy(out, in)
	if err != nil {
		return 0, fmt.Errorf("tenant: copy to %s: %w", dest, err)
	}
	if err := out.Sync(); err != nil {
		return 0, fmt.Errorf("tenant: flush %s: %w", dest, err)
	}
	return n, nil
}
