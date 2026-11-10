package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The webhook `aivoicebus forward --webhook` posts new messages to (a Grok Bot routine's, or any
// service that wakes an agent when it is POSTed to). The URL is a capability (its path usually holds
// an id) and the key is a credential, so both are kept like the token: a file of mode 0600 in
// Dir(), or the environment, never a command-line argument, and neither is ever printed.
const (
	WebhookURLFile    = "webhook-url"
	WebhookKeyFile    = "webhook-key"
	DefaultWebhookHdr = "Authorization"
)

// Webhook is where the forwarder posts, and how it proves who it is.
type Webhook struct {
	URL    string // https://host/path (http only to this machine)
	Key    string // the sender key; "" when the endpoint needs none
	Header string // the request header the key goes in
	Prefix string // put before the key in that header ("Bearer " for Authorization)
}

// webhookEnv is the first of these names that is set and not empty: the generic one, then the Grok Bot one.
func webhookEnv(suffix string) (string, bool) {
	for _, p := range []string{"AIVOICEBUS_WEBHOOK_", "AIVOICEBUS_GROKBOT_WEBHOOK_"} {
		if v := strings.TrimSpace(os.Getenv(p + suffix)); v != "" {
			return v, true
		}
	}
	return "", false
}

func webhookValue(suffix, file string) string {
	if v, ok := webhookEnv(suffix); ok && v != "" {
		return v
	}
	if d := Dir(); d != "" {
		if lines := SecretFileLines(filepath.Join(d, file)); len(lines) > 0 {
			return lines[0]
		}
	}
	return ""
}

// LoadWebhook reads the webhook from AIVOICEBUS_WEBHOOK_URL / _KEY / _HEADER / _PREFIX (or the same
// with AIVOICEBUS_GROKBOT_WEBHOOK_), else from the files webhook-url and webhook-key in Dir(). With
// no URL it returns the zero Webhook and no error (nothing is configured). The header is
// Authorization unless _HEADER names another; the prefix is "Bearer " for Authorization, nothing for
// another header, unless _PREFIX is set (an empty _PREFIX means none).
func LoadWebhook() (Webhook, error) {
	w := Webhook{URL: webhookValue("URL", WebhookURLFile)}
	if w.URL == "" {
		return Webhook{}, nil
	}
	w.Key = webhookValue("KEY", WebhookKeyFile)
	w.Header = DefaultWebhookHdr
	if v, ok := webhookEnv("HEADER"); ok && v != "" {
		w.Header = v
	}
	if strings.EqualFold(w.Header, "authorization") {
		w.Prefix = "Bearer "
	}
	if v, ok := os.LookupEnv("AIVOICEBUS_WEBHOOK_PREFIX"); ok {
		w.Prefix = v
	} else if v, ok := os.LookupEnv("AIVOICEBUS_GROKBOT_WEBHOOK_PREFIX"); ok {
		w.Prefix = v
	}
	return w, w.Check()
}

// Check says why w cannot be used, without quoting the URL or the key.
func (w Webhook) Check() error {
	u, err := url.Parse(w.URL)
	switch {
	case err != nil || u.Host == "":
		return errors.New("the webhook URL is not an absolute http(s) URL")
	case u.User != nil:
		return errors.New("the webhook URL holds a user name or password: put the key in the key file or AIVOICEBUS_WEBHOOK_KEY")
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback(u.Hostname()):
	case u.Scheme == "http":
		return fmt.Errorf("the webhook URL (host %s) is plain http: the key would cross the network in the clear; use https", u.Hostname())
	default:
		return errors.New("the webhook URL is not an http(s) URL")
	}
	if !headerToken(w.Header) {
		return fmt.Errorf("%q is not a usable header name", w.Header)
	}
	if strings.ContainsAny(w.Key+w.Prefix, "\r\n\x00") {
		return errors.New("the webhook key or prefix holds a line break")
	}
	return nil
}

// Host is the webhook's host name, the one part of it that may be printed.
func (w Webhook) Host() string {
	if u, err := url.Parse(w.URL); err == nil {
		return u.Host
	}
	return ""
}

// Secrets are the values of w that must never leave this machine in a message or a log.
func (w Webhook) Secrets() []string {
	var out []string
	for _, v := range []string{w.URL, w.Key} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func loopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// headerToken reports whether s is an RFC 9110 field name (token characters only).
func headerToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)
		if !ok {
			return false
		}
	}
	return true
}
