package ui

// The panel's Mail page (Mailpit) endpoints and state, plus the dev
// servers' idle-sleep settings.
//
// The inbox is read through these endpoints rather than from the page:
// Mailpit's API lives on another origin (127.0.0.1:<inbox port>), and
// going through Mullion means no CORS setup and a stable JSON shape for
// the page whatever Mailpit's own looks like.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/mailpit"
)

// mailState is getState's "mail" entry. It must stay cheap — the page
// polls state every few seconds — so it only probes the inbox port and,
// when Mailpit answers, asks it for the unread count (≤1s).
func mailState(a *app.App) map[string]any {
	c := a.State.Config
	st := map[string]any{"installed": c.Mailpit != ""}
	if c.Mailpit == "" {
		return st
	}
	running := mailpit.Running()
	st["version"] = c.Mailpit
	st["label"] = mailpit.Label(c.Mailpit)
	st["running"] = running
	st["stopped"] = c.MailStopped
	st["smtpPort"] = mailpit.SMTPPort
	st["uiPort"] = mailpit.UIPort
	st["url"] = a.MailURL()
	st["directUrl"] = fmt.Sprintf("http://127.0.0.1:%d", mailpit.UIPort)
	st["catchPhp"] = c.MailCatchPHP
	st["env"] = app.MailEnvSnippet(mailpit.SMTPPort)
	st["node"] = app.MailNodeSnippet(mailpit.SMTPPort)
	st["siteTaken"] = a.State.FindSite(mailpit.SiteName) != nil
	if running {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if l, err := mailpit.Messages(ctx, 1); err == nil {
			st["unread"] = l.Unread
			st["total"] = l.Total
		}
	}
	return st
}

// panelAddr is a mailbox as the page shows it.
type panelAddr struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// panelMessage is one inbox row.
type panelMessage struct {
	ID          string      `json:"id"`
	From        panelAddr   `json:"from"`
	To          []panelAddr `json:"to"`
	Subject     string      `json:"subject"`
	Created     string      `json:"created"`
	Read        bool        `json:"read"`
	Snippet     string      `json:"snippet"`
	Size        int64       `json:"size"`
	Attachments int         `json:"attachments"`
}

func panelAddrs(in []mailpit.Address) []panelAddr {
	out := make([]panelAddr, 0, len(in))
	for _, a := range in {
		out = append(out, panelAddr{Name: a.Name, Address: a.Address})
	}
	return out
}

// mailListJSON maps Mailpit's message list to the page's shape (no nil
// slices, a From even when the message had none).
func mailListJSON(l mailpit.List) map[string]any {
	msgs := make([]panelMessage, 0, len(l.Messages))
	for _, m := range l.Messages {
		pm := panelMessage{
			ID: m.ID, Subject: m.Subject, Created: m.Created, Read: m.Read, Snippet: m.Snippet,
			Size: m.Size, Attachments: m.Attachments, To: panelAddrs(append(m.To, m.Cc...)),
		}
		if m.From != nil {
			pm.From = panelAddr{Name: m.From.Name, Address: m.From.Address}
		}
		msgs = append(msgs, pm)
	}
	return map[string]any{"total": l.Total, "unread": l.Unread, "messages": msgs}
}

// mailMessageJSON maps one message (with its headers) for the preview.
func mailMessageJSON(m mailpit.Message, headers map[string][]string) map[string]any {
	from := panelAddr{}
	if m.From != nil {
		from = panelAddr{Name: m.From.Name, Address: m.From.Address}
	}
	type att struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size"`
	}
	atts := []att{}
	for _, p := range m.Attachments {
		atts = append(atts, att{Name: p.FileName, Type: p.ContentType, Size: p.Size})
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hdrs := make([][2]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range headers[k] {
			hdrs = append(hdrs, [2]string{k, v})
		}
	}
	return map[string]any{
		"id": m.ID, "from": from, "to": panelAddrs(m.To), "cc": panelAddrs(m.Cc), "replyTo": panelAddrs(m.ReplyTo),
		"subject": m.Subject, "date": m.Date, "text": m.Text, "html": m.HTML, "size": m.Size,
		"attachments": atts, "headers": hdrs,
	}
}

