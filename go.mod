// A collage plugin that compresses responses with Brotli and gzip: negotiated
// per request, cached per ETag so collage's cached pages are not compressed
// again on every request, and written as .br and .gz siblings by a static build.
module github.com/Elagoht/collage-compress

go 1.26

require github.com/Elagoht/collage v0.55.0

require github.com/andybalholm/brotli v1.2.5

retract v0.1.4 // tagged by mistake on the previous release's code; use v0.1.5 or later
