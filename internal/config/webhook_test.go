package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearWebhookEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, p := range []string{"AIVOICEBUS_WEBHOOK_", "AIVOICEBUS_GROKBOT_WEBHOOK_"} {
		for _, s := range []string{"URL", "KEY", "HEADER", "PREFIX"} {
			t.Setenv(p+s, "")
			os.Unsetenv(p + s) // unset, as a shell without it has it (t.Setenv restores it)
		}
	}
}

func TestTheWebhookKeyGoesInAuthorizationAsBearerByDefault(t *testing.T) {
	clearWebhookEnv(t)
	t.Setenv("AIVOICEBUS_GROKBOT_WEBHOOK_URL", "https://hooks.example.com/x/1")
	t.Setenv("AIVOICEBUS_GROKBOT_WEBHOOK_KEY", "k")
	w, err := LoadWebhook()
	if err != nil || w.Header != "Authorization" || w.Prefix != "Bearer " || w.Key != "k" || w.Host() != "hooks.example.com" {
		t.Fatalf("%+v %v", w, err)
	}
	t.Setenv("AIVOICEBUS_WEBHOOK_HEADER", "X-Key")
	if w, _ = LoadWebhook(); w.Header != "X-Key" || w.Prefix != "" {
		t.Errorf("another header takes no prefix: %+v", w)
	}
	t.Setenv("AIVOICEBUS_WEBHOOK_PREFIX", "Token ")
	if w, _ = LoadWebhook(); w.Prefix != "Token " {
		t.Errorf("prefix %q", w.Prefix)
	}
}

func TestTheWebhookComesFromFilesWhenTheEnvironmentHasNone(t *testing.T) {
	clearWebhookEnv(t)
	if w, err := LoadWebhook(); err != nil || w.URL != "" {
		t.Fatalf("nothing set: %+v %v", w, err)
	}
	if err := writeTestFile(Dir()+"/"+WebhookURLFile, "https://h.example/p\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(Dir()+"/"+WebhookKeyFile, "filekey\n"); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWebhook()
	if err != nil || w.URL != "https://h.example/p" || w.Key != "filekey" {
		t.Fatalf("%+v %v", w, err)
	}
	if got := strings.Join(SecretValues(), "|"); !strings.Contains(got, "filekey") || !strings.Contains(got, "https://h.example/p") {
		t.Error("the webhook URL and key are not among the secrets a send refuses")
	}
}

func TestAnUnsafeWebhookIsRefusedWithoutQuotingIt(t *testing.T) {
	for _, u := range []string{"http://hooks.example.com/SECRET", "https://user:pw@hooks.example.com/SECRET", "ftp://x/SECRET", "SECRET"} {
		err := Webhook{URL: u, Header: "Authorization"}.Check()
		if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: %v", u, err)
		}
	}
	if err := (Webhook{URL: "http://127.0.0.1:9/x", Header: "Authorization"}).Check(); err != nil {
		t.Errorf("loopback http: %v", err)
	}
	if err := (Webhook{URL: "https://a.example/x", Header: "Bad Header"}).Check(); err == nil {
		t.Error("a header name with a space")
	}
	if err := (Webhook{URL: "https://a.example/x", Header: "Authorization", Key: "a\nb"}).Check(); err == nil {
		t.Error("a key with a line break")
	}
}

func writeTestFile(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o600)
}
