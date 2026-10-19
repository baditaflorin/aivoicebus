package config

import (
	"os"
	"path/filepath"
	"strings"
)

// NotifyFile is ~/.config/aivoicebus/notify-url (mode 0600: the URL may be a secret), where
// `aivoicebus watch notify set` keeps the person's phone channel.
const NotifyFile = "notify-url"

// NotifyURL is where alerts for this machine's person are posted: AIVOICEBUS_NOTIFY_URL, else the
// notify-url file; "" when neither is set.
func NotifyURL() string {
	if v := strings.TrimSpace(os.Getenv("AIVOICEBUS_NOTIFY_URL")); v != "" {
		return v
	}
	if b, err := os.ReadFile(filepath.Join(Dir(), NotifyFile)); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}
