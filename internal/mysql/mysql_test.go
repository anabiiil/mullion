package mysql

import "testing"

// backupFileNames is BackupDatabases' naming logic factored out so it
// can be tested without a running server: one <name>.sql per database,
// nothing else — in particular no "all-databases.sql", which is the
// whole point of BackupDatabases existing separately from BackupTo.
func TestBackupFileNamesOneSqlFilePerDatabase(t *testing.T) {
	dbs := []string{"shop", "blog", "cms"}
	got := backupFileNames(dbs)
	want := []string{"shop.sql", "blog.sql", "cms.sql"}
	if len(got) != len(want) {
		t.Fatalf("backupFileNames(%v) = %v, want %v", dbs, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backupFileNames(%v)[%d] = %q, want %q", dbs, i, got[i], want[i])
		}
	}
}

func TestBackupFileNamesNoCombinedFile(t *testing.T) {
	for _, name := range backupFileNames([]string{"a", "b"}) {
		if name == "all-databases.sql" {
			t.Errorf("backupFileNames produced a combined file %q — BackupDatabases must not write one", name)
		}
	}
}

func TestBackupFileNamesEmpty(t *testing.T) {
	if got := backupFileNames(nil); len(got) != 0 {
		t.Errorf("backupFileNames(nil) = %v, want empty", got)
	}
}

func TestFormatSizeSwitchesToGB(t *testing.T) {
	cases := []struct {
		bytes float64
		want  string
	}{
		{500_000, "0.5 MB"},
		{5_000_000, "5.0 MB"},
		{999_999_999, "1000.0 MB"},
		{1_000_000_000, "1.00 GB"},
		{12_500_000_000, "12.50 GB"},
	}
	for _, c := range cases {
		if got := formatSize(c.bytes); got != c.want {
			t.Errorf("formatSize(%v) = %q, want %q", c.bytes, got, c.want)
		}
	}
}
