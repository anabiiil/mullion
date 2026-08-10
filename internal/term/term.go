// Package term adds ANSI color to CLI output — enabled only when stdout
// is a real terminal that supports it, so piped output stays plain.
package term

var enabled bool

// Init detects terminal support (and on Windows switches the console
// into VT mode). Call once at program start.
func Init() { enabled = initPlatform() }

func wrap(code, s string) string {
	if !enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func Bold(s string) string   { return wrap("1", s) }
func Dim(s string) string    { return wrap("2", s) }
func Red(s string) string    { return wrap("31", s) }
func Green(s string) string  { return wrap("32", s) }
func Yellow(s string) string { return wrap("33", s) }
func Cyan(s string) string   { return wrap("36", s) }

// ClearLine erases from the cursor to the end of the line — for
// progress bars that redraw in place.
func ClearLine() string {
	if !enabled {
		return "   "
	}
	return "\x1b[K"
}
