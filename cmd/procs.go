package cmd

import "pm/internal/sysproc"

// Thin aliases over the shared per-OS process plumbing.

type portConflict = sysproc.Conflict

func portOwner(port int) (int, string)        { return sysproc.PortOwner(port) }
func killWithParent(pid int)                  { sysproc.KillWithParent(pid) }
func processesUnder(dir, image string) []int  { return sysproc.ProcessesUnder(dir, image) }
func killProcess(pid int)                     { sysproc.KillProcess(pid) }
func openURL(url string)                      { sysproc.OpenURL(url) }
func scheduleHomeRemoval(home string) error   { return sysproc.ScheduleHomeRemoval(home) }
func stopConflict(c portConflict)             { sysproc.StopConflict(c) }
func printStackHint(conflicts []portConflict) { sysproc.PrintStackHint(conflicts) }

const conflictExample = sysproc.ConflictExample
