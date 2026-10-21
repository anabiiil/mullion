package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"pm/internal/mailpit"
)

func TestMailListJSON(t *testing.T) {
	// Decoded from a trimmed real Mailpit v1.31.6 answer.
	var l mailpit.List
	if err := json.Unmarshal([]byte(`{"total":2,"unread":1,"count":2,"messages":[
	 {"ID":"a1","Read":false,"From":{"Name":"App","Address":"app@example.test"},"To":[{"Name":"Bob","Address":"bob@example.test"}],
	  "Cc":[{"Name":"","Address":"cc@example.test"}],"Subject":"Hello","Created":"2026-10-21T13:23:50.537+03:00","Size":413,"Attachments":1,"Snippet":"Hi there"},
	 {"ID":"b2","Read":true,"From":null,"To":null,"Subject":"","Created":"2026-10-21T13:20:00+03:00"}]}`), &l); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(mailListJSON(l))
	got := string(b)
	for _, want := range []string{
		`"total":2`, `"unread":1`,
		`{"id":"a1","from":{"name":"App","address":"app@example.test"},"to":[{"name":"Bob","address":"bob@example.test"},{"name":"","address":"cc@example.test"}],"subject":"Hello","created":"2026-10-21T13:23:50.537+03:00","read":false,"snippet":"Hi there","size":413,"attachments":1}`,
		`{"id":"b2","from":{"name":"","address":""},"to":[],`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mapped JSON lacks %s\n%s", want, got)
		}
	}
	// An empty inbox is an empty array, never null.
	b, _ = json.Marshal(mailListJSON(mailpit.List{}))
	if !strings.Contains(string(b), `"messages":[]`) {
		t.Errorf("empty: %s", b)
	}
}

func TestMailMessageJSON(t *testing.T) {
	m := mailpit.Message{ID: "a1", Subject: "S", HTML: "<b>x</b>", Text: "x",
		Attachments: []mailpit.Part{{FileName: "a.pdf", ContentType: "application/pdf", Size: 9}}}
	b, _ := json.Marshal(mailMessageJSON(m, map[string][]string{"To": {"b@x"}, "Received": {"r2", "r1"}}))
	got := string(b)
	for _, want := range []string{`"html":"\u003cb\u003ex\u003c/b\u003e"`, `"attachments":[{"name":"a.pdf","type":"application/pdf","size":9}]`,
		`"headers":[["Received","r2"],["Received","r1"],["To","b@x"]]`, `"from":{"name":"","address":""}`, `"cc":[]`} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %s\n%s", want, got)
		}
	}
}
