package version

import "testing"

func TestModuleReleaseAcceptsOnlyPlainTaggedReleases(t *testing.T) {
	for in, want := range map[string]string{
		"v0.1.0":                               "0.1.0",
		"v1.20.3":                              "1.20.3",
		"v0.0.1":                               "0.0.1",
		"":                                     "",
		"(devel)":                              "",
		"0.1.0":                                "",
		"v0.1":                                 "",
		"v0.1.0-rc1":                           "",
		"v0.1.0+incompatible":                  "",
		"v0.0.0-20261016123456-abcdef123456":   "",
		"v0.1.1-0.20261016123456-abcdef123456": "",
		"v01.2.3":                              "",
		"v1.2.3 ":                              "",
	} {
		if got := ModuleRelease(in); got != want {
			t.Errorf("ModuleRelease(%q) = %q, want %q", in, got, want)
		}
	}
}

// Test binaries record "(devel)", so they stay on "dev" and nothing in the tests depends on the tag.
func TestATestBinaryStaysOnDev(t *testing.T) {
	if Release != "dev" {
		t.Errorf("Release = %q in a test binary", Release)
	}
}
