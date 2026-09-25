package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/term"
)

// renderDoctorCheck prints one DoctorCheck the same way `mullion doctor`
// always has: a plain informational line when Status is "" (e.g. the
// version banner), otherwise "<name>: <ok/PROBLEM> [detail]" followed by
// any extra block (a log tail, already fully formatted).
func renderDoctorCheck(c app.DoctorCheck) {
	if c.Status == "" {
		fmt.Println(c.Name)
	} else {
		marker := term.Red("PROBLEM")
		if c.Status == app.DoctorOK {
			marker = term.Green("ok")
		} else if c.Status == app.DoctorWarn {
			marker = term.Yellow("warning")
		}
		if c.Detail != "" {
			fmt.Printf("%s: %s %s\n", c.Name, marker, c.Detail)
		} else {
			fmt.Printf("%s: %s\n", c.Name, marker)
		}
	}
	if c.Fix != "" {
		fmt.Print(c.Fix)
	}
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose the whole stack — paste the output when reporting a problem",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		for _, c := range a.Doctor(cmd.Context()) {
			renderDoctorCheck(c)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
