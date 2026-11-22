package frankenphp

import (
	"bytes"
	"encoding/csv"
	"strconv"
)

// tasklistImage is the image name (field 1) of pid's row in the output
// of `tasklist /FI "PID eq N" /FO CSV /NH`, or "" when pid isn't listed.
func tasklistImage(out []byte, pid int) string {
	r := csv.NewReader(bytes.NewReader(out))
	r.FieldsPerRecord = -1
	want := strconv.Itoa(pid)
	for {
		rec, err := r.Read()
		if err != nil {
			return ""
		}
		if len(rec) >= 2 && rec[1] == want {
			return rec[0]
		}
	}
}
