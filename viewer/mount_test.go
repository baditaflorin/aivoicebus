package viewer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A dashboard mounts the viewer under a path of its own, behind its own login: the page must work
// there, so everything it loads and asks for is relative to where it is served.
func TestTheViewerWorksMountedUnderAPathBehindAnotherLogin(t *testing.T) {
	v, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Demo(time.Now()); err != nil {
		t.Fatal(err)
	}
	loggedIn := func(r *http.Request) bool { return r.Header.Get("X-Test-Login") == "yes" }
	mux := http.NewServeMux()
	mux.Handle("/msgview/", http.StripPrefix("/msgview", v.Handler(HTTPOptions{Authorize: loggedIn})))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(path string, login bool) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if login {
			req.Header.Set("X-Test-Login", "yes")
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/msgview/api/sessions", false); code != 403 {
		t.Errorf("without the dashboard's login: %d", code)
	}
	code, page := get("/msgview/", true)
	if code != 200 || !strings.Contains(page, `src="viewer.js"`) {
		t.Fatalf("%d %s", code, page)
	}
	for _, p := range []string{"/msgview/viewer.js", "/msgview/viewer.css", "/msgview/api/status", "/msgview/api/sessions", "/msgview/api/conversations"} {
		if code, _ := get(p, true); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
}

func TestThePageUsesOnlyRelativeURLs(t *testing.T) {
	for _, f := range []string{"ui/index.html", "ui/viewer.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := regexp.MustCompile(`(src|href)="/|get\("/|fetch\("/`).Find(b); m != nil {
			t.Errorf("%s has an absolute URL (%s): it would break under a mount point", f, m)
		}
	}
}
