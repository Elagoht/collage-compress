package compress

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/Elagoht/collage/pkg/collage"
	"github.com/andybalholm/brotli"
)

// OnBuildFinished writes a .br and a .gz file beside every compressible file a
// static build wrote — pages, documents and mounted files alike — for a host that
// serves precompressed files. They are compressed at the build levels, since each
// is compressed once and served for as long as the build is deployed.
func (p *Plugin) OnBuildFinished(ctx context.Context, ev *collage.BuildFinishedEvent) error {
	if p.opts.NoPrecompress {
		return nil
	}
	type failure struct{ path, message string }
	var (
		mu       sync.Mutex
		failures []failure
		wg       sync.WaitGroup
		files    = make(chan collage.BuiltFile)
	)
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for f := range files {
				if err := p.precompress(f.File); err != nil {
					mu.Lock()
					failures = append(failures, failure{f.Path, err.Error()})
					mu.Unlock()
				}
			}
		})
	}
	for _, f := range ev.Files {
		if ctx.Err() != nil {
			break
		}
		// A sibling written by an earlier build into the same directory is not
		// compressed again.
		if strings.HasSuffix(f.File, ".br") || strings.HasSuffix(f.File, ".gz") {
			continue
		}
		files <- f
	}
	close(files)
	wg.Wait()
	for _, f := range failures {
		ev.Error(f.path, "precompress", f.message)
	}
	return ctx.Err()
}

// precompress writes file's siblings when it is compressible and large enough.
func (p *Plugin) precompress(file string) error {
	body, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if len(body) < p.opts.MinSize {
		return nil
	}
	contentType := mime.TypeByExtension(filepath.Ext(file))
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	if !p.compressible(contentType) {
		return nil
	}
	var br bytes.Buffer
	bw := brotli.NewWriterLevel(&br, p.opts.BuildBrotliLevel)
	if _, err := bw.Write(body); err != nil {
		return err
	}
	if err := bw.Close(); err != nil {
		return err
	}
	var gz bytes.Buffer
	gw, err := gzip.NewWriterLevel(&gz, p.opts.BuildGzipLevel)
	if err != nil {
		return err
	}
	if _, err := gw.Write(body); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	for ext, data := range map[string][]byte{".br": br.Bytes(), ".gz": gz.Bytes()} {
		if err := os.WriteFile(file+ext, data, info.Mode().Perm()); err != nil {
			return fmt.Errorf("compress: %w", err)
		}
	}
	return nil
}
