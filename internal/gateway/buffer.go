package gateway

import "sync"

const proxyBufferSize = 32 << 10

// proxyBuffers reuses streaming copy buffers. sync.Pool may discard them at GC;
// it is an allocation optimization, not part of the cache's retained-byte budget.
type proxyBuffers struct{ pool sync.Pool }

func (p *proxyBuffers) Get() []byte {
	if value := p.pool.Get(); value != nil {
		return value.([]byte)
	}
	return make([]byte, proxyBufferSize)
}
func (p *proxyBuffers) Put(buffer []byte) {
	if cap(buffer) == proxyBufferSize {
		p.pool.Put(buffer[:proxyBufferSize])
	}
}
