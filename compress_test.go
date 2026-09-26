package compress_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	compress "github.com/Elagoht/collage-compress"
	"github.com/Elagoht/collage/pkg/collage"
	"github.com/andybalholm/brotli"
)

// big is a page body comfortably above the default MinSize.
var big = "<html><body>" + strings.Repeat("<p>collage compresses this paragraph.</p>", 200) + "</body></html>"

func site(t *testing.T, opts compress.Options, extra func(app *collage.App)) *collage.App {
	t.Helper()
	app, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/big.html":   {Data: []byte(big)},
			"t/small.html": {Data: []byte(`<p>small</p>`)},
		}, Root: "t"},
		Cache:   collage.CacheConfig{Enabled: true, Type: "memory", DefaultTTL: time.Hour},
		Plugins: []collage.Plugin{compress.New(opts)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*collage.Page{
		collage.NewPage("big").WithContent(collage.NewFragment("big", "big.html").Build()).WithPath("en", "/").Static().Build(),
		collage.NewPage("small").WithContent(collage.NewFragment("small", "small.html").Build()).WithPath("en", "/small").Static().Build(),
	} {
		if err := app.RegisterPage(p); err != nil {
			t.Fatal(err)
		}
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 4096)...)
	if err := app.RegisterDocument(collage.NewDocument("logo", "image/png").AtRoot("/logo.png").WithBody(png).Build()); err != nil {
		t.Fatal(err)
	}
	if err := app.Mount("/static/", fstest.MapFS{"app.css": {Data: []byte(strings.Repeat("body { color: red; }\n", 200)), ModTime: time.Unix(1, 0)}}); err != nil {
		t.Fatal(err)
	}
	if extra != nil {
		extra(app)
	}
	return app
}

func get(app *collage.App, path string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, r)
	return rec
}

func decode(t *testing.T, encoding string, body []byte) string {
	t.Helper()
	var r io.Reader
	switch encoding {
	case "br":
		r = brotli.NewReader(bytes.NewReader(body))
	case "gzip":
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r = gr
	default:
		return string(body)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decoding %s: %v", encoding, err)
	}
	return string(out)
}

func TestNegotiation(t *testing.T) {
	app := site(t, compress.Options{}, nil)
	for _, c := range []struct{ accept, want string }{
		{"gzip, deflate, br", "br"},
		{"gzip", "gzip"},
		{"br;q=0, gzip", "gzip"},
		{"*", "br"},
		{"*, br;q=0", "gzip"},
		{"x-gzip", "gzip"},
		{"deflate", ""},
		{"", ""},
	} {
		rec := get(app, "/", "Accept-Encoding", c.accept)
		if got := rec.Header().Get("Content-Encoding"); got != c.want {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want %q", c.accept, got, c.want)
			continue
		}
		if body := decode(t, c.want, rec.Body.Bytes()); body != big {
			t.Errorf("Accept-Encoding %q: body does not decode to the page", c.accept)
		}
		if rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: Vary = %q", c.accept, rec.Header().Get("Vary"))
		}
	}
}

func TestCompressedHeaders(t *testing.T) {
	rec := get(site(t, compress.Options{}, nil), "/", "Accept-Encoding", "br")
	if rec.Body.Len() >= len(big) {
		t.Errorf("compressed %d bytes into %d", len(big), rec.Body.Len())
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length %s for a %d-byte body", cl, rec.Body.Len())
	}
	if etag := rec.Header().Get("ETag"); !strings.HasSuffix(etag, `-br"`) {
		t.Errorf("ETag = %q, want the encoding inside the quotes", etag)
	}
}

