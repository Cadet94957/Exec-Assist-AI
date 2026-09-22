package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestZoneFromPath(t *testing.T) {
	cases := []struct{ path, want string }{
		// Where macOS points /etc/localtime.
		{"/var/db/timezone/zoneinfo/America/Los_Angeles", "America/Los_Angeles"},
		// Where most Linux distributions point it.
		{"/usr/share/zoneinfo/America/Chicago", "America/Chicago"},
		// A relative link, as some systems write it.
		{"../usr/share/zoneinfo/Europe/London", "Europe/London"},
		// Three-part names must survive whole.
		{"/usr/share/zoneinfo/America/Indiana/Indianapolis", "America/Indiana/Indianapolis"},
		// Parallel trees carry the same names; the tree is not part of it.
		{"/usr/share/zoneinfo/posix/America/Denver", "America/Denver"},
		{"/usr/share/zoneinfo/right/America/Denver", "America/Denver"},
		// What this project's CI container reports.
		{"/usr/share/zoneinfo/Etc/UTC", "Etc/UTC"},
		// Not a zoneinfo path at all.
		{"/etc/localtime", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := zoneFromPath(c.path); got != c.want {
			t.Errorf("zoneFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestWindowsZone(t *testing.T) {
	cases := []struct{ tzutil, want string }{
		// tzutil ends its output with a newline.
		{"Pacific Standard Time\r\n", "America/Los_Angeles"},
		{"Central Standard Time", "America/Chicago"},
		// Arizona does not observe daylight saving, and Windows says so with
		// a separate zone rather than the Mountain one.
		{"US Mountain Standard Time", "America/Phoenix"},
		// Reported when "adjust for daylight saving time" is switched off.
		{"Eastern Standard Time_dstoff", "America/New_York"},
		// Unknown zones fall through to the loud UTC fallback, never a guess.
		{"Central Europe Standard Time", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := windowsZone(c.tzutil); got != c.want {
			t.Errorf("windowsZone(%q) = %q, want %q", c.tzutil, got, c.want)
		}
	}
}

// Every name the Windows table can produce has to be one Go can load, or the
// table would hand account creation a zone it then rejects. This runs against
// the embedded time/tzdata, which is also what a Windows machine would use.
func TestWindowsZonesAllLoad(t *testing.T) {
	for win, iana := range windowsZones {
		if _, err := time.LoadLocation(iana); err != nil {
			t.Errorf("%q maps to %q, which does not load: %v", win, iana, err)
		}
	}
}

// Whatever detection returns, on whichever machine runs this, must be a name
// Go can load — otherwise quickstart would create an account that then fails.
func TestDetectZoneReturnsALoadableName(t *testing.T) {
	name, ok := detectZone()
	if !ok {
		t.Skip("this machine's timezone could not be determined; the fallback path covers it")
	}
	if _, err := time.LoadLocation(name); err != nil {
		t.Fatalf("detectZone returned %q, which does not load: %v", name, err)
	}
}

// An explicit TZ outranks everything else, as it does for every other program.
func TestDetectZoneHonoursTZ(t *testing.T) {
	for _, tz := range []string{"America/Denver", ":America/Denver", "/usr/share/zoneinfo/America/Denver"} {
		t.Setenv("TZ", tz)
		if got, ok := detectZone(); !ok || got != "America/Denver" {
			t.Errorf("TZ=%q: detectZone() = %q, %v; want America/Denver", tz, got, ok)
		}
	}
}

// The bug this file exists to fix: a machine set to Los Angeles, whose
// /etc/localtime points into the zoneinfo tree, must be read as Los Angeles —
// not as UTC, which is what asking time.Local produced on every such machine.
func TestDetectZoneReadsLocaltimeLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reads its zone from tzutil, not /etc/localtime")
	}
	t.Setenv("TZ", "") // empty is ignored, so the link is what gets read

	for _, target := range []string{
		"/var/db/timezone/zoneinfo/America/Los_Angeles", // macOS
		"/usr/share/zoneinfo/America/Los_Angeles",       // Linux
	} {
		dir := t.TempDir()
		link := filepath.Join(dir, "localtime")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		setZonePaths(t, link, filepath.Join(dir, "no-such-file"))

		got, source := localZone("")
		if got != "America/Los_Angeles" || source != zoneDetected {
			t.Errorf("link to %s: localZone() = %q (source %d), want America/Los_Angeles detected",
				target, got, source)
		}
	}
}

// When /etc/localtime is a copied file rather than a link, the Debian-style
// text file is the fallback.
func TestDetectZoneReadsTimezoneFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reads its zone from tzutil")
	}
	t.Setenv("TZ", "")
	dir := t.TempDir()
	file := filepath.Join(dir, "timezone")
	if err := os.WriteFile(file, []byte("America/Chicago\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setZonePaths(t, filepath.Join(dir, "not-a-link"), file)

	if got, source := localZone(""); got != "America/Chicago" || source != zoneDetected {
		t.Errorf("localZone() = %q (source %d), want America/Chicago detected", got, source)
	}
}

// With nothing to go on, the answer is UTC — and flagged as a fallback so the
// person running it is told loudly, rather than shown a confident wrong zone.
func TestLocalZoneFallsBackLoudly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reads its zone from tzutil")
	}
	t.Setenv("TZ", "")
	dir := t.TempDir()
	setZonePaths(t, filepath.Join(dir, "nothing"), filepath.Join(dir, "nothing-either"))

	if got, source := localZone(""); got != "UTC" || source != zoneFallback {
		t.Errorf("localZone() = %q (source %d), want UTC as a fallback", got, source)
	}
}

func setZonePaths(t *testing.T, localtime, timezoneFile string) {
	t.Helper()
	oldL, oldT := localtimePath, timezoneFilePath
	localtimePath, timezoneFilePath = localtime, timezoneFile
	t.Cleanup(func() { localtimePath, timezoneFilePath = oldL, oldT })
}
