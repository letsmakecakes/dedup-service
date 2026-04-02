// Package fingerprint derives a deterministic SHA-256 key from the raw
// properties of an HTTP request. The key is used as a Redis SETNX key to
// detect duplicates.
//
// Fingerprint inputs (in hash order):
//  1. HTTP method        — r.Method, exactly as received
//  2. Request URI+query  — r.RequestURI, exactly as received
//  3. Request body       — full raw request body bytes
//
// No identity signal (Authorization, session header, client IP) is included.
// This is safe because identical requests from different callers cannot occur
// by design in this deployment — uniqueness is guaranteed by the request
// content itself (e.g. resource IDs in the URI or body).
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"sync"
	"unsafe"
)

const redisKeyPrefix = "dedup:"

// sha256 digest length and hex-encoded length.
const (
	sha256Len    = sha256.Size     // 32
	sha256HexLen = sha256.Size * 2 // 64
)

// Pools to reduce per-request heap allocations on the hot path.
var (
	// hasherPool recycles sha256.digest objects (~100 B each).
	hasherPool = sync.Pool{
		New: func() any { return sha256.New() },
	}
)

// Request holds the raw inputs used to compute the fingerprint.
// Fields are taken verbatim from the incoming HTTP request with no
// normalisation, case folding, or whitespace trimming.
type Request struct {
	Method string // r.Method — e.g. "POST"
	URI    string // r.RequestURI — e.g. "/api/orders?ref=abc"
	Body   []byte // full raw body bytes
}

// FromHTTP constructs a Request directly from r without any normalisation.
// Nginx forwards the original client request as-is via proxy_pass.
//
// r.Body is read fully. The caller must not attempt to
// re-read r.Body after this call.
func FromHTTP(r *http.Request) (*Request, error) {
	fp := &Request{
		Method: r.Method,
		URI:    r.RequestURI,
	}

	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		fp.Body = body
	}

	return fp, nil
}

// RedisKey returns the fully-qualified Redis key for this fingerprint.
// Format: "dedup:<64-char hex SHA-256>"
//
// The key is built in a single allocation: prefix (6 bytes) + hex hash (64 bytes).
func (f *Request) RedisKey() string {
	var buf [len(redisKeyPrefix) + sha256HexLen]byte
	copy(buf[:], redisKeyPrefix)
	f.hashInto(buf[len(redisKeyPrefix):])
	return string(buf[:])
}

// Hash returns the lowercase hex-encoded SHA-256 of the fingerprint inputs.
func (f *Request) Hash() string {
	var buf [sha256HexLen]byte
	f.hashInto(buf[:])
	return string(buf[:])
}

// hashInto computes the SHA-256 and writes the 64 hex chars into dst.
// dst must be at least 64 bytes. The hasher is borrowed from a pool.
func (f *Request) hashInto(dst []byte) {
	h := hasherPool.Get().(hash.Hash)
	h.Reset()

	// Write method and URI as raw bytes (zero-copy via unsafe).
	h.Write(unsafe.Slice(unsafe.StringData(f.Method), len(f.Method))) // #nosec G103 G104 -- intentional zero-copy; hash.Write never errors
	h.Write(unsafe.Slice(unsafe.StringData(f.URI), len(f.URI)))       // #nosec G103 G104
	h.Write(f.Body)                                                   // #nosec G104

	var digest [sha256Len]byte
	h.Sum(digest[:0])
	hex.Encode(dst, digest[:])

	hasherPool.Put(h)
}
