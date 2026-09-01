package proxy

import (
	"crypto/rand"
	"encoding/binary"
	"sync"

	"github.com/tokaco/pg-k8s-proxy/internal/pgwire"
	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

// cancelEntry remembers where a session lives so a later CancelRequest, which
// arrives on a brand new connection carrying no database name, can be routed.
type cancelEntry struct {
	// backendAddr is the address the session was proxied to.
	backendAddr string
	// backendKey is the key the backend itself issued.
	backendKey pgwire.CancelRequest
	// tls is the backend TLS policy the session used, so the cancel
	// connection is established the same way the session was.
	tls registry.TLSConfig
}

// cancelRegistry maps the keys the proxy hands to clients onto the real backend
// keys. The proxy substitutes its own key so that identical process IDs from two
// different backends cannot collide.
type cancelRegistry struct {
	mu      sync.RWMutex
	entries map[pgwire.CancelRequest]cancelEntry
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{entries: make(map[pgwire.CancelRequest]cancelEntry)}
}

// issue mints a unique key for a session and records how to reach its backend.
func (r *cancelRegistry) issue(entry cancelEntry) pgwire.CancelRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		key := randomCancelKey()
		if _, taken := r.entries[key]; taken {
			continue
		}
		r.entries[key] = entry
		return key
	}
}

// lookup returns the backend behind a proxy-issued key.
func (r *cancelRegistry) lookup(key pgwire.CancelRequest) (cancelEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[key]
	return entry, ok
}

// release forgets a key once its session ends.
func (r *cancelRegistry) release(key pgwire.CancelRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

func (r *cancelRegistry) len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// randomCancelKey draws a key from a CSPRNG. The secret is what stops an
// unrelated client from cancelling somebody else's query, so it must not be
// guessable; the process ID half is random for the same reason.
func randomCancelKey() pgwire.CancelRequest {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand.Read never returns an error on any supported platform;
		// if that ever changes, failing loudly beats issuing a guessable key.
		panic("pgproxy: crypto/rand is unavailable: " + err.Error())
	}
	return pgwire.CancelRequest{
		// Clear the sign bit: some clients store the process ID as a positive
		// integer and mangle negative values.
		ProcessID: int32(binary.BigEndian.Uint32(buf[0:4]) & 0x7FFFFFFF),
		SecretKey: int32(binary.BigEndian.Uint32(buf[4:8])),
	}
}
