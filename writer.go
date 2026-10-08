package compress

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// streamAt is how much of a compressible body is held back before compression
// starts streaming. A body that ends before it is compressed whole, which is what
// lets it be sent with a Content-Length and kept in the cache; one that does not
// is compressed as it arrives, so a large response is never held in memory.
const streamAt = 64 << 10

// encoder is what gzip.Writer and brotli.Writer have in common.
type encoder interface {
	io.WriteCloser
	Flush() error
	Reset(io.Writer)
}

type writerState int

const (
	undecided writerState = iota
	passing               // sent as the handler writes it
	pending               // compressible, held back until it is known to be large enough
	streaming             // compressed as it arrives
	cached                // answered from the cache; what the handler writes is dropped
	hijacked              // the connection is the handler's
)

// compressWriter decides, once the handler's headers are final, whether the
// response is compressed, and compresses it. Anything not compressible — an
// image, an event stream, a body already encoded — passes straight through, and
// so does a hijacked connection.
type compressWriter struct {
	http.ResponseWriter
	p        *Plugin
	encoding string
	head     bool
	// stripped are the tags an If-None-Match carried with this encoding's suffix,
	// which a 304 naming one of them is answered with suffixed again.
	stripped map[string]bool

	state  writerState
	status int
	etag   string // the handler's own, before the suffix
	// personal is a response marked private or no-store, kept out of the cache.
	personal bool
	body     bytes.Buffer
	enc      encoder
	tee      *teeWriter
}

