package compress_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	compress "github.com/Elagoht/collage-compress"
	"github.com/Elagoht/collage/pkg/collage"
)

// privateSite serves /api/x with the Cache-Control lines the request's "cc"
// query parameters name, one header line each.
func privateSite(t *testing.T, opts compress.Options) *collage.App {
	t.Helper()
	return site(t, opts, func(app *collage.App) {
		if err := app.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			for _, line := range r.URL.Query()["cc"] {
				w.Header().Add("Cache-Control", line)
			}
			_, _ = w.Write([]byte(`<p>csrf=SECRET</p>` + strings.Repeat("<p>padding</p>", 200)))
		})); err != nil {
			t.Fatal(err)
		}
	})
}

// With skipPrivate, a response marked private or no-store — the kind that carries
// a CSRF token or other per-reader secret — is sent uncompressed, so BREACH has
// no compressed length to measure.
func TestSkipPrivate(t *testing.T) {
	for _, c := range []struct {
		name       string
		lines      []string
		compressed bool
	}{
		{"private", []string{"private"}, false},
		{"no-store", []string{"no-store"}, false},
		{"mixed case", []string{"Private"}, false},
		{"upper case", []string{"NO-STORE"}, false},
		{"among others", []string{"max-age=0, private, must-revalidate"}, false},
		{"second line", []string{"public, max-age=60", "no-store"}, false},
		{"public", []string{"public, max-age=60"}, true},
		{"lookalike", []string{"x-private, no-storex"}, true},
		{"none", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := privateSite(t, compress.Options{SkipPrivate: true})
			path := "/api/x"
			for i, line := range c.lines {
				if i == 0 {
					path += "?"
				} else {
					path += "&"
				}
				path += "cc=" + strings.ReplaceAll(strings.ReplaceAll(line, " ", "+"), ",", "%2C")
			}
			rec := get(app, path, "Accept-Encoding", "gzip, br")
			got := rec.Header().Get("Content-Encoding") != ""
			if got != c.compressed {
				t.Errorf("Cache-Control %q: compressed = %v, want %v", c.lines, got, c.compressed)
			}
			if !c.compressed && !strings.Contains(rec.Body.String(), "csrf=SECRET") {
				t.Errorf("Cache-Control %q: the body was not passed through: %.40q", c.lines, rec.Body.String())
			}
		})
	}
}

// Without skipPrivate, the default, a private response is compressed as before.
func TestSkipPrivateIsOffByDefault(t *testing.T) {
	app := privateSite(t, compress.Options{})
	if rec := get(app, "/api/x?cc=private", "Accept-Encoding", "gzip"); rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("a private response was not compressed by default: %v", rec.Header())
	}
}

// skipPrivate is read from the application's configuration.
func TestSkipPrivateFromConfiguration(t *testing.T) {
	app, err := collage.New(&collage.Config{
		Server:       collage.ServerConfig{Host: "localhost", Port: 3000},
		Template:     collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p></p>`)}}, Root: "t"},
		Plugins:      []collage.Plugin{compress.New(compress.Options{})},
		PluginConfig: map[string]json.RawMessage{"elagoht/compress": json.RawMessage(`{"skipPrivate": true}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "private")
		_, _ = w.Write([]byte(strings.Repeat("<p>padding</p>", 200)))
	})); err != nil {
		t.Fatal(err)
	}
	if rec := get(app, "/api/x", "Accept-Encoding", "gzip"); rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("skipPrivate from configuration was not applied: %v", rec.Header())
	}
}
