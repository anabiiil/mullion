// Package tty reports whether a file is genuinely connected to an
// interactive terminal.
//
// os.FileInfo.Mode()&os.ModeCharDevice is not a safe test for this: on
// Unix, /dev/null is also a character device, so a background process
// (the control panel, the tray, or the wake agent launched by launchd)
// whose stdin is redirected from /dev/null looks exactly like a human
// sitting at a terminal. That false positive sends elevation prompts
// down the "there's a terminal to type a password into" path (e.g.
// sudo) when there is none, and the command fails with something like
// "sudo: a terminal is required to read the password".
//
// IsTerminal instead asks the kernel for the file's terminal settings
// (termios on Unix, the console mode on Windows); only a real terminal
// answers that call successfully.
package tty