// The ETag differs per encoding, and a conditional request is answered with a
// 304 only for the representation the client holds.
func TestConditionalRequests(t *testing.T) {
	app := site(t, compress.Options{}, nil)
	plain := get(app, "/").Header().Get("ETag")
	br := get(app, "/", "Accept-Encoding", "br").Header().Get("ETag")
	gz := get(app, "/", "Accept-Encoding", "gzip").Header().Get("ETag")
	if plain == "" || br == plain || gz == plain || br == gz {
		t.Fatalf("ETags identity %q, br %q, gzip %q: want three distinct", plain, br, gz)
	}

	rec := get(app, "/", "Accept-Encoding", "br", "If-None-Match", br)
	if rec.Code != http.StatusNotModified || rec.Header().Get("ETag") != br {
		t.Errorf("revalidating br: %d ETag %q, want 304 %q", rec.Code, rec.Header().Get("ETag"), br)
	}
	rec = get(app, "/", "Accept-Encoding", "gzip", "If-None-Match", br)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("a br tag from a gzip client: %d %q, want a full gzip response", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	rec = get(app, "/", "If-None-Match", br)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("a br tag from an identity client: %d %q, want a full identity response", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	rec = get(app, "/", "Accept-Encoding", "br", "If-None-Match", plain)
	if rec.Code != http.StatusNotModified || rec.Header().Get("ETag") != plain {
		t.Errorf("an identity tag from a br client: %d ETag %q, want 304 %q", rec.Code, rec.Header().Get("ETag"), plain)
	}
	rec = get(app, "/static/app.css", "Accept-Encoding", "gzip")
	gzCSS := rec.Header().Get("ETag")
	if rec.Header().Get("Content-Encoding") != "gzip" || !strings.HasSuffix(gzCSS, `-gz"`) {
		t.Fatalf("mounted file: %q ETag %q", rec.Header().Get("Content-Encoding"), gzCSS)
	}
	if rec.Header().Get("Accept-Ranges") != "" {
		t.Error("a compressed response offers ranges of the uncompressed bytes")
	}
	if rec := get(app, "/static/app.css", "Accept-Encoding", "gzip", "If-None-Match", gzCSS); rec.Code != http.StatusNotModified || rec.Header().Get("ETag") != gzCSS {
		t.Errorf("revalidating a mounted file: %d %q", rec.Code, rec.Header().Get("ETag"))
	}
}

func TestLeftAlone(t *testing.T) {
	app := site(t, compress.Options{}, func(app *collage.App) {
		if err := app.Handle("/raw/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte(big))
		})); err != nil {
			t.Fatal(err)
		}
		if err := app.Handle("/events/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + big + "\n\n"))
			http.NewResponseController(w).Flush()
		})); err != nil {
			t.Fatal(err)
		}
	})
	for _, c := range []struct {
		name, path string
		headers    []string
		vary       bool
	}{
		{"a small page", "/small", nil, true},
		{"an image", "/logo.png", nil, false},
		{"a range", "/static/app.css", []string{"Range", "bytes=0-99"}, false},
		{"an encoded body", "/raw/x", nil, false},
		{"an event stream", "/events/x", nil, false},
	} {
		rec := get(app, c.path, append([]string{"Accept-Encoding", "br, gzip"}, c.headers...)...)
		if ce := rec.Header().Get("Content-Encoding"); ce != "" && c.path != "/raw/x" {
			t.Errorf("%s: Content-Encoding %q", c.name, ce)
		}
		if got := rec.Header().Get("Vary") == "Accept-Encoding"; got != c.vary {
			t.Errorf("%s: Vary %q", c.name, rec.Header().Get("Vary"))
		}
		if c.path == "/static/app.css" && rec.Code != http.StatusPartialContent {
			t.Errorf("%s: %d, want 206", c.name, rec.Code)
		}
		if c.path == "/raw/x" && (rec.Header().Get("Content-Encoding") != "gzip" || rec.Body.String() != big) {
			t.Errorf("%s: the handler's body was touched", c.name)
		}
	}
}

func TestHead(t *testing.T) {
	app := site(t, compress.Options{}, nil)
	r := httptest.NewRequest(http.MethodHead, "/", nil)
	r.Header.Set("Accept-Encoding", "br")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, r)
	if rec.Header().Get("Content-Encoding") != "br" || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %q with %d bytes", rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}
}

// A compressed body is kept under its ETag: a handler answering with the same
// ETag is answered with what was compressed the first time, which is what spares
// collage's cached pages a compression per request.
func TestCacheByETag(t *testing.T) {
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(`{"n":` + strconv.Itoa(int(n)) + `,"pad":"` + strings.Repeat("x", 2000) + `"}`))
	})
	for _, c := range []struct {
		name   string
		opts   compress.Options
		cached bool
	}{{"default", compress.Options{}, true}, {"cache off", compress.Options{CacheBytes: -1}, false}} {
		calls.Store(0)
		app := site(t, c.opts, func(app *collage.App) {
			if err := app.Handle("/api/", handler); err != nil {
				t.Fatal(err)
			}
		})
		first := decode(t, "br", get(app, "/api/x", "Accept-Encoding", "br").Body.Bytes())
		second := get(app, "/api/x", "Accept-Encoding", "br")
		if !strings.HasPrefix(first, `{"n":1,`) {
			t.Fatalf("%s: first body %.20q", c.name, first)
		}
		got := decode(t, "br", second.Body.Bytes())
		if c.cached != strings.HasPrefix(got, `{"n":1,`) {
			t.Errorf("%s: second body %.20q", c.name, got)
		}
		if second.Header().Get("ETag") != `"v1-br"` {
			t.Errorf("%s: ETag %q", c.name, second.Header().Get("ETag"))
		}
		// gzip is a different entry.
		if got := decode(t, "gzip", get(app, "/api/x", "Accept-Encoding", "gzip").Body.Bytes()); !strings.HasPrefix(got, `{"n":3,`) {
			t.Errorf("%s: gzip body %.20q", c.name, got)
		}
	}
}

