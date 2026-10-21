package mailpit

// A small client for Mailpit's REST API (/api/v1, checked against
// v1.31): the panel reads the inbox through Mullion's own API instead of
// talking to Mailpit from the page, so there is no CORS to configure and
// the inbox port never has to be known by the browser.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var apiClient = &http.Client{Timeout: 15 * time.Second}

// baseURL is where the API answers; a variable so tests can point it at
// an httptest server.
var baseURL = func() string { return "http://127.0.0.1:" + strconv.Itoa(UIPort) }

// Address is a mailbox in a message summary.
type Address struct {
	Name    string `json:"Name"`
	Address string `json:"Address"`
}

// Summary is one message in GET /api/v1/messages.
type Summary struct {
	ID          string    `json:"ID"`
	Read        bool      `json:"Read"`
	From        *Address  `json:"From"`
	To          []Address `json:"To"`
	Cc          []Address `json:"Cc"`
	Subject     string    `json:"Subject"`
	Created     string    `json:"Created"`
	Size        int64     `json:"Size"`
	Attachments int       `json:"Attachments"`
	Snippet     string    `json:"Snippet"`
}

// List is GET /api/v1/messages.
type List struct {
	Total    int       `json:"total"`
	Unread   int       `json:"unread"`
	Count    int       `json:"count"`
	Messages []Summary `json:"messages"`
}

// Part is an attachment of a message.
type Part struct {
	PartID      string `json:"PartID"`
	FileName    string `json:"FileName"`
	ContentType string `json:"ContentType"`
	Size        int64  `json:"Size"`
}

// Message is GET /api/v1/message/{id} (which also marks it read).
type Message struct {
	ID          string    `json:"ID"`
	From        *Address  `json:"From"`
	To          []Address `json:"To"`
	Cc          []Address `json:"Cc"`
	Bcc         []Address `json:"Bcc"`
	ReplyTo     []Address `json:"ReplyTo"`
	Subject     string    `json:"Subject"`
	Date        string    `json:"Date"`
	Text        string    `json:"Text"`
	HTML        string    `json:"HTML"`
	Size        int64     `json:"Size"`
	Attachments []Part    `json:"Attachments"`
	Inline      []Part    `json:"Inline"`
}

// HTMLCheck is the summary of GET /api/v1/message/{id}/html-check:
// how well the HTML is supported across mail clients (caniemail data).
type HTMLCheck struct {
	Total struct {
		Tests       int     `json:"Tests"`
		Nodes       int     `json:"Nodes"`
		Supported   float64 `json:"Supported"`
		Partial     float64 `json:"Partial"`
		Unsupported float64 `json:"Unsupported"`
	} `json:"Total"`
	Warnings []struct {
		Title    string `json:"Title"`
		Category string `json:"Category"`
		Score    struct {
			Found       int     `json:"Found"`
			Supported   float64 `json:"Supported"`
			Partial     float64 `json:"Partial"`
			Unsupported float64 `json:"Unsupported"`
		} `json:"Score"`
	} `json:"Warnings"`
}

// LinkCheck is GET /api/v1/message/{id}/link-check: Mailpit requests
// every link in the message and reports the broken ones.
type LinkCheck struct {
	Errors int `json:"Errors"`
	Links  []struct {
		URL        string `json:"URL"`
		StatusCode int    `json:"StatusCode"`
		Status     string `json:"Status"`
	} `json:"Links"`
}

// WebUI is GET /api/v1/webui: which optional features are on.
type WebUI struct {
	MessageRelay struct {
		Enabled           bool   `json:"Enabled"`
		SMTPServer        string `json:"SMTPServer"`
		AllowedRecipients string `json:"AllowedRecipients"`
	} `json:"MessageRelay"`
	SpamAssassin bool `json:"SpamAssassin"`
}

// Info is GET /api/v1/info.
type Info struct {
	Version  string `json:"Version"`
	Messages int    `json:"Messages"`
	Unread   int    `json:"Unread"`
}

// call does one API request and decodes a JSON answer into out (when
// non-nil). Mailpit answers errors as plain text, which is returned.
func call(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		if !Running() {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		text := strings.TrimSpace(string(msg))
		if text == "" {
			text = resp.Status
		}
		return fmt.Errorf("Mailpit: %s", text)
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		*s = string(b)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func msgPath(id, suffix string) string {
	return "/api/v1/message/" + url.PathEscape(id) + suffix
}

// Messages lists the newest messages (limit ≤ 0 = Mailpit's default, 50).
func Messages(ctx context.Context, limit int) (List, error) {
	path := "/api/v1/messages"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var l List
	err := call(ctx, http.MethodGet, path, nil, &l)
	return l, err
}

// GetMessage fetches one message (Mailpit marks it read).
func GetMessage(ctx context.Context, id string) (Message, error) {
	var m Message
	err := call(ctx, http.MethodGet, msgPath(id, ""), nil, &m)
	return m, err
}

// Headers returns a message's headers, each with all its values.
func Headers(ctx context.Context, id string) (map[string][]string, error) {
	h := map[string][]string{}
	err := call(ctx, http.MethodGet, msgPath(id, "/headers"), nil, &h)
	return h, err
}

// Raw returns a message's full source.
func Raw(ctx context.Context, id string) (string, error) {
	var s string
	err := call(ctx, http.MethodGet, msgPath(id, "/raw"), nil, &s)
	return s, err
}

// CheckHTML runs Mailpit's HTML compatibility check on a message.
func CheckHTML(ctx context.Context, id string) (HTMLCheck, error) {
	var c HTMLCheck
	err := call(ctx, http.MethodGet, msgPath(id, "/html-check"), nil, &c)
	return c, err
}

// CheckLinks runs Mailpit's link check on a message (Mailpit fetches
// each link, so this can take a few seconds).
func CheckLinks(ctx context.Context, id string) (LinkCheck, error) {
	var c LinkCheck
	err := call(ctx, http.MethodGet, msgPath(id, "/link-check"), nil, &c)
	return c, err
}

// ReleaseMessage delivers a captured message to real addresses through the
// configured relay (see RelayConfig). Mailpit refuses recipients the
// relay's allowed-recipients pattern doesn't match.
func ReleaseMessage(ctx context.Context, id string, to []string) error {
	if len(to) == 0 {
		return fmt.Errorf("give at least one address to release the message to")
	}
	return call(ctx, http.MethodPost, msgPath(id, "/release"), map[string][]string{"To": to}, nil)
}

// SetRead marks messages read or unread.
func SetRead(ctx context.Context, ids []string, read bool) error {
	return call(ctx, http.MethodPut, "/api/v1/messages", map[string]any{"IDs": ids, "Read": read}, nil)
}

// Delete removes the given messages.
func Delete(ctx context.Context, ids []string) error {
	return call(ctx, http.MethodDelete, "/api/v1/messages", map[string]any{"IDs": ids}, nil)
}

// DeleteAll empties the mailbox.
func DeleteAll(ctx context.Context) error {
	return call(ctx, http.MethodDelete, "/api/v1/messages", map[string]any{}, nil)
}

// UIConfig reports Mailpit's optional features (relay, SpamAssassin).
func UIConfig(ctx context.Context) (WebUI, error) {
	var w WebUI
	err := call(ctx, http.MethodGet, "/api/v1/webui", nil, &w)
	return w, err
}

// GetInfo reports the running Mailpit's version and message counts.
func GetInfo(ctx context.Context) (Info, error) {
	var i Info
	err := call(ctx, http.MethodGet, "/api/v1/info", nil, &i)
	return i, err
}
