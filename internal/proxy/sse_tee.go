package proxy

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/AsatovDias/trimbar/internal/anthropic"
)

const (
	sseReadChunkSize  = 4096
	sseAsyncQueueSize = 512
)

// StreamSSE forwards an upstream Server-Sent Events body to w with zero buffering
// (flushing after every read chunk) while asynchronously feeding a copy of all bytes
// to an anthropic.SSEParser.
func StreamSSE(w http.ResponseWriter, body io.Reader, contentEncoding string) (anthropic.StreamResult, int64, error) {
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		// Flush response headers immediately so the client does not wait for the first chunk.
		flusher.Flush()
	}

	chunks := make(chan []byte, sseAsyncQueueSize)
	done := make(chan struct{})
	var result anthropic.StreamResult

	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[trimbar] WARN: panic recovered during SSE parsing: %v\n", r)
			}
		}()
		parser := anthropic.NewSSEParser()
		if strings.EqualFold(strings.TrimSpace(contentEncoding), "gzip") {
			cr := &chanReader{ch: chunks}
			gz, err := gzip.NewReader(cr)
			if err == nil {
				_, _ = io.Copy(parser, gz)
				_ = gz.Close()
			} else {
				// Drain remaining chunks so the writer loop never stalls.
				for range chunks {
				}
			}
		} else {
			for chunk := range chunks {
				_, _ = parser.Write(chunk)
			}
		}
		result = parser.Finish()
	}()

	buf := make([]byte, sseReadChunkSize)
	var totalBytes int64
	var streamErr error

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			totalBytes += int64(written)
			if flusher != nil {
				flusher.Flush()
			}

			cp := make([]byte, n)
			copy(cp, buf[:n])
			chunks <- cp

			if writeErr != nil {
				streamErr = writeErr
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				streamErr = readErr
			}
			break
		}
	}

	close(chunks)
	<-done

	return result, totalBytes, streamErr
}

// chanReader adapts a channel of byte slices into an io.Reader for gzip streaming.
type chanReader struct {
	ch  <-chan []byte
	cur []byte
}

func (r *chanReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		chunk, ok := <-r.ch
		if !ok {
			return 0, io.EOF
		}
		r.cur = chunk
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}
