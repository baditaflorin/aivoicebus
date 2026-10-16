package viewer

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

//go:embed ui/index.html ui/viewer.js ui/viewer.css
var ui embed.FS

// HTTPOptions guard the handler.
type HTTPOptions struct {
	// Key is the secret in the URL `aivoicebus view` prints. The first visit with ?key= sets a
	// cookie and every later request needs it, so another user of the same machine, or a web page
	// that makes the browser call 127.0.0.1, gets nothing. Empty: the caller authorises requests
	// itself (Authorize).
	Key string
	// Hosts are the Host headers accepted ("127.0.0.1:7788", "localhost:7788"): a page on another
	// site that points its own name at 127.0.0.1 (DNS rebinding) is refused. Empty: any.
	Hosts []string
	// Authorize, when set, is asked about every request (a dashboard's own login).
	Authorize func(*http.Request) bool
}

const cookieName = "aivoicebus_view"

// threadRef is what a message or thread id looks like: no path characters, no spaces.
var threadRef = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Handler serves the page and its read-only JSON API.
func (v *Viewer) Handler(o HTTPOptions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveFile(w, "ui/index.html", "text/html; charset=utf-8")
	})
	mux.HandleFunc("/viewer.js", func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, "ui/viewer.js", "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("/viewer.css", func(w http.ResponseWriter, r *http.Request) { serveFile(w, "ui/viewer.css", "text/css; charset=utf-8") })
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		roster, rev, last, lastErr := v.snapshot()
		writeJSON(w, map[string]any{
			"title": v.title, "rev": rev, "demo": v.c == nil, "sessions": len(roster), "messages": len(v.st.Messages()),
			"last_pull": nonZero(last), "error": view.OneLine(lastErr, 300),
		})
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		roster, _, _, _ := v.snapshot()
		writeJSON(w, Sessions(roster, v.st.Messages()))
	})
	mux.HandleFunc("/api/conversations", func(w http.ResponseWriter, r *http.Request) {
		roster, _, _, _ := v.snapshot()
		session := r.URL.Query().Get("session")
		if len(session) > msg.MaxIdentity {
			http.Error(w, "session is too long", http.StatusBadRequest)
			return
		}
		writeJSON(w, Conversations(v.st.Messages(), titles(roster), session))
	})
	mux.HandleFunc("/api/thread", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !threadRef.MatchString(id) {
			http.Error(w, "id must be a message or thread id", http.StatusBadRequest)
			return
		}
		roster, _, _, _ := v.snapshot()
		threads := []string{id}
		for _, c := range Conversations(v.st.Messages(), titles(roster), "") {
			if c.Thread == id || slices.Contains(c.Threads, id) {
				if len(c.Threads) > 0 {
					threads = c.Threads
				}
				break
			}
		}
		var out []ThreadMessage
		for _, t := range threads {
			for _, m := range view.SanitiseMessages(v.st.Thread(t)) {
				out = append(out, ThreadMessage{Message: m, ReadBy: v.st.Readers(m.ID)})
			}
		}
		writeJSON(w, Collapse(out))
	})
	return guard(o, mux)
}

func nonZero(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func serveFile(w http.ResponseWriter, name, ctype string) {
	b, err := ui.ReadFile(name)
	if err != nil {
		http.Error(w, "missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Write(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// guard is everything a request passes before the mux: read-only methods, a known Host, the key
// (or the dashboard's own Authorize), and headers that keep the page in its own origin.
func guard(o HTTPOptions, next http.Handler) http.Handler {
	hosts := map[string]bool{}
	for _, h := range o.Hosts {
		hosts[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		if len(hosts) > 0 && !hosts[strings.ToLower(r.Host)] {
			http.Error(w, "unknown host", http.StatusForbidden)
			return
		}
		if o.Authorize != nil && !o.Authorize(r) {
			http.Error(w, "not authorised", http.StatusForbidden)
			return
		}
		if o.Key != "" {
			if k := r.URL.Query().Get("key"); k != "" && r.URL.Path == "/" {
				if subtle.ConstantTimeCompare([]byte(k), []byte(o.Key)) != 1 {
					http.Error(w, "wrong key: open the URL `aivoicebus view` printed", http.StatusForbidden)
					return
				}
				http.SetCookie(w, &http.Cookie{Name: cookieName, Value: o.Key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/", http.StatusSeeOther) // the key leaves the address bar and the history
				return
			}
			c, err := r.Cookie(cookieName)
			if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(o.Key)) != 1 {
				http.Error(w, "open the URL `aivoicebus view` printed (it carries a one-time key)", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// LoopbackHosts are the Host headers a browser sends for a viewer listening on addr (a loopback address).
func LoopbackHosts(addr string) []string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	return []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port}
}
