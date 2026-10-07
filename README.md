# elagoht/compress

A collage plugin that compresses responses with Brotli and gzip: negotiated per
request, kept per ETag so a page collage serves from its cache is not compressed
again for every reader, and written beside a static build as `.br` and `.gz` files
for a host that serves precompressed files. Registering it is the whole of it.

```go
app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{
		compress.New(compress.Options{}),
		secure.New(secure.Options{CSP: "..."}),
	},
})
```

Requires collage v0.50.0 or later.

Order does not matter for elagoht/secure and elagoht/honeypot (v0.2.0 and v0.4.0
or later): they rewrite a page in collage's `PersonaliseHook`, before any middleware
sees it. **Register compress before any other plugin that rewrites response bodies**
in a middleware of its own. The first plugin registered is the outermost middleware,
so it sees the body last; a plugin registered before it would be handed compressed
bytes to search for its placeholder in.

A response whose `Cache-Control` carries `private` or `no-store`, such as a page
personalised per response, is compressed but never kept in the cache.

## What is compressed

Every response — a page, a document, a mounted file, a handler of your own mounted
with `Handle` — whose `Content-Type` is a text type:

- `text/*` (HTML, CSS, plain text, CSV)
- JSON: `application/json` and any `+json` (`application/manifest+json`, `application/ld+json`)
- XML: `application/xml` and any `+xml` (RSS, Atom, SVG)
- JavaScript: `application/javascript`, `application/x-javascript`, `application/ecmascript`
- `application/wasm`
- and whatever `Types` adds

and whose body is at least `MinSize` bytes. Images, video, archives and fonts in
compressed formats are sent as they are: compressing compressed bytes costs time
to make them larger.

The encoding is the best one the request's `Accept-Encoding` allows: Brotli, then
gzip, then none. A coding the header refuses with `q=0` is refused even when `*`
would accept it. The response gets `Content-Encoding`, loses `Content-Length` when
it is streamed and `Accept-Ranges` always, and carries `Vary: Accept-Encoding` — on
every response of a compressible type, compressed or not, so a cache in front never
hands one reader's representation to a client that accepts another.

## Left alone

- `text/event-stream`: an event has to reach the reader when it is written, and a
  compressor holds bytes back until it has a block.
- A hijacked connection (a WebSocket): it is the handler's.
- A `Range` request: a range is a range of the bytes the handler has, and a resumed
  download cannot be spliced from two encodings. It is answered uncompressed, as
  `206` with the mount's own `ETag`.
- A response that already has a `Content-Encoding`.
- `204`, `206`, `304` bodies and informational responses.

## ETags and 304

A compressed response's `ETag` gains the encoding inside the quotes: `"abc"` is
sent as `"abc-br"` or `"abc-gz"`, and a weak tag stays weak. Two encodings of one
page are different bytes, and a tag that named both would let a cache revalidate a
gzip copy with a Brotli response.

A conditional request still gets its `304`. On the way in, the suffix of the
negotiated encoding is taken off `If-None-Match`, so collage — which only knows the
uncompressed page's tag — matches it; on the way out, the `304` names the tag the
client sent. A tag carrying another encoding's suffix matches nothing, and the
client gets the full response in the encoding it asked for now.

## The cache

A `200` response with an `ETag` is compressed once. The compressed body is kept
under the ETag and the encoding in an LRU bounded by `CacheBytes`, and the next
response with the same tag is answered from it without compressing. collage's
ETags are content hashes, so a cached page, a document and a mounted file are each
compressed once per encoding for as long as they stay the same.

This trusts the ETag to name the bytes. A handler of your own that sends one ETag
for two different bodies is answered with the first body it compressed; turn the
cache off with a negative `CacheBytes` if you have such a handler. No single body
larger than an eighth of `CacheBytes` is kept.

A body is held back until it is 64 KiB or the handler returns. One that ends first
is compressed whole, sent with a `Content-Length` and cached; a larger one is
compressed as it arrives and cached if it is small enough to keep. A handler that
flushes starts the stream at once.

## Static builds

When a static build has written every file, a `.br` and a `.gz` are written beside
each compressible file of at least `MinSize` — pages, documents and copied mounts
alike — at the build levels, Brotli 11 and gzip 9 by default, since each is
compressed once and served for as long as the build is deployed. They are for a
host that serves precompressed files: nginx's `gzip_static` and `brotli_static`,
Caddy's `file_server { precompressed br gzip }`, most CDNs and static hosts. A file
that cannot be written is reported as an error-level finding, which fails the
build. `NoPrecompress` turns this off.

## Options

| Option | Default | |
| --- | --- | --- |
| `MinSize` | `1024` | Bytes below which a response is sent as it is |
| `GzipLevel` | `6` | gzip level of a served response, 1–9 |
| `BrotliLevel` | `5` | Brotli level of a served response, 1–11. Above 5 Brotli costs more time than the bytes it saves while a reader waits |
| `BuildGzipLevel` | `9` | gzip level of a static build's `.gz` files |
| `BuildBrotliLevel` | `11` | Brotli level of a static build's `.br` files |
| `CacheBytes` | 32 MiB | Bound of the compressed-body cache; negative turns it off |
| `Types` | none | Media types to compress beyond the built-in ones |
| `NoPrecompress` | `false` | A static build writes no `.br` or `.gz` files |

A level outside its range, a negative `MinSize` or a `Types` entry that is not a
media type stops the application from starting.

## Configuration

```json
{
  "elagoht/compress": {
    "minSize": 512,
    "gzipLevel": 6,
    "brotliLevel": 4,
    "cacheBytes": 67108864,
    "types": ["application/x-yaml"]
  }
}
```

## Limitations

- **Register it first.** A body-rewriting plugin registered before it sees
  compressed bytes. The plugin cannot tell, so it cannot refuse. `elagoht/health`
  may be listed before or after it: compress only re-encodes, and health's bodies
  are tiny.
- **No `deflate` or `zstd`.** Every client that accepts either accepts gzip.
- **No compression of `Range` requests**, and so none of a resumed download.
- **A `HEAD` request whose handler writes no body** is answered with the headers
  a GET of unknown length would get: compressed, with no `Content-Length`.
- **The cache trusts ETags.** See above.
- **Precompressed files are not served by this plugin.** A running collage server
  compresses (or answers from its cache); the `.br` and `.gz` files are for a
  static host. A mount that holds precompressed files of its own serves them under
  their own names and types.

## Changes

### v0.1.7

- Docs only: the README says `elagoht/health` may be listed before or after the plugin. Nothing else changes.
