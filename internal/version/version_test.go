package version

import "testing"

func TestString(t *testing.T) {
	t.Cleanup(func(version, commit, date string) func() {
		return func() { Version, Commit, Date = version, commit, date }
	}(Version, Commit, Date))

	if got, want := VersionString("agwtest"), "agwtest version unknown (commit unknown on unknown)"; got != want {
		t.Errorf("VersionString() with defaults = %q, want %q", got, want)
	}

	Version, Commit, Date = "1.2.3", "0123456789ab-dev", "2026-09-21T12:34:56Z"
	want := "agwtest version 1.2.3 (commit 0123456789ab-dev on 2026-09-21T12:34:56Z)"
	if got := VersionString("agwtest"); got != want {
		t.Errorf("VersionString() = %q, want %q", got, want)
	}
}
