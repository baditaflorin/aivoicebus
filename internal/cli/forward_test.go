package cli

import (
	"strings"
	"testing"
)

func noWebhook(t *testing.T) {
	t.Helper()
	for _, p := range []string{"AIVOICEBUS_WEBHOOK_", "AIVOICEBUS_GROKBOT_WEBHOOK_"} {
		for _, s := range []string{"URL", "KEY", "HEADER", "PREFIX"} {
			t.Setenv(p+s, "")
		}
	}
}

func TestForwardNeedsAWebhookAndSaysSoWithoutAnyValue(t *testing.T) {
	mesh(t)
	noWebhook(t)
	code, _, e := run(t, "", "forward", "--webhook")
	if code == 0 || !strings.Contains(e, "no webhook") || !strings.Contains(e, "Authorization") {
		t.Errorf("code=%d err=%q", code, e)
	}
	if code, _, _ := run(t, "", "forward"); code != 2 {
		t.Errorf("without --webhook: code=%d", code)
	}
	// ensure is for a shell rc file: silent and successful when there is nothing to start
	if code, out, e := run(t, "", "forward", "ensure"); code != 0 || out != "" || e != "" {
		t.Errorf("ensure: code=%d out=%q err=%q", code, out, e)
	}
	if _, out, _ := run(t, "", "forward", "ensure", "--verbose"); !strings.Contains(out, "no webhook is set") {
		t.Errorf("ensure --verbose: %q", out)
	}
	if code, out, _ := run(t, "", "forward", "status"); code != 0 || !strings.Contains(out, "not set") || !strings.Contains(out, "not running") {
		t.Errorf("status: %q", out)
	}
}

func TestForwardCheckNamesTheIdentityAndHostButNeverTheURLOrKey(t *testing.T) {
	mesh(t)
	noWebhook(t)
	t.Setenv("AIVOICEBUS_GROKBOT_WEBHOOK_URL", "https://hooks.example.com/automations/webhook/SECRET-ID")
	t.Setenv("AIVOICEBUS_GROKBOT_WEBHOOK_KEY", "KEY-VALUE-1234")
	t.Setenv("AIVOICEBUS_ME", "")
	code, out, e := run(t, "", "forward", "--webhook", "--check")
	if code != 0 || !strings.Contains(out, "/grokbot") || !strings.Contains(out, "hooks.example.com") {
		t.Fatalf("code=%d out=%q err=%q", code, out, e)
	}
	for _, s := range []string{out, e} {
		if strings.Contains(s, "SECRET-ID") || strings.Contains(s, "KEY-VALUE-1234") {
			t.Errorf("leaked: %q", s)
		}
	}
	_, st, _ := run(t, "", "forward", "status")
	if strings.Contains(st, "SECRET-ID") || strings.Contains(st, "KEY-VALUE-1234") || !strings.Contains(st, "hooks.example.com") {
		t.Errorf("status: %q", st)
	}
	t.Setenv("AIVOICEBUS_GROKBOT_WEBHOOK_URL", "http://hooks.example.com/SECRET-ID")
	if code, _, e := run(t, "", "forward", "--webhook", "--check"); code == 0 || strings.Contains(e, "SECRET-ID") || !strings.Contains(e, "plain http") {
		t.Errorf("plain http: code=%d err=%q", code, e)
	}
}