func (w *compressWriter) WriteHeader(status int) {
	switch w.state {
	case undecided:
		w.decide(status)
	case passing:
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *compressWriter) Write(b []byte) (int, error) {
	if w.state == undecided {
		w.decide(http.StatusOK)
	}
	switch w.state {
	case passing:
		return w.ResponseWriter.Write(b)
	case cached:
		return len(b), nil
	case hijacked:
		return 0, http.ErrHijacked
	case pending:
		w.body.Write(b)
		if w.body.Len() >= max(streamAt, w.p.opts.MinSize) {
			w.startStream()
		}
		return len(b), nil
	default: // streaming
		return w.enc.Write(b)
	}
}

// decide looks at the response's status and headers, which the handler has
// finished writing, and chooses what happens to its body.
func (w *compressWriter) decide(status int) {
	h := w.Header()
	if status < http.StatusOK {
		// An informational response (103 Early Hints) precedes the real one,
		// whose headers are not final yet.
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.state = passing
	if status == http.StatusNotModified {
		// The handler matched an If-None-Match stripped of this encoding's
		// suffix; the client holds the compressed representation, and the 304
		// names it.
		if tag := h.Get("ETag"); tag != "" && w.stripped[strings.TrimPrefix(tag, "W/")] {
			if amended := tagFor(tag, w.encoding); amended != "" {
				h.Set("ETag", amended)
			}
			addVary(h)
		}
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if h.Get("Content-Encoding") != "" || status == http.StatusNoContent || status == http.StatusPartialContent ||
		!w.p.compressible(h.Get("Content-Type")) {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	// A private or no-store response is one reader's, and may carry a secret
	// beside what that reader sent: with SkipPrivate it is not compressed, so its
	// length says nothing about how well the two compress together (BREACH). It
	// does not depend on Accept-Encoding then, and gets no Vary.
	if w.p.opts.SkipPrivate && personal(h.Values("Cache-Control")) {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	// The response depends on Accept-Encoding whether or not this one is
	// compressed: a cache in front must not hand this reader's version to one
	// that accepts something else.
	addVary(h)
	if cl := h.Get("Content-Length"); w.encoding == "" || cl != "" && tooSmall(cl, w.p.opts.MinSize) {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.etag = h.Get("ETag")
	w.personal = personal(h.Values("Cache-Control"))
	if body, ok := w.lookup(); ok {
		w.encodingHeaders()
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.ResponseWriter.WriteHeader(status)
		if !w.head {
			_, _ = w.ResponseWriter.Write(body)
		}
		w.state = cached
		return
	}
	w.state = pending
}

func tooSmall(contentLength string, minSize int) bool {
	n, err := strconv.Atoi(contentLength)
	return err == nil && n < minSize
}

func (w *compressWriter) key() string { return w.etag + "\x00" + w.encoding }

// cacheable reports whether the compressed body may be kept: a complete 200
// response that names its content with an ETag, so the same key can never stand
// for other bytes. A private or no-store response is not kept: a plugin that
// personalises a page gives it a new ETag every time, so what was kept would never
// be asked for again and would only push out what is.
func (w *compressWriter) cacheable() bool {
	return w.p.cache != nil && w.status == http.StatusOK && w.etag != "" && !w.personal
}

// personal reports whether a Cache-Control header, in any of its lines, carries
// the private or no-store directive. Directive names are matched whole and
// without regard to case.
func personal(values []string) bool {
	for _, v := range values {
		for _, d := range strings.Split(v, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(d), "=")
			name = strings.TrimSpace(name)
			if equalASCIIFold(name, "private") || equalASCIIFold(name, "no-store") {
				return true
			}
		}
	}
	return false
}

// equalASCIIFold compares s with the lower-case ASCII name without regard to
// ASCII case. Unlike strings.EqualFold it does not fold Unicode, so "no-ſtore"
// (U+017F) is not "no-store".
func equalASCIIFold(s, name string) bool {
	if len(s) != len(name) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != name[i] {
			return false
		}
	}
	return true
}

func (w *compressWriter) lookup() ([]byte, bool) {
	if !w.cacheable() {
		return nil, false
	}
	return w.p.cache.get(w.key())
}

// encodingHeaders turns the handler's headers into the compressed
// representation's.
func (w *compressWriter) encodingHeaders() {
	h := w.Header()
	h.Set("Content-Encoding", w.encoding)
	h.Del("Content-Length")
	// Ranges were ranges of the uncompressed bytes.
	h.Del("Accept-Ranges")
	if w.etag != "" {
		if amended := tagFor(w.etag, w.encoding); amended != "" {
			h.Set("ETag", amended)
		} else {
			h.Del("ETag")
		}
	}
}

func addVary(h http.Header) {
	for _, v := range h.Values("Vary") {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name == "*" || strings.EqualFold(name, "Accept-Encoding") {
				return
			}
		}
	}
	h.Add("Vary", "Accept-Encoding")
}

// startStream sends the headers and compresses the rest of the body as it
// arrives, keeping a copy for the cache while it stays small enough to keep.
func (w *compressWriter) startStream() {
	w.encodingHeaders()
	w.ResponseWriter.WriteHeader(w.status)
	w.tee = &teeWriter{w: w.ResponseWriter}
	if w.cacheable() {
		w.tee.keep = true
		w.tee.limit = w.p.cache.maxEntry()
	}
	w.enc = w.p.encoder(w.encoding)
	w.enc.Reset(w.tee)
	w.state = streaming
	_, _ = w.enc.Write(w.body.Bytes())
	w.body = bytes.Buffer{}
}

// Flush sends what the handler has written so far. A handler that flushes wants
// its bytes on the wire now, so a body still held back starts streaming.
func (w *compressWriter) Flush() {
	switch w.state {
	case pending:
		w.startStream()
		fallthrough
	case streaming:
		_ = w.enc.Flush()
	case cached, hijacked:
		return
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack hands the connection to the handler, a WebSocket's, and steps aside.
func (w *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.state = hijacked
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the connection: a stream's write
// deadline, a full-duplex body.
func (w *compressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish sends what is still held back, once the handler has returned.
func (w *compressWriter) finish() {
	switch w.state {
	case pending:
		if w.head && w.body.Len() == 0 {
			// A HEAD response whose handler wrote no body says what a GET would
			// be sent with; its length was not below MinSize, or decide would
			// have let it pass.
			w.encodingHeaders()
			w.ResponseWriter.WriteHeader(w.status)
			return
		}
		if w.body.Len() < w.p.opts.MinSize {
			w.ResponseWriter.WriteHeader(w.status)
			_, _ = w.ResponseWriter.Write(w.body.Bytes())
			return
		}
		var out bytes.Buffer
		enc := w.p.encoder(w.encoding)
		enc.Reset(&out)
		_, _ = enc.Write(w.body.Bytes())
		if err := enc.Close(); err != nil {
			// A compressor writing to memory does not fail; if one did, the
			// body goes out as the handler wrote it.
			w.ResponseWriter.WriteHeader(w.status)
			_, _ = w.ResponseWriter.Write(w.body.Bytes())
			return
		}
		w.p.release(w.encoding, enc)
		w.encodingHeaders()
		w.Header().Set("Content-Length", strconv.Itoa(out.Len()))
		w.ResponseWriter.WriteHeader(w.status)
		if !w.head {
			_, _ = w.ResponseWriter.Write(out.Bytes())
		}
		if w.cacheable() {
			w.p.cache.put(w.key(), out.Bytes())
		}
	case streaming:
		err := w.enc.Close()
		w.p.release(w.encoding, w.enc)
		if err == nil && w.tee.keep && !w.tee.over && w.tee.err == nil {
			w.p.cache.put(w.key(), w.tee.kept.Bytes())
		}
	}
}

// teeWriter writes a compressed stream to the client and keeps a copy until it
// grows past limit.
type teeWriter struct {
	w     io.Writer
	keep  bool
	over  bool
	limit int
	kept  bytes.Buffer
	err   error
}

func (t *teeWriter) Write(b []byte) (int, error) {
	if t.keep && !t.over {
		if t.kept.Len()+len(b) > t.limit {
			t.over = true
			t.kept = bytes.Buffer{}
		} else {
			t.kept.Write(b)
		}
	}
	n, err := t.w.Write(b)
	if err != nil {
		t.err = err
	}
	return n, err
}

// encoder takes a compressor for encoding from its pool.
func (p *Plugin) encoder(encoding string) encoder {
	if encoding == Brotli {
		return p.brots.Get().(encoder)
	}
	return p.gzips.Get().(encoder)
}

// release returns a closed compressor to its pool, pointed at nothing so it does
// not keep the response it wrote to alive.
func (p *Plugin) release(encoding string, enc encoder) {
	enc.Reset(io.Discard)
	if encoding == Brotli {
		p.brots.Put(enc)
		return
	}
	p.gzips.Put(enc)
}
