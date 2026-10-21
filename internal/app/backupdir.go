// This file backs two callers that both need to confirm a folder is
// usable for database backups before committing to it: the control
// panel's Settings page (see SetBackupDir in settings.go, which calls
// ValidateBackupDir) and `mullion setup`'s prompt for where to put a
// MySQL export it's about to take from another server on the machine
// (see importFromExistingMysql in cmd/setup.go, which calls
// ValidateBackupDirForExport). Keeping the checks here means both
// surfaces reject an unusable path with the same wording.
package app

import (
	"fmt"
	"os"
	"path/filepath"

	"pm/internal/diskfree"
)

// ValidateBackupDir checks that dir is usable as a destination for
// database backups: it must not already exist as a file, it is
// created if missing, and a write into it is verified (create+remove
// a temp file). It returns a descriptive error naming exactly what
// failed, suitable for showing directly to the user.
func ValidateBackupDir(dir string) error {
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		return fmt.Errorf("%s is a file, not a folder", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	probe := filepath.Join(dir, ".mullion-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	os.Remove(probe)
	return nil
}

// ValidateBackupDirForExport validates dir like ValidateBackupDir and,
// when neededBytes is known (> 0), also checks that the volume holding
// dir has enough free space for an export of roughly that size, with a
// 20% safety margin. Pass neededBytes <= 0 when the size of what's
// about to be exported couldn't be estimated — the space check is then
// skipped rather than blocking the export over a guess.
func ValidateBackupDirForExport(dir string, neededBytes int64) error {
	if err := ValidateBackupDir(dir); err != nil {
		return err
	}
	if neededBytes <= 0 {
		return nil
	}
	free, err := diskfree.Free(dir)
	if err != nil {
		// Can't tell either way — don't block the export over it.
		return nil
	}
	needed := uint64(float64(neededBytes) * 1.2)
	if free < needed {
		return fmt.Errorf("needs about %s free but %s only has %s available",
			HumanBytes(needed), dir, HumanBytes(free))
	}
	return nil
}

// HumanBytes renders a byte count the way the rest of Mullion's output
// does elsewhere (e.g. download/export progress): one decimal in MB,
// switching to GB once the number gets large. Exported so callers
// outside this package (cmd/setup.go's MySQL-data-directory space
// check) print matching units.
func HumanBytes(n uint64) string {
	const mb = 1e6
	const gb = 1e9
	if n >= gb {
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/mb)
}

// ImportSpaceRequirement computes how many free bytes an import needs
// on each volume it touches, given estimate (the size of what's about
// to be exported, as mysql.DatabasesSize reports it): the backup
// directory needs about 1.2x that (export headroom for mysqldump's
// textual overhead — see ValidateBackupDirForExport), and the volume
// holding Mullion's own MySQL data directory needs about 1.3x
// (restoring a dump back into InnoDB tends to need somewhat more room
// than the dump itself). When sameVolume is true, both copies draw
// from the same pool of free space, so the two margins are added
// together and returned for BOTH values — checking either one against
// that volume's free space then covers both needs at once. estimate <=
// 0 (the size couldn't be determined) returns 0, 0: callers should
// skip the check rather than block the import on a guess.
func ImportSpaceRequirement(estimate int64, sameVolume bool) (backupDirNeeded, datadirNeeded uint64) {
	if estimate <= 0 {
		return 0, 0
	}
	backupDirNeeded = uint64(float64(estimate) * 1.2)
	datadirNeeded = uint64(float64(estimate) * 1.3)
	if sameVolume {
		combined := backupDirNeeded + datadirNeeded
		return combined, combined
	}
	return backupDirNeeded, datadirNeeded
}
