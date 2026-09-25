package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/pkgsearch"
)

var pkgCmd = &cobra.Command{
	Use:   "pkg",
	Short: "Search, add, and remove Composer/npm packages for a linked site",
}

var pkgSearchCmd = &cobra.Command{
	Use:   "search <composer|npm> <query...>",
	Short: "Search Packagist or the npm registry for packages",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		manager := args[0]
		query := strings.Join(args[1:], " ")

		var results []pkgsearch.Package
		var err error
		switch manager {
		case "composer":
			results, err = pkgsearch.SearchComposer(cmd.Context(), query, 20)
		case "npm":
			results, err = pkgsearch.SearchNpm(cmd.Context(), query, 20)
		default:
			return fmt.Errorf("unknown package manager %q (want composer or npm)", manager)
		}
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Println("No packages found.")
			return nil
		}
		for _, p := range results {
			line := p.Name
			if p.Version != "" {
				line += " (" + p.Version + ")"
			}
			fmt.Println(line)
			if p.Description != "" {
				fmt.Println("  " + p.Description)
			}
			if p.Downloads > 0 {
				fmt.Printf("  downloads: %d\n", p.Downloads)
			}
		}
		return nil
	},
}

var pkgAddDev bool

var pkgAddCmd = &cobra.Command{
	Use:   "add <site> <composer|npm> <name>[@version]",
	Short: "Install a package into a linked site",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		site, manager, spec := args[0], args[1], args[2]
		name, version := splitNameVersion(spec)
		err := a.InstallPackage(cmd.Context(), site, manager, name, version, pkgAddDev, func(b []byte) {
			os.Stdout.Write(b)
		})
		if err != nil {
			return err
		}
		fmt.Printf("Installed %s into %s.\n", name, site)
		return nil
	},
}

var pkgRemoveCmd = &cobra.Command{
	Use:   "remove <site> <composer|npm> <name>",
	Short: "Remove a package from a linked site",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		site, manager, name := args[0], args[1], args[2]
		err := a.RemovePackage(cmd.Context(), site, manager, name, func(b []byte) {
			os.Stdout.Write(b)
		})
		if err != nil {
			return err
		}
		fmt.Printf("Removed %s from %s.\n", name, site)
		return nil
	},
}

var pkgListCmd = &cobra.Command{
	Use:   "list <site>",
	Short: "List a linked site's Composer and npm packages",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		composerPkgs, npmPkgs, err := a.InstalledPackages(args[0])
		if err != nil {
			return err
		}

		fmt.Println("Composer:")
		if len(composerPkgs) == 0 {
			fmt.Println("  (none)")
		}
		for _, p := range composerPkgs {
			printInstalledPkgLine(p.Name, p.Constraint, p.Installed, p.Dev)
		}
		fmt.Println("npm:")
		if len(npmPkgs) == 0 {
			fmt.Println("  (none)")
		}
		for _, p := range npmPkgs {
			printInstalledPkgLine(p.Name, p.Constraint, p.Installed, p.Dev)
		}
		return nil
	},
}

func printInstalledPkgLine(name, constraint, installed string, dev bool) {
	line := "  " + name + " " + constraint
	if installed != "" {
		line += " (installed " + installed + ")"
	}
	if dev {
		line += " [dev]"
	}
	fmt.Println(line)
}

// splitNameVersion splits a CLI package spec like "vendor/pkg@1.2.3" or
// "@scope/pkg@1.2.3" into name and version, respecting that a scoped
// npm package name itself starts with "@".
func splitNameVersion(spec string) (name, version string) {
	search := spec
	offset := 0
	if strings.HasPrefix(spec, "@") {
		search = spec[1:]
		offset = 1
	}
	idx := strings.LastIndex(search, "@")
	if idx < 0 {
		return spec, ""
	}
	cut := idx + offset
	return spec[:cut], spec[cut+1:]
}

func init() {
	pkgAddCmd.Flags().BoolVar(&pkgAddDev, "dev", false, "install as a dev/require-dev dependency")
	pkgCmd.AddCommand(pkgSearchCmd, pkgAddCmd, pkgRemoveCmd, pkgListCmd)
	rootCmd.AddCommand(pkgCmd)
}
