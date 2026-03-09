// Package fingerprint derives a deterministic SHA-256 key from the raw
// properties of an HTTP request. The key is used as a Redis SETNX key to
// detect duplicates.
//
// Fingerprint inputs (in hash order):
//  1. HTTP method        — r.Method, exactly as received
//  2. Request URI+query  — r.RequestURI, exactly as received
//  3. Request body       — raw bytes up to MaxBodyBytes; remainder discarded
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

// MaxBodyBytes is the maximum number of body bytes read for hashing.
// Bodies larger than this are truncated; the remainder is discarded.
// 64 KB covers the vast majority of API payloads without excessive memory use.
const MaxBodyBytes = 65536

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
	// bodyPool recycles 64 KB body read buffers.
	bodyPool = sync.Pool{
		New: func() any {
			b := make([]byte, MaxBodyBytes)
			return &b
		},
	}
)

// Request holds the raw inputs used to compute the fingerprint.
// Fields are taken verbatim from the incoming HTTP request with no
// normalisation, case folding, or whitespace trimming.
type Request struct {
	Method string // r.Method — e.g. "POST"
	URI    string // r.RequestURI — e.g. "/api/orders?ref=abc"
	Body   []byte // raw body bytes, capped at MaxBodyBytes
}

// FromHTTP constructs a Request directly from r without any normalisation.
// Nginx forwards the original client request as-is via proxy_pass.
//
// r.Body is read up to MaxBodyBytes. The caller must not attempt to
// re-read r.Body after this call.
func FromHTTP(r *http.Request) (*Request, error) {
	fp := &Request{
		Method: r.Method,
		URI:    r.RequestURI,
	}

	if r.Body != nil && r.ContentLength != 0 {
		// Borrow a pooled buffer and read into it instead of using io.ReadAll
		// which dynamically grows its backing slice.
		bufp := bodyPool.Get().(*[]byte)
		buf := *bufp
		n, err := io.ReadFull(r.Body, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			bodyPool.Put(bufp)
			return nil, err
		}
		// Copy only the bytes actually read so we can return the pooled buffer.
		fp.Body = make([]byte, n)
		copy(fp.Body, buf[:n])
		bodyPool.Put(bufp)
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

// GetBodyBuf returns a pooled byte buffer of MaxBodyBytes.
// The caller must call PutBodyBuf when done.
func GetBodyBuf() *[]byte { return bodyPool.Get().(*[]byte) }

// PutBodyBuf returns a buffer obtained from GetBodyBuf to the pool.
func PutBodyBuf(b *[]byte) { bodyPool.Put(b) }

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
