package version

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type GlobalMigrationStatus struct {
	ServiceName         string
	LastMigratedVersion string
}

var (
	GlobalMigrationStatusDirPath = "/var/lib/casaos/migration"

	ErrInvalidServiceName = errors.New("service name should not contain space or upper case letter")
)

// Done persists the migration status atomically. Wave 35-Q residual:
// previously called os.WriteFile directly, which truncates-then-writes
// in two syscalls. If the daemon crashed (or the box lost power) between
// truncate and the final write, the next start-up read a zero-byte file
// and treated the migration as "never run", causing it to re-run a
// destructive schema migration on a database that had already been
// upgraded. Fix: write to a sibling temp file, fsync it, then rename
// atomically over the destination, and treat zero-byte status files as
// "unknown" on read.
func (m *GlobalMigrationStatus) Done(version string) error {
	// error if version does not start with 'v'
	if strings.HasPrefix(version, "v") {
		m.LastMigratedVersion = version
	} else {
		m.LastMigratedVersion = "v" + version
	}

	// create runtimePath if not exists
	if _, err := os.Stat(GlobalMigrationStatusDirPath); os.IsNotExist(err) {
		if err := os.MkdirAll(GlobalMigrationStatusDirPath, 0o755); err != nil {
			return err
		}
	}

	dest := m.GetGlobalMigrationStatusFilePath()
	tmp, err := os.CreateTemp(GlobalMigrationStatusDirPath, "."+m.ServiceName+".status.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write([]byte(m.LastMigratedVersion)); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		cleanup()
		return err
	}
	return nil
}

func GetGlobalMigrationStatus(serviceName string) (*GlobalMigrationStatus, error) {
	if err := validateServiceName(serviceName); err != nil {
		return nil, err
	}

	m := &GlobalMigrationStatus{
		ServiceName:         serviceName,
		LastMigratedVersion: "",
	}

	filepath := m.GetGlobalMigrationStatusFilePath()

	if info, err := os.Stat(filepath); !os.IsNotExist(err) {
		// Wave 35-Q: a zero-byte status file means a previous Done() crashed
		// mid-write. Treat it as "unknown" rather than a successful run.
		if err == nil && info.Size() == 0 {
			return m, nil
		}
		buf, err := os.ReadFile(filepath)
		if err != nil {
			return nil, err
		}
		m.LastMigratedVersion = strings.TrimSpace(string(buf))
	}

	return m, nil
}

func (m *GlobalMigrationStatus) GetGlobalMigrationStatusFilePath() string {
	return filepath.Join(GlobalMigrationStatusDirPath, m.ServiceName+".status")
}

func validateServiceName(serviceName string) error {
	// should not contain space
	if strings.Contains(serviceName, " ") {
		return ErrInvalidServiceName
	}

	// should be all lower case
	if strings.ToLower(serviceName) != serviceName {
		return ErrInvalidServiceName
	}

	return nil
}
