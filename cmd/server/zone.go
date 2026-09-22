package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	// Embeds the IANA timezone database in the binary (~450KB).
	//
	// Windows ships no IANA database, and Go only finds one there if Go itself
	// is installed. Without this, on a contractor's Windows laptop every
	// time.LoadLocation("America/Los_Angeles") fails: account creation rejects
	// any zone but UTC, and Tenant.Location quietly falls back to UTC. macOS
	// and Linux have /usr/share/zoneinfo and production installs tzdata, which
	// is why this never showed up anywhere it was tested.
	_ "time/tzdata"
)

// detectZone reports this computer's IANA timezone, or ok=false if it cannot
// work one out.
//
// Go cannot answer this directly. On a normally configured Mac, Linux or
// Windows machine time.Local is named "Local" rather than
// "America/Los_Angeles", so asking it looks like "no answer" everywhere — an
// earlier version of this code did exactly that and created every account in
// UTC, which puts "today" and the 6am brief hours out for anyone in the US.
// The real name has to be read from where each operating system keeps it.
func detectZone() (name string, ok bool) {
	for _, candidate := range zoneCandidates() {
		if candidate == "" {
			continue
		}
		if _, err := time.LoadLocation(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// zoneCandidates lists guesses in order of how much to trust them.
func zoneCandidates() []string {
	var out []string

	// An explicit TZ wins, as it does for every other program. It may be a
	// bare name, ":Name", or a path into a zoneinfo directory.
	if tz, set := os.LookupEnv("TZ"); set && tz != "" {
		tz = strings.TrimPrefix(tz, ":")
		if z := zoneFromPath(tz); z != "" {
			out = append(out, z)
		} else {
			out = append(out, tz)
		}
	}

	if runtime.GOOS == "windows" {
		if b, err := exec.Command("tzutil", "/g").Output(); err == nil {
			out = append(out, windowsZone(string(b)))
		}
		return out
	}

	// macOS and most Linux: /etc/localtime is a symlink into the zoneinfo
	// tree, and the part of its target after "zoneinfo/" is the IANA name.
	if target, err := os.Readlink(localtimePath); err == nil {
		out = append(out, zoneFromPath(target))
	}
	// Debian-family systems also record the name as plain text, which covers
	// the case where /etc/localtime is a copied file rather than a link.
	if b, err := os.ReadFile(timezoneFilePath); err == nil {
		out = append(out, strings.TrimSpace(string(b)))
	}
	return out
}

// Where Unix systems record their timezone. Variables only so the tests can
// point them at a symlink of their own: the case that matters — a machine set
// to somewhere other than UTC — cannot be reproduced any other way on a CI
// machine that is itself set to UTC.
var (
	localtimePath    = "/etc/localtime"
	timezoneFilePath = "/etc/timezone"
)

// zoneFromPath extracts the IANA name from a path into a zoneinfo directory,
// such as /var/db/timezone/zoneinfo/America/Los_Angeles on macOS or
// /usr/share/zoneinfo/America/Los_Angeles on Linux. It returns "" for a path
// that does not look like one.
func zoneFromPath(p string) string {
	const marker = "zoneinfo/"
	i := strings.LastIndex(p, marker)
	if i < 0 {
		return ""
	}
	name := p[i+len(marker):]
	// Some distributions keep parallel trees ("posix/", "right/") with the
	// same names inside; the name is what matters, not the tree.
	for _, prefix := range []string{"posix/", "right/"} {
		name = strings.TrimPrefix(name, prefix)
	}
	return name
}

// windowsZone translates the name `tzutil /g` prints into an IANA name, or ""
// for a zone not in the table.
//
// Windows uses its own names ("Pacific Standard Time") rather than IANA ones.
// The mappings are the territory-neutral entries from Unicode CLDR's
// windowsZones table, limited to where this product's customers are: every
// North American zone, the UK and Ireland, and Australia and New Zealand.
// Anything else falls back to UTC with a visible warning and a -tz flag,
// rather than a guess that is quietly hours out.
func windowsZone(tzutil string) string {
	name := strings.TrimSpace(tzutil)
	// Reported when "adjust for daylight saving time" is switched off.
	name = strings.TrimSuffix(name, "_dstoff")
	return windowsZones[name]
}

var windowsZones = map[string]string{
	"Eastern Standard Time":        "America/New_York",
	"US Eastern Standard Time":     "America/Indiana/Indianapolis",
	"Central Standard Time":        "America/Chicago",
	"Canada Central Standard Time": "America/Regina",
	"Mountain Standard Time":       "America/Denver",
	"US Mountain Standard Time":    "America/Phoenix",
	"Pacific Standard Time":        "America/Los_Angeles",
	"Alaskan Standard Time":        "America/Anchorage",
	"Hawaiian Standard Time":       "Pacific/Honolulu",
	"Atlantic Standard Time":       "America/Halifax",
	"Newfoundland Standard Time":   "America/St_Johns",
	"GMT Standard Time":            "Europe/London",
	"AUS Eastern Standard Time":    "Australia/Sydney",
	"E. Australia Standard Time":   "Australia/Brisbane",
	"Cen. Australia Standard Time": "Australia/Adelaide",
	"AUS Central Standard Time":    "Australia/Darwin",
	"W. Australia Standard Time":   "Australia/Perth",
	"Tasmania Standard Time":       "Australia/Hobart",
	"New Zealand Standard Time":    "Pacific/Auckland",
	"UTC":                          "UTC",
}
