//go:build windows

package postgres

// edbPlatform is the platform part of EDB's binary zip names.
const edbPlatform = "windows-x64"

func platformSupported() error { return nil }

// prepareBinaries has nothing to do on Windows (no exec bits, no
// quarantine attribute).
func prepareBinaries(dir string) {}

// runHint is appended to "does not run" install errors: postgres.exe
// (through ICU) needs vcruntime140.dll and msvcp140.dll, which EDB's
// zip does not ship.
const runHint = " — the Microsoft Visual C++ 2015-2022 runtime may be missing (mullion setup installs it)"