func registerMail(api apiRegistrar) {
	// installJob runs an install-class background job on a fresh App.
	installJob := func(fn func(a2 *app.App, setStatus func(string)) (any, error)) (any, error) {
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			a2.Progress = setStatus
			return fn(a2, setStatus)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	}
	// quickJob is for operations that wait on a process (start/stop).
	quickJob := func(fn func(a2 *app.App, setStatus func(string)) (any, error)) (any, error) {
		id := jobs.start(func(setStatus func(string)) (any, error) {
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return fn(a2, setStatus)
		})
		return map[string]string{"job": id}, nil
	}
	reqCtx := func(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(r.Context(), d)
	}

	api("/api/mail/install", func(a *app.App, r *http.Request) (any, error) {
		upgrade := a.State.Config.Mailpit != ""
		return installJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Checking the latest Mailpit…")
			v, err := a2.InstallMail(context.Background(), func(done, total int64) {
				if total > 0 {
					setStatus(fmt.Sprintf("Downloading Mailpit… %d%% (%.1f / %.1f MB)", done*100/total, float64(done)/1e6, float64(total)/1e6))
				} else {
					setStatus(fmt.Sprintf("Downloading Mailpit… %.1f MB", float64(done)/1e6))
				}
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"version": v, "upgraded": upgrade}, nil
		})
	})
	api("/api/mail/update", func(a *app.App, r *http.Request) (any, error) {
		ctx, cancel := reqCtx(r, 5*time.Second)
		defer cancel()
		return map[string]string{"latest": a.MailUpdate(ctx)}, nil
	})
	api("/api/mail/start", func(a *app.App, r *http.Request) (any, error) {
		return quickJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Starting Mail…")
			return nil, a2.StartMail()
		})
	})
	api("/api/mail/stop", func(a *app.App, r *http.Request) (any, error) {
		return quickJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Stopping Mail…")
			return nil, a2.StopMail()
		})
	})
	api("/api/mail/restart", func(a *app.App, r *http.Request) (any, error) {
		return quickJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Restarting Mail…")
			return nil, a2.RestartMail()
		})
	})
	api("/api/mail/uninstall", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ KeepMail bool }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return installJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Removing Mail…")
			return nil, a2.UninstallMail(in.KeepMail)
		})
	})
	api("/api/mail/catch-php", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ On bool }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return nil, a.SetMailCatchPHP(in.On)
	})

	/* ── inbox ──────────────────────────────────────────────── */
	api("/api/mail/messages", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Limit int }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		if in.Limit <= 0 || in.Limit > 200 {
			in.Limit = 50
		}
		if a.State.Config.Mailpit == "" || !mailpit.Running() {
			return mailListJSON(mailpit.List{}), nil
		}
		ctx, cancel := reqCtx(r, 5*time.Second)
		defer cancel()
		l, err := mailpit.Messages(ctx, in.Limit)
		if err != nil {
			return nil, err
		}
		return mailListJSON(l), nil
	})
	api("/api/mail/message", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := reqCtx(r, 10*time.Second)
		defer cancel()
		m, err := mailpit.GetMessage(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		h, _ := mailpit.Headers(ctx, in.ID)
		return mailMessageJSON(m, h), nil
	})
	api("/api/mail/raw", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := reqCtx(r, 10*time.Second)
		defer cancel()
		return mailpit.Raw(ctx, in.ID)
	})
	// /api/mail/check runs one of Mailpit's checks: "html" (client
	// support, caniemail data) or "links" (Mailpit requests every link —
	// can take a while). Spam scoring needs a SpamAssassin server and is
	// only offered when Mailpit reports one.
	api("/api/mail/check", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ ID, Kind string }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := reqCtx(r, 60*time.Second)
		defer cancel()
		switch in.Kind {
		case "html":
			return mailpit.CheckHTML(ctx, in.ID)
		case "links":
			return mailpit.CheckLinks(ctx, in.ID)
		}
		return nil, fmt.Errorf("unknown check %q", in.Kind)
	})
	api("/api/mail/delete", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ IDs []string }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := reqCtx(r, 10*time.Second)
		defer cancel()
		return nil, mailpit.Delete(ctx, in.IDs)
	})
	api("/api/mail/clear", func(a *app.App, r *http.Request) (any, error) {
		ctx, cancel := reqCtx(r, 10*time.Second)
		defer cancel()
		return nil, mailpit.DeleteAll(ctx)
	})
	api("/api/mail/read", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			IDs  []string
			Read bool
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := reqCtx(r, 10*time.Second)
		defer cancel()
		return nil, mailpit.SetRead(ctx, in.IDs, in.Read)
	})
	api("/api/mail/send", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			From, Subject, Text, HTML string
			To                        []string
			Attachments               []struct{ Name, Type, Data string }
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		if a.State.Config.Mailpit == "" {
			return nil, fmt.Errorf("Mail is not installed")
		}
		m := mailpit.TestMessage{From: in.From, To: in.To, Subject: in.Subject, Text: in.Text, HTML: in.HTML}
		for _, at := range in.Attachments {
			data, err := base64.StdEncoding.DecodeString(at.Data)
			if err != nil {
				return nil, fmt.Errorf("attachment %s: %v", at.Name, err)
			}
			m.Attachments = append(m.Attachments, mailpit.Attachment{Name: at.Name, ContentType: at.Type, Data: data})
		}
		return nil, mailpit.SendTest(m)
	})

	/* ── release relay ──────────────────────────────────────── */
	api("/api/mail/relay", func(a *app.App, r *http.Request) (any, error) {
		c := mailpit.LoadRelay(a.Paths)
		out := map[string]any{
			"enabled": c.Enabled, "host": c.Host, "port": c.Port, "username": c.Username,
			"hasPassword": c.Password != "", "security": c.Security, "allowedRecipients": c.AllowedRecipients,
		}
		if c.Security == "" {
			out["security"] = "starttls"
		}
		if mailpit.Running() {
			ctx, cancel := reqCtx(r, 2*time.Second)
			defer cancel()
			if w, err := mailpit.UIConfig(ctx); err == nil {
				out["active"] = w.MessageRelay.Enabled
				out["spamAssassin"] = w.SpamAssassin
			}
		}
		return out, nil
	})
	api("/api/mail/relay/save", func(a *app.App, r *http.Request) (any, error) {
		var in mailpit.RelayConfig
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return quickJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Saving the relay and restarting Mail…")
			return nil, a2.SaveMailRelay(in)
		})
	})
	api("/api/mail/release", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			ID string
			To []string
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		var to []string
		for _, s := range in.To {
			for _, p := range strings.Split(s, ",") {
				if p = strings.TrimSpace(p); p != "" {
					to = append(to, p)
				}
			}
		}
		if !mailpit.LoadRelay(a.Paths).Enabled {
			return nil, fmt.Errorf("set up a relay first (Mail → Release to a real inbox)")
		}
		ctx, cancel := reqCtx(r, 60*time.Second)
		defer cancel()
		return nil, mailpit.ReleaseMessage(ctx, in.ID, to)
	})

	/* ── dev servers' idle sleep (Settings) ─────────────────── */
	api("/api/dev/idle", func(a *app.App, r *http.Request) (any, error) {
		noTab, withTab, never := a.State.Config.DevIdle(config.Site{})
		return map[string]any{"noTabMin": int(noTab.Minutes()), "withTabMin": int(withTab.Minutes()), "never": never,
			"defaultNoTabMin": int(config.DefaultDevIdleNoTab.Minutes()), "defaultWithTabMin": int(config.DefaultDevIdleWithTab.Minutes())}, nil
	})
	api("/api/dev/idle/set", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			NoTabMin, WithTabMin int
			Never                bool
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return nil, a.SetDevIdle(in.NoTabMin, in.WithTabMin, in.Never)
	})
	api("/api/sites/idle", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Name, Mode           string
			NoTabMin, WithTabMin int
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return nil, a.SetSiteDevIdle(in.Name, in.Mode, in.NoTabMin, in.WithTabMin)
	})
}