// A body larger than what is held back is compressed as it arrives, and a flush
// reaches the client.
func TestStreaming(t *testing.T) {
	chunk := strings.Repeat("streamed text, ", 1000)
	app := site(t, compress.Options{}, func(app *collage.App) {
		if err := app.Handle("/stream/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			for range 20 {
				_, _ = io.WriteString(w, chunk)
			}
			http.NewResponseController(w).Flush()
			_, _ = io.WriteString(w, "end")
		})); err != nil {
			t.Fatal(err)
		}
	})
	rec := get(app, "/stream/x", "Accept-Encoding", "gzip")
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("headers %v", rec.Header())
	}
	if !rec.Flushed {
		t.Error("the flush did not reach the client")
	}
	if got := decode(t, "gzip", rec.Body.Bytes()); got != strings.Repeat(chunk, 20)+"end" {
		t.Errorf("streamed body of %d bytes does not decode to what was written", len(got))
	}
}

// A hijacked connection is the handler's: nothing is compressed or written after.
func TestHijack(t *testing.T) {
	app := site(t, compress.Options{}, func(app *collage.App) {
		if err := app.Handle("/ws/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 6\r\nConnection: close\r\n\r\nraw ok")
			_ = rw.Flush()
		})); err != nil {
			t.Fatal(err)
		}
	})
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/ws/x", nil)
	req.Header.Set("Accept-Encoding", "br")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(bufio.NewReader(resp.Body))
	if string(body) != "raw ok" || resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("hijacked response %q %v", body, resp.Header)
	}
}

func TestMisconfiguration(t *testing.T) {
	for name, opts := range map[string]compress.Options{
		"gzip level":   {GzipLevel: 10},
		"brotli level": {BrotliLevel: 12},
		"build level":  {BuildBrotliLevel: -1},
		"min size":     {MinSize: -5},
		"type":         {Types: []string{"nonsense"}},
	} {
		if rec := get(site(t, opts, nil), "/"); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d, want 503", name, rec.Code)
		}
	}
}

func TestConfiguration(t *testing.T) {
	app, err := collage.New(&collage.Config{
		Server:       collage.ServerConfig{Host: "localhost", Port: 3000},
		Template:     collage.TemplateConfig{FS: fstest.MapFS{"t/small.html": {Data: []byte(`<p>small, but compressed anyway</p>`)}}, Root: "t"},
		Plugins:      []collage.Plugin{compress.New(compress.Options{})},
		PluginConfig: map[string]json.RawMessage{"elagoht/compress": json.RawMessage(`{"minSize": 1}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.RegisterPage(collage.NewPage("small").WithContent(collage.NewFragment("small", "small.html").Build()).WithPath("en", "/").Build()); err != nil {
		t.Fatal(err)
	}
	if rec := get(app, "/", "Accept-Encoding", "gzip"); rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("minSize from configuration was not applied: %v", rec.Header())
	}
}

func TestStaticBuild(t *testing.T) {
	out := t.TempDir()
	app := site(t, compress.Options{}, nil)
	b, err := collage.NewBuilder(app, collage.BuildOptions{OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	var index string
	var siblings []string
	_ = filepath.WalkDir(out, func(path string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(out, path)
		if strings.HasSuffix(rel, ".br") || strings.HasSuffix(rel, ".gz") {
			siblings = append(siblings, filepath.ToSlash(rel))
		}
		if rel == "index.html" {
			index = path
		}
		return nil
	})
	if index == "" {
		t.Fatal("no index.html written")
	}
	for _, want := range []string{"index.html.br", "index.html.gz", "static/app.css.br", "static/app.css.gz"} {
		if !contains(siblings, want) {
			t.Errorf("no %s among %v", want, siblings)
		}
	}
	for _, s := range siblings {
		if strings.HasPrefix(s, "small") || strings.HasPrefix(s, "logo.png") {
			t.Errorf("%s written for a small or incompressible file", s)
		}
	}
	br, _ := os.ReadFile(index + ".br")
	gz, _ := os.ReadFile(index + ".gz")
	if decode(t, "br", br) != big || decode(t, "gzip", gz) != big {
		t.Error("a sibling does not decode to its page")
	}

	out2 := t.TempDir()
	b, _ = collage.NewBuilder(site(t, compress.Options{NoPrecompress: true}, nil), collage.BuildOptions{OutDir: out2})
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out2, "index.html.br")); err == nil {
		t.Error("NoPrecompress wrote a sibling")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
