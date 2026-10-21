package app

import (
	"fmt"

	"pm/internal/config"
)

// checkIdleMinutes validates an idle limit in minutes (0 = default).
func checkIdleMinutes(m int) error {
	if m < 0 || m > config.MaxDevIdleMin {
		return fmt.Errorf("idle limit %d min is out of range — use 1-%d minutes", m, config.MaxDevIdleMin)
	}
	return nil
}

// SetDevIdle sets the global idle-sleep limits of frontend dev servers
// (minutes; 0 = default) and the "never sleep" switch. The agent reads
// them on its next check — nothing restarts.
func (a *App) SetDevIdle(noTabMin, withTabMin int, never bool) error {
	for _, m := range []int{noTabMin, withTabMin} {
		if err := checkIdleMinutes(m); err != nil {
			return err
		}
	}
	c := &a.State.Config
	c.DevIdleNoTabMin, c.DevIdleWithTabMin, c.DevNeverSleep = noTabMin, withTabMin, never
	return a.State.Save()
}

// SetSiteDevIdle overrides one site's idle-sleep: mode "global" (follow
// Settings), "never", or "custom" with its own minutes (0 = the global
// value).
func (a *App) SetSiteDevIdle(name, mode string, noTabMin, withTabMin int) error {
	s := a.State.FindSite(name)
	if s == nil {
		return fmt.Errorf("no site named %q", name)
	}
	if s.Kind != "node" {
		return fmt.Errorf("%s is not a frontend site — only dev servers sleep", s.Name)
	}
	switch mode {
	case "global", "":
		s.DevIdleNever, s.DevIdleNoTabMin, s.DevIdleWithTabMin = false, 0, 0
	case "never":
		s.DevIdleNever, s.DevIdleNoTabMin, s.DevIdleWithTabMin = true, 0, 0
	case "custom":
		for _, m := range []int{noTabMin, withTabMin} {
			if err := checkIdleMinutes(m); err != nil {
				return err
			}
		}
		if noTabMin == 0 && withTabMin == 0 {
			return fmt.Errorf("give at least one custom limit (or use the global setting)")
		}
		s.DevIdleNever, s.DevIdleNoTabMin, s.DevIdleWithTabMin = false, noTabMin, withTabMin
	default:
		return fmt.Errorf("unknown idle mode %q (use global, never or custom)", mode)
	}
	return a.State.Save()
}
