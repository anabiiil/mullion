package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/mailpit"
	"pm/internal/term"
)

var mailCmd = &cobra.Command{
	Use:   "mail",
	Short: "Mail — a local mail server (Mailpit) that catches every email your apps send",
	Long: `Mail runs Mailpit (github.com/axllent/mailpit): an SMTP server on
127.0.0.1:1025 that accepts any message and keeps it in a web inbox at
https://mail.<tld> instead of delivering it. Point an app's mailer at it
(` + "`mullion mail env`" + ` prints the Laravel .env lines) and nothing ever
reaches a real inbox — unless you release a message on purpose through
a relay set up on the Mail page.`,
	RunE: func(cmd *cobra.Command, args []string) error { return mailStatus(mustApp()) },
}

func mailProgress(label string) mailpit.Progress {
	return func(done, total int64) {
		if total > 0 {
			fmt.Printf("\r  %s %3d%%  %.1f / %.1f MB%s", label, done*100/total, float64(done)/1e6, float64(total)/1e6, term.ClearLine())
		} else {
			fmt.Printf("\r  %s  %.1f MB%s", label, float64(done)/1e6, term.ClearLine())
		}
	}
}

var mailInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install (or upgrade to) the latest Mailpit and start it",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		prev := a.State.Config.Mailpit
		fmt.Println("Downloading the latest Mailpit...")
		v, err := a.InstallMail(cmd.Context(), mailProgress("mailpit"))
		fmt.Println()
		if err != nil {
			return err
		}
		switch {
		case prev == "":
			fmt.Println(term.Green("✓ Mail is running:"), mailpit.Label(v))
		case prev != v:
			fmt.Printf("%s %s → %s\n", term.Green("✓ Mail upgraded:"), prev, v)
		default:
			fmt.Println(term.Green("✓"), mailpit.Label(v), "is already the latest — it's running.")
		}
		return mailStatus(a)
	},
}

var mailUninstallKeep bool

var mailUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove Mailpit (--keep-mail keeps the caught messages for a reinstall)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if a.State.Config.Mailpit == "" {
			fmt.Println("Mail is not installed.")
			return nil
		}
		if err := a.UninstallMail(mailUninstallKeep); err != nil {
			return err
		}
		if mailUninstallKeep {
			fmt.Println("Mail removed. Messages kept in", mailpit.DBFile(a.Paths))
		} else {
			fmt.Println("Mail removed.")
		}
		return nil
	},
}

var mailStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start Mail",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.StartMail(); err != nil {
			return err
		}
		return mailStatus(a)
	},
}

var mailStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop Mail (it stays stopped until `mullion mail start`)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.StopMail(); err != nil {
			return err
		}
		fmt.Println("Mail stopped.")
		return nil
	},
}

var mailRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart Mail",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.RestartMail(); err != nil {
			return err
		}
		return mailStatus(a)
	},
}

var mailStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Mail's state, ports and inbox",
	RunE:  func(cmd *cobra.Command, args []string) error { return mailStatus(mustApp()) },
}

func mailStatus(a *app.App) error {
	c := a.State.Config
	if c.Mailpit == "" {
		fmt.Println("Mail is not installed (run: mullion mail install)")
		return nil
	}
	fmt.Println("mail:     " + mailStatusLine(c.Mailpit, mailpit.Owned(a.Paths) && mailpit.Running(), c.MailStopped, a.MailURL()))
	fmt.Printf("smtp:     127.0.0.1:%d (no TLS; any username/password)\n", mailpit.SMTPPort)
	fmt.Printf("inbox:    %s  (also http://127.0.0.1:%d)\n", a.MailURL(), mailpit.UIPort)
	php := "off (`mullion mail catch-php on`)"
	if c.MailCatchPHP {
		php = "caught"
	}
	fmt.Println("php mail():", php)
	if mailpit.Running() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if l, err := mailpit.Messages(ctx, 1); err == nil {
			fmt.Printf("messages: %d (%d unread)\n", l.Total, l.Unread)
		}
	}
	return nil
}

var mailOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open the inbox in the browser",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if a.State.Config.Mailpit == "" {
			return fmt.Errorf("Mail is not installed (run: mullion mail install)")
		}
		openURL(a.MailURL())
		return nil
	},
}

var mailClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Delete every caught message",
	RunE: func(cmd *cobra.Command, args []string) error {
		mustApp()
		if err := mailpit.DeleteAll(cmd.Context()); err != nil {
			return err
		}
		fmt.Println("Inbox emptied.")
		return nil
	},
}

var mailEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Print the Laravel .env lines that send mail to Mail",
	RunE: func(cmd *cobra.Command, args []string) error {
		mustApp()
		fmt.Print(app.MailEnvSnippet(mailpit.SMTPPort))
		fmt.Fprintln(os.Stderr, "# Laravel 11+ reads MAIL_SCHEME, older versions MAIL_ENCRYPTION — keep both.")
		return nil
	},
}

var mailCatchPHPCmd = &cobra.Command{
	Use:       "catch-php on|off",
	Short:     "Route PHP's mail() into Mail (sets sendmail_path in every PHP's php.ini)",
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"on", "off"},
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		on := strings.EqualFold(args[0], "on")
		if !on && !strings.EqualFold(args[0], "off") {
			return fmt.Errorf("say on or off")
		}
		if err := a.SetMailCatchPHP(on); err != nil {
			return err
		}
		if on {
			fmt.Println("PHP mail() now lands in Mail.")
		} else {
			fmt.Println("PHP mail() settings restored.")
		}
		return nil
	},
}

var (
	mailSendFrom, mailSendSubject, mailSendBody, mailSendHTML string
	mailSendTo, mailSendAttach                                []string
)

var mailSendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send a test email into the inbox",
	Example: `  mullion mail send --to you@example.test --subject Hi --body "Hello"
  mullion mail send --to you@example.test --html welcome.html --attach invoice.pdf`,
	RunE: func(cmd *cobra.Command, args []string) error {
		mustApp()
		if len(mailSendTo) == 0 {
			return fmt.Errorf("--to is required")
		}
		m := mailpit.TestMessage{From: mailSendFrom, To: mailSendTo, Subject: mailSendSubject, Text: mailSendBody}
		if mailSendHTML != "" {
			b, err := os.ReadFile(mailSendHTML)
			if err != nil {
				return err
			}
			m.HTML = string(b)
		}
		for _, f := range mailSendAttach {
			b, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			m.Attachments = append(m.Attachments, mailpit.Attachment{Name: filepath.Base(f), Data: b})
		}
		if err := mailpit.SendTest(m); err != nil {
			return err
		}
		fmt.Println("Sent — open the inbox with `mullion mail open`.")
		return nil
	},
}

func init() {
	mailUninstallCmd.Flags().BoolVar(&mailUninstallKeep, "keep-mail", false, "keep the caught messages")
	mailSendCmd.Flags().StringVar(&mailSendFrom, "from", mailpit.DefaultFrom, "sender address")
	mailSendCmd.Flags().StringSliceVar(&mailSendTo, "to", nil, "recipient address (repeat or comma-separate)")
	mailSendCmd.Flags().StringVar(&mailSendSubject, "subject", "Test email from Mullion", "subject line")
	mailSendCmd.Flags().StringVar(&mailSendBody, "body", "", "plain-text body")
	mailSendCmd.Flags().StringVar(&mailSendHTML, "html", "", "file with the HTML body")
	mailSendCmd.Flags().StringSliceVar(&mailSendAttach, "attach", nil, "file to attach (repeatable)")
	mailCmd.AddCommand(mailInstallCmd, mailUninstallCmd, mailStartCmd, mailStopCmd, mailRestartCmd,
		mailStatusCmd, mailOpenCmd, mailClearCmd, mailEnvCmd, mailCatchPHPCmd, mailSendCmd)
	rootCmd.AddCommand(mailCmd)
}
