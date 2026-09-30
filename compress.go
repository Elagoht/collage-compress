// Package compress is a collage plugin that compresses responses with Brotli and
// gzip.
//
//	app, err := collage.New(&collage.Config{
//		Plugins: []collage.Plugin{compress.New(compress.Options{}), secure.New(secure.Options{})},
//	})
//
// Every response of a text type — HTML, CSS, JavaScript, JSON, XML, SVG,
// WebAssembly — larger than MinSize is compressed with the best encoding the
// client accepts, Brotli before gzip, whether a page, a document, a mounted file
// or a handler of your own produced it. Its ETag gains the encoding, "-br" or
// "-gz" inside the quotes, so a conditional request is still answered with a 304
// for the representation the client holds and never with one for another.
//
// A response carrying an ETag is compressed once: the compressed body is kept in
// a bounded cache under the ETag and the encoding, so a page collage serves from
// its own cache is not compressed again for every reader.
//
// A static build gets a .br and a .gz file beside every compressible file it
// wrote, for a host that serves precompressed files (nginx's gzip_static and
// brotli_static, Caddy's precompressed).
//
// Register it before any plugin that rewrites response bodies, such as
// elagoht/secure: the first plugin registered is the outermost middleware, and a
// body has to be rewritten before it is compressed, not after.
package compress

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Elagoht/collage/pkg/collage"
	"github.com/andybalholm/brotli"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/compress"

// The encodings the plugin produces, as Accept-Encoding and Content-Encoding
// name them.
const (
	Brotli = "br"
	Gzip   = "gzip"
)

// Options configures the plugin. A zero value is a default.
type Options struct {
	// MinSize is the size in bytes below which a response is sent as it is: a
	// small body gains a few bytes at best, and costs a compressor either way.
	// Default 1024.
	MinSize int `json:"minSize"`
	// GzipLevel is the gzip level of a served response, 1 to 9. Default 6.
	GzipLevel int `json:"gzipLevel"`
	// BrotliLevel is the Brotli level of a served response, 1 to 11. Default 5:
	// above it Brotli grows slow enough to cost more time than the bytes it saves
	// on a response compressed while the reader waits.
	BrotliLevel int `json:"brotliLevel"`
	// BuildGzipLevel and BuildBrotliLevel are the levels of the files a static
	// build writes, compressed once and served many times. Default 9 and 11.
	BuildGzipLevel   int `json:"buildGzipLevel"`
	BuildBrotliLevel int `json:"buildBrotliLevel"`
	// CacheBytes bounds the cache of compressed bodies, in bytes. Default 32 MiB;
	// negative turns the cache off.
	CacheBytes int `json:"cacheBytes"`
	// Types are media types to compress beyond the built-in ones: "font/ttf",
	// "application/x-yaml".
	Types []string `json:"types"`
	// NoPrecompress stops a static build from writing .br and .gz files.
	NoPrecompress bool `json:"noPrecompress"`
}

// Plugin compresses responses.
type Plugin struct {
	opts   Options
	types  map[string]bool
	cache  *lru
	gzips  sync.Pool
	brots  sync.Pool
	logger *slog.Logger
}

var (
	_ collage.Plugin            = (*Plugin)(nil)
	_ collage.BuildFinishedHook = (*Plugin)(nil)
)

// New returns a plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.1.2" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

// Init reads the configuration, refuses a level no compressor has, and wraps
// every request.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if err := host.Config(&p.opts); err != nil {
		return err
	}
	p.logger = host.Logger()
	o := &p.opts
	if o.MinSize < 0 {
		return fmt.Errorf("compress: minSize %d is negative", o.MinSize)
	}
	if o.MinSize == 0 {
		o.MinSize = 1024
	}
	levels := []struct {
		name     string
		level    *int
		def, max int
	}{
		{"gzipLevel", &o.GzipLevel, 6, 9},
		{"brotliLevel", &o.BrotliLevel, 5, 11},
		{"buildGzipLevel", &o.BuildGzipLevel, 9, 9},
		{"buildBrotliLevel", &o.BuildBrotliLevel, 11, 11},
	}
	for _, l := range levels {
		if *l.level == 0 {
			*l.level = l.def
		}
		if *l.level < 1 || *l.level > l.max {
			return fmt.Errorf("compress: %s %d is outside 1 to %d", l.name, *l.level, l.max)
		}
	}
	p.types = make(map[string]bool)
	for _, t := range o.Types {
		media, _, err := mime.ParseMediaType(t)
		if err != nil || !strings.Contains(media, "/") {
			return fmt.Errorf("compress: %q is not a media type", t)
		}
		p.types[media] = true
	}
	switch {
	case o.CacheBytes == 0:
		o.CacheBytes = 32 << 20
		p.cache = newLRU(o.CacheBytes)
	case o.CacheBytes > 0:
		p.cache = newLRU(o.CacheBytes)
	}
	gzipLevel, brotliLevel := o.GzipLevel, o.BrotliLevel
	p.gzips.New = func() any { // any: sync.Pool's own signature
		w, _ := gzip.NewWriterLevel(io.Discard, gzipLevel) // the level was checked above
		return w
	}
	p.brots.New = func() any { // any: sync.Pool's own signature
		return brotli.NewWriterLevel(io.Discard, brotliLevel)
	}
	return host.Use(p.middleware)
}

