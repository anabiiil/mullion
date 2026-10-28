package explain

import (
	"errors"
	"fmt"
)

var (
	errMulti        = errors.New("only one statement can be explained at a time")
	errEmpty        = errors.New("there's no SQL to explain")
	errInsertValues = errors.New("an INSERT … VALUES has no plan worth reading — only SELECT, UPDATE, DELETE and INSERT … SELECT can be explained")
	errMemorySQLite = errors.New("the app uses an in-memory SQLite database (:memory:) — there's nothing on disk to explain against")
	// ErrAnalyzeSelect is returned when Analyze is asked for a statement
	// that would change data.
	ErrAnalyzeSelect = errors.New("Analyze runs the query, so it is only available for SELECT statements")
)

func errNotExplainable(verb string) error {
	return fmt.Errorf("%s statements can't be explained — only SELECT, UPDATE, DELETE and INSERT … SELECT", verb)
}

func errUnknownConn(name string) error {
	return fmt.Errorf("the query ran on the %q connection, which isn't a stock Laravel connection (mysql, mariadb, pgsql, sqlite, sqlsrv) and isn't in config/database.php — Mullion can't tell which database it is", name)
}

func errNoSQLiteFile(path string) error {
	return fmt.Errorf("the SQLite database %s doesn't exist", path)
}
