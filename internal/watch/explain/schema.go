package explain

// The schema read behind the Performance tab's database checks: every
// table of the connection's database (or schema) with its row estimate,
// columns and indexes, plus the foreign keys — catalog reads only,
// through the same PHP script and connection as Explain.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ForeignKey is one foreign key constraint.
type ForeignKey struct {
	Table   string   `json:"table"`
	Name    string   `json:"name"`
	Ref     string   `json:"ref"`
	Columns []string `json:"columns"`
}

// Schema is a database's tables and foreign keys.
type Schema struct {
	Engine      string            `json:"engine"`
	Conn        string            `json:"conn"`
	Tables      map[string]*Table `json:"tables"`
	ForeignKeys []ForeignKey      `json:"foreignKeys"`
	// Truncated: more than 400 tables (only the first were read).
	Truncated bool `json:"truncated,omitempty"`
}

// ReadSchema reads the connection's schema.
func ReadSchema(ctx context.Context, run Runner, c Conn) (*Schema, error) {
	req, _ := json.Marshal(map[string]any{
		"mode": "schema",
		"conn": map[string]any{"driver": c.Driver, "host": c.Host, "port": c.Port, "socket": c.Socket,
			"database": c.Database, "username": c.Username, "password": c.Password},
	})
	cctx, cancel := context.WithTimeout(ctx, 2*Timeout)
	defer cancel()
	out, err := run(cctx, req)
	if err != nil {
		return nil, errors.New(scrub(err.Error(), c.Password))
	}
	var r struct {
		OK          bool              `json:"ok"`
		Error       string            `json:"error"`
		Driver      string            `json:"driver"`
		Tables      map[string]*Table `json:"tables"`
		ForeignKeys []ForeignKey      `json:"foreignKeys"`
		Truncated   bool              `json:"truncated"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil {
		s := strings.TrimSpace(string(out))
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		return nil, errors.New(scrub("unexpected output from PHP: "+s, c.Password))
	}
	if !r.OK {
		return nil, errors.New(scrub(fmt.Sprintf("%s: %s", c.Label(), r.Error), c.Password))
	}
	if r.Tables == nil {
		r.Tables = map[string]*Table{}
	}
	return &Schema{Engine: r.Driver, Conn: c.Label(), Tables: r.Tables, ForeignKeys: r.ForeignKeys, Truncated: r.Truncated}, nil
}