// compressible reports whether a response of contentType is worth compressing.
// Everything else — images, video, fonts in formats that are already compressed,
// archives — is sent as it is, because compressing compressed bytes spends time to
// make them larger.
func (p *Plugin) compressible(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case media == "text/event-stream":
		// A stream's events have to reach the reader as they are written; a
		// compressor holds them back until it has a block's worth.
		return false
	case strings.HasPrefix(media, "text/"),
		strings.HasSuffix(media, "+json"), strings.HasSuffix(media, "+xml"),
		media == "application/json", media == "application/xml",
		media == "application/javascript", media == "application/x-javascript",
		media == "application/ecmascript", media == "application/wasm",
		media == "image/svg+xml":
		return true
	}
	return p.types[media]
}

// negotiate picks the encoding for a request's Accept-Encoding: Brotli when the
// client accepts it, gzip when it accepts that, and "" for neither. A coding the
// header names with q=0 is refused even when "*" would accept it, as RFC 9110
// says.
func negotiate(header string) string {
	if header == "" {
		return ""
	}
	q := map[string]float64{}
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding == "" {
			continue
		}
		weight := 1.0
		for _, param := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					weight = f
				}
			}
		}
		q[coding] = weight
	}
	accepts := func(coding string) bool {
		if w, ok := q[coding]; ok {
			return w > 0
		}
		if coding == Gzip {
			if w, ok := q["x-gzip"]; ok {
				return w > 0
			}
		}
		w, ok := q["*"]
		return ok && w > 0
	}
	switch {
	case accepts(Brotli):
		return Brotli
	case accepts(Gzip):
		return Gzip
	}
	return ""
}

// suffix is what an encoding adds to an ETag.
func suffix(encoding string) string {
	if encoding == Brotli {
		return "-br"
	}
	return "-gz"
}

// tagFor is etag as it is sent for encoding: `"abc"` becomes `"abc-br"`, and a
// weak tag stays weak. A tag that is not quoted is not one this plugin can amend,
// and "" is returned so the response goes without one rather than with one that
// names the uncompressed bytes.
func tagFor(etag, encoding string) string {
	if len(etag) < 2 || !strings.HasSuffix(etag, `"`) {
		return ""
	}
	return etag[:len(etag)-1] + suffix(encoding) + `"`
}

// stripTags removes encoding's suffix from each tag of an If-None-Match, so the
// handler — which knows only the uncompressed representation's ETag — can match
// it, and returns the tags it stripped, which a 304 is answered with suffixed
// again. A tag naming another encoding is left as it is, and matches nothing:
// the client holds a representation this response will not be.
func stripTags(header, encoding string) (string, map[string]bool) {
	end := suffix(encoding) + `"`
	stripped := map[string]bool{}
	parts := strings.Split(header, ",")
	for i, part := range parts {
		tag := strings.TrimSpace(part)
		if strings.HasSuffix(tag, end) {
			tag = tag[:len(tag)-len(end)] + `"`
			stripped[strings.TrimPrefix(tag, "W/")] = true
		}
		parts[i] = tag
	}
	return strings.Join(parts, ", "), stripped
}

func (p *Plugin) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A range is a range of the bytes the handler has; there is no honest way
		// to answer one from a compressed stream, and a resumed download of a
		// compressed body would splice two different encodings together.
		if r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w, p: p, encoding: negotiate(r.Header.Get("Accept-Encoding")), head: r.Method == http.MethodHead}
		if cw.encoding != "" {
			if inm := r.Header.Get("If-None-Match"); inm != "" {
				var stripped string
				stripped, cw.stripped = stripTags(inm, cw.encoding)
				if len(cw.stripped) > 0 {
					r = r.Clone(r.Context())
					r.Header.Set("If-None-Match", stripped)
				}
			}
		}
		next.ServeHTTP(cw, r)
		cw.finish()
	})
}
