package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// The probes read the hardware; the real ones are in probe_darwin.go and
// probe_other.go, and tests replace them.
var (
	probeModel   = platformModel   // hw.model, "" when this is not a Mac
	probeChip    = platformChip    // machdep.cpu.brand_string
	probeProfile = platformProfile // marketing name and chip from system_profiler
)

// Label is the host part of every identity. It is the hostname, plus on a Mac
// the chip and product, so identically named machines stay apart:
//
//	m1pro-mbp  MacBook Pro, M1 Pro
//	laptop-c      MacBook Air, M1
//	laptop-b     Mac mini, M4
//
// AIVOICEBUS_HOST overrides it, which is how to separate two machines that
// still collide (a 14" and a 16" M1 Pro, say): AIVOICEBUS_HOST=laptop-a16.
func Label() string {
	if v := cleanLabel(os.Getenv("AIVOICEBUS_HOST")); v != "" {
		return v
	}
	if s := hardwareSuffix(); s != "" {
		return Host() + "-" + s
	}
	return Host()
}

// hardwareSuffix is "<chip>-<product>" for this Mac, or "" elsewhere. The
// system_profiler answer is cached per hardware model, so it is asked once.
func hardwareSuffix() string {
	model := probeModel()
	if model == "" {
		return ""
	}
	cache := ""
	if dir := config.Dir(); dir != "" {
		cache = filepath.Join(dir, "hostlabel.json")
	}
	if s := readCache(cache, model); s != "" {
		return s
	}
	if name, chip, ok := probeProfile(); ok {
		s := suffixFrom(name, chip, model)
		writeCache(cache, model, s)
		return s
	}
	// system_profiler unavailable: best effort from sysctl, and not cached, so
	// the accurate answer replaces it as soon as it can be had.
	return suffixFrom("", probeChip(), model)
}

type cacheFile struct {
	Model  string `json:"model"`
	Suffix string `json:"suffix"`
}

func readCache(path, model string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c cacheFile
	if json.Unmarshal(b, &c) != nil || c.Model != model {
		return ""
	}
	return c.Suffix
}

func writeCache(path, model, suffix string) {
	if path == "" || suffix == "" {
		return
	}
	b, _ := json.Marshal(cacheFile{Model: model, Suffix: suffix})
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = os.WriteFile(path, append(b, '\n'), 0o644)
	}
}

// suffixFrom builds "<chip>-<product>" from what Apple reports. model is the
// hw.model identifier, used for the product only when the marketing name is
// unavailable (new Macs report just "Laptop,2" there).
func suffixFrom(machineName, chip, model string) string {
	product := productSlug(machineName)
	if product == "" {
		product = legacyProduct(model)
	}
	if product == "" && model != "" {
		product = "mac"
	}
	parts := []string{}
	if c := chipSlug(chip); c != "" {
		parts = append(parts, c)
	}
	if product != "" {
		parts = append(parts, product)
	}
	return strings.Join(parts, "-")
}

// chipSlug: "Apple M1 Pro" -> m1pro, "Apple M4 Max" -> m4max, Intel -> intel.
func chipSlug(chip string) string {
	c := strings.ToLower(chip)
	if strings.Contains(c, "intel") {
		return "intel"
	}
	return alnum(strings.TrimPrefix(c, "apple "))
}

// productSlug turns the marketing name into a short code.
func productSlug(name string) string {
	switch n := strings.ToLower(strings.TrimSpace(name)); n {
	case "macbook air":
		return "mba"
	case "macbook pro":
		return "mbp"
	case "macbook":
		return "mb"
	case "mac mini":
		return "mini"
	case "mac studio":
		return "studio"
	case "mac pro":
		return "macpro"
	default:
		return alnum(n) // imac -> imac, "imac pro" -> imacpro
	}
}

// legacyProduct reads the product from an old-style model identifier
// (MacBookPro18,3). Identifiers like Laptop,2 carry no product.
func legacyProduct(model string) string {
	for _, p := range []struct{ prefix, code string }{
		{"MacBookAir", "mba"}, {"MacBookPro", "mbp"}, {"MacBook", "mb"},
		{"Macmini", "mini"}, {"iMac", "imac"}, {"MacPro", "macpro"},
	} {
		if strings.HasPrefix(model, p.prefix) {
			return p.code
		}
	}
	return ""
}

// alnum keeps only lowercase letters and digits.
func alnum(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanLabel makes an override safe inside an identity: lowercase, only
// letters, digits, dot, underscore and hyphen (no slash, no spaces).
func cleanLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
