/*
 * Copyright 2020 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package z

import (
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/dustin/go-humanize"
)

// Allocator amortizes the cost of small allocations by allocating memory in bigger chunks. It uses
// Go memory to make these allocations because when using Calloc and storing Go pointers in there,
// we see strange crashes -- we suspect these to be some sort of bug in how Go interprets memory.
// Once allocated, the memory is not moved, so it is safe to use the allocated bytes to unsafe cast
// them to Go struct pointers. Maintaining a freelist is slow.  Instead, Allocator only allocates
// memory, with the idea that finally we would just release the entire Allocator.
type Allocator struct {
	sync.Mutex
	compIdx uint64 // Stores bufIdx in 32 MSBs and posIdx in 32 LSBs.
	buffers [][]byte
	Ref     uint64
	Tag     string
}

// allocs keeps references to all Allocators, so we can safely discard them later.
var allocsMu *sync.Mutex
var allocRef uint64
var allocs map[uint64]*Allocator
var calculatedLog2 []int

func init() {
	allocsMu = new(sync.Mutex)
	allocs = make(map[uint64]*Allocator)

	// Set up a unique Ref per process.
	rand.Seed(time.Now().UnixNano())
	allocRef = uint64(rand.Int63n(1<<16)) << 48

	calculatedLog2 = make([]int, 1025)
	for i := 1; i <= 1024; i++ {
		calculatedLog2[i] = int(math.Log2(float64(i)))
	}
}

// NewAllocator creates an allocator starting with the given size.
func NewAllocator(sz int) *Allocator {
	ref := atomic.AddUint64(&allocRef, 1)
	// We should not allow a zero sized page because addBufferWithMinSize
	// will run into an infinite loop trying to double the pagesize.
	if sz <= 0 {
		sz = 512
	}
	a := &Allocator{
		Ref:     ref,
		buffers: make([][]byte, 32),
	}

	l2 := uint64(log2(sz))
	if bits.OnesCount64(uint64(sz)) > 1 {
		// If l2 is a power of 2, then we can allocate the requested size of data. Otherwise, we
		// bump up to the next power of 2.
		l2 += 1
	}
	a.buffers[0] = make([]byte, 1<<l2)

	allocsMu.Lock()
	allocs[ref] = a
	allocsMu.Unlock()
	return a
}

func (a *Allocator) Reset() {
	atomic.StoreUint64(&a.compIdx, 0)
}

func PrintAllocators() {
	allocsMu.Lock()
	tags := make(map[string]uint64)
	for _, ac := range allocs {
		tags[ac.Tag] += ac.Allocated()
	}
	for tag, sz := range tags {
		fmt.Printf("Allocator Tag: %s Size: %s\n", tag, humanize.IBytes(sz))
	}
	allocsMu.Unlock()
}

func (a *Allocator) String() string {
	var s strings.Builder
	s.WriteString(fmt.Sprintf("Allocator: %x\n", a.Ref))
	var cum int
	for i, b := range a.buffers {
		cum += len(b)
		if len(b) == 0 {
			break
		}
		s.WriteString(fmt.Sprintf("idx: %d len: %d cum: %d\n", i, len(b), cum))
	}
	pos := atomic.LoadUint64(&a.compIdx)
	bi, pi := parse(pos)
	s.WriteString(fmt.Sprintf("bi: %d pi: %d\n", bi, pi))
	s.WriteString(fmt.Sprintf("Size: %d\n", a.Size()))
	return s.String()
}

// AllocatorFrom would return the allocator corresponding to the ref.
func AllocatorFrom(ref uint64) *Allocator {
	allocsMu.Lock()
	a := allocs[ref]
	allocsMu.Unlock()
	return a
}

func parse(pos uint64) (bufIdx, posIdx int) {
	return int(pos >> 32), int(pos & 0xFFFFFFFF)
}

// Size returns the size of the allocations so far.
func (a *Allocator) Size() int {
	pos := atomic.LoadUint64(&a.compIdx)
	bi, pi := parse(pos)
	var sz int
	for i, b := range a.buffers {
		if i < bi {
			sz += len(b)
			continue
		}
		sz += pi
		return sz
	}
	panic("Size should not reach here")
}

func log2(sz int) int {
	if sz < len(calculatedLog2) {
		return calculatedLog2[sz]
	}
	pow := 10
	sz >>= 10
	for sz > 1 {
		sz >>= 1
		pow++
	}
	return pow
}

func (a *Allocator) Allocated() uint64 {
	var alloc int
	for _, b := range a.buffers {
		alloc += cap(b)
	}
	return uint64(alloc)
}

// Release would release the memory back. Remember to make this call to avoid memory leaks.
func (a *Allocator) Release() {
	if a == nil {
		return
	}

	allocsMu.Lock()
	delete(allocs, a.Ref)
	allocsMu.Unlock()
}

const maxAlloc = 1 << 30

func (a *Allocator) MaxAlloc() int {
	return maxAlloc
}

const nodeAlign = unsafe.Sizeof(uint64(0)) - 1

func (a *Allocator) AllocateAligned(sz int) []byte {
	tsz := sz + int(nodeAlign)
	out := a.Allocate(tsz)

	addr := uintptr(unsafe.Pointer(&out[0]))
	aligned := (addr + nodeAlign) & ^nodeAlign
	start := int(aligned - addr)

	return out[start : start+sz]
}

func (a *Allocator) Copy(buf []byte) []byte {
	if a == nil {
		return append([]byte{}, buf...)
	}
	out := a.Allocate(len(buf))
	copy(out, buf)
	return out
}

func (a *Allocator) addBufferAt(bufIdx, minSz int) {
	for {
		if bufIdx >= len(a.buffers) {
			panic(fmt.Sprintf("Allocator can not allocate more than %d buffers", len(a.buffers)))
		}
		if len(a.buffers[bufIdx]) == 0 {
			break
		}
		if minSz <= len(a.buffers[bufIdx]) {
			// No need to do anything. We already have a buffer which can satisfy minSz.
			return
		}
		bufIdx++
	}
	assert(bufIdx > 0)
	// We need to allocate a new buffer.
	// Make pageSize double of the last allocation.
	pageSize := 2 * len(a.buffers[bufIdx-1])
	// Ensure pageSize is bigger than sz.
	for pageSize < minSz {
		pageSize *= 2
	}
	// If bigger than maxAlloc, trim to maxAlloc.
	if pageSize > maxAlloc {
		pageSize = maxAlloc
	}

	buf := make([]byte, pageSize)
	assert(len(a.buffers[bufIdx]) == 0)
	a.buffers[bufIdx] = buf
}

func (a *Allocator) Allocate(sz int) []byte {
	if a == nil {
		return make([]byte, sz)
	}
	if sz > maxAlloc {
		panic(fmt.Sprintf("Unable to allocate more than %d\n", maxAlloc))
	}
	if sz == 0 {
		return nil
	}
	for {
		pos := atomic.AddUint64(&a.compIdx, uint64(sz))
		bufIdx, posIdx := parse(pos)
		buf := a.buffers[bufIdx]
		if posIdx > len(buf) {
			a.Lock()
			newPos := atomic.LoadUint64(&a.compIdx)
			newBufIdx, _ := parse(newPos)
			if newBufIdx != bufIdx {
				a.Unlock()
				continue
			}
			a.addBufferAt(bufIdx+1, sz)
			atomic.StoreUint64(&a.compIdx, uint64((bufIdx+1)<<32))
			a.Unlock()
			// We added a new buffer. Let's acquire slice the right way by going back to the top.
			continue
		}
		data := buf[posIdx-sz : posIdx]
		return data
	}
}
