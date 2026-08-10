package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/caddy"
)

var secureCmd = &cobra.Command{
	Use:   "secure [name]",
	Short: "Serve a site over HTTPS with a locally-trusted certificate",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		name, err := siteNameFromArgOrCwd(a, args)
		if err != nil {
			return err
		}
		site := a.State.FindSite(name)
		if site == nil {
			return fmt.Errorf("no site named %q", name)
		}
		if site.Secure {
			fmt.Printf("https://%s is already secured\n", a.State.Host(*site))
			return nil
		}
		site.Secure = true
		if err := a.Apply(); err != nil {
			return err
		}
		// Make sure Caddy's local root CA is in the system trust store so
		// browsers show the padlock (one-time confirmation prompt).
		if err := caddy.TrustCA(a.Paths); err != nil {
			fmt.Println("note:", err)
			fmt.Println("If the browser warns about the certificate, run `mullion start` again as administrator once.")
		}
		fmt.Printf("Secured: https://%s\n", a.State.Host(*site))
		return nil
	},
}

var unsecureCmd = &cobra.Command{
	Use:   "unsecure [name]",
	Short: "Go back to plain HTTP for a site",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		name, err := siteNameFromArgOrCwd(a, args)
		if err != nil {
			return err
		}
		site := a.State.FindSite(name)
		if site == nil {
			return fmt.Errorf("no site named %q", name)
		}
		site.Secure = false
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("Now serving http://%s\n", a.State.Host(*site))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(secureCmd, unsecureCmd)
}
