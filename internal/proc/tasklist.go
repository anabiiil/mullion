package proc

import (
	"bytes"
	"encoding/csv"
	"strconv"
)

// TasklistHasPID reports whether the output of
// `tasklist /FI "PID eq N" /FO CSV /NH` lists pid. The PID column is
// compared exactly: a substring check would take 1234 for 123.
func TasklistHasPID(out []byte, pid int) bool {
	r := csv.NewReader(bytes.NewReader(out))
	r.FieldsPerRecord = -1
	want := strconv.Itoa(pid)
	for {
		rec, err := r.Read()
		if err != nil {
			// io.EOF, or a non-CSV line such as "INFO: No tasks…"
			// (which parses as a single field and is skipped below).
			return false
		}
		if len(rec) >= 2 && rec[1] == want {
			return true
		}
	}
}
