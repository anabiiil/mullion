package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateBackupDirRejectsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBackupDir(file); err == nil {
		t.Error("ValidateBackupDir on a file should fail")
	}
}

func TestValidateBackupDirCreatesMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	if err := ValidateBackupDir(dir); err != nil {
		t.Fatalf("ValidateBackupDir: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("ValidateBackupDir did not create %q", dir)
	}
	// The write-probe must not be left behind.
	if _, err := os.Stat(filepath.Join(dir, ".mullion-write-test")); err == nil {
		t.Error("ValidateBackupDir left its write-test file behind")
	}
}

func TestValidateBackupDirRejectsUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based write protection is unreliable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := ValidateBackupDir(dir); err == nil {
		t.Error("ValidateBackupDir on a read-only directory should fail")
	}
}

func TestValidateBackupDirForExportSkipsSpaceCheckWithoutEstimate(t *testing.T) {
	dir := t.TempDir()
	if err := ValidateBackupDirForExport(dir, 0); err != nil {
		t.Errorf("ValidateBackupDirForExport(0) = %v, want nil (space check skipped)", err)
	}
	if err := ValidateBackupDirForExport(dir, -1); err != nil {
		t.Errorf("ValidateBackupDirForExport(-1) = %v, want nil (space check skipped)", err)
	}
}

func TestValidateBackupDirForExportSmallEstimateFits(t *testing.T) {
	dir := t.TempDir()
	if err := ValidateBackupDirForExport(dir, 1024); err != nil {
		t.Errorf("ValidateBackupDirForExport(1KB) = %v, want nil", err)
	}
}

func TestValidateBackupDirForExportHugeEstimateFails(t *testing.T) {
	dir := t.TempDir()
	// No real volume has an exabyte free; this must trip the space check.
	const huge = int64(1) << 60
	if err := ValidateBackupDirForExport(dir, huge); err == nil {
		t.Error("ValidateBackupDirForExport with an impossible estimate should fail")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{500_000, "0.5 MB"},
		{5_000_000, "5.0 MB"},
		{5_000_000_000, "5.0 GB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.n); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestImportSpaceRequirementSkipsUnknownEstimate(t *testing.T) {
	for _, estimate := range []int64{0, -1} {
		backup, datadir := ImportSpaceRequirement(estimate, false)
		if backup != 0 || datadir != 0 {
			t.Errorf("ImportSpaceRequirement(%d, false) = (%d, %d), want (0, 0)", estimate, backup, datadir)
		}
		backup, datadir = ImportSpaceRequirement(estimate, true)
		if backup != 0 || datadir != 0 {
			t.Errorf("ImportSpaceRequirement(%d, true) = (%d, %d), want (0, 0)", estimate, backup, datadir)
		}
	}
}

func TestImportSpaceRequirementDifferentVolumes(t *testing.T) {
	backup, datadir := ImportSpaceRequirement(1_000_000_000, false)
	if backup != 1_200_000_000 {
		t.Errorf("backupDirNeeded = %d, want 1200000000", backup)
	}
	if datadir != 1_300_000_000 {
		t.Errorf("datadirNeeded = %d, want 1300000000", datadir)
	}
}

func TestImportSpaceRequirementSameVolumeAdds(t *testing.T) {
	backup, datadir := ImportSpaceRequirement(1_000_000_000, true)
	const want = uint64(2_500_000_000)
	if backup != want || datadir != want {
		t.Errorf("same-volume ImportSpaceRequirement = (%d, %d), want (%d, %d)", backup, datadir, want, want)
	}
}
