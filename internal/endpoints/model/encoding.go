package model

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

var (
	errUnsupportedEncoding = errors.New("unsupported request content encoding")
	errUndecodableBody     = errors.New("request body could not be decoded")
)

// maxZstdWindow bounds a zstd decoder's memory, separately from the decoded
// size limit; clients use windows of a few MiB.
const maxZstdWindow = 32 << 20

// decodedBody reads a model request body, decoding a gzip or zstd
// Content-Encoding such as Codex sends to the ChatGPT backend, so the gateway
// inspects and forwards plain JSON. The decoded body is bounded like any other.
func decodedBody(r *http.Request, limit int64) ([]byte, error) {
	encoded, err := readBounded(r.Body, limit)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return encoded, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(encoded))
		if err != nil {
			return nil, errUndecodableBody
		}
		defer func() { _ = reader.Close() }()
		return readDecoded(reader, limit)
	case "zstd":
		reader, err := zstd.NewReader(bytes.NewReader(encoded), zstd.WithDecoderMaxWindow(maxZstdWindow),
			zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, errUndecodableBody
		}
		defer reader.Close()
		return readDecoded(reader, limit)
	}
	return nil, errUnsupportedEncoding
}

// readDecoded reads at most limit decoded bytes, telling a corrupt body apart
// from one that decodes past the limit.
func readDecoded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, errUndecodableBody
	}
	if int64(len(data)) > limit {
		return nil, errors.New("decoded body exceeds the gateway limit")
	}
	return data, nil
}
