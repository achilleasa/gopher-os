package physical

import (
	"math"
	"reflect"
	"unsafe"

	"github.com/achilleasa/gopher-os/kernel/errors"
	"github.com/achilleasa/gopher-os/kernel/hal/multiboot"
	"github.com/achilleasa/gopher-os/kernel/mem"
)

// Flag defines the flags that can be passed to AllocatePage.
type Flag uint16

const (
	// FlagKernel requests a page to be used inside kernel code. The contents
	// of the page will be cleared before it is returned.
	FlagKernel Flag = FlagClear

	// FlagClear instructs the allocator to clear the page contents.
	FlagClear = 1 << iota

	// FlagDoNotClear instructs the allocator not to clear the page contents.
	FlagDoNotClear
)

type reservationMode uint8

const (
	markFree     reservationMode = 0
	markReserved                 = 1
)

var (
	// PageAllocator is an allocator instance that deals with page-based allocations.
	PageAllocator buddyAllocator

	// Overriden by tests
	memsetFn         = mem.Memset
	visitMemRegionFn = multiboot.VisitMemRegions

	// ErrPageNotAllocated is returned when trying to free a page not marked by the allocator as reserved.
	ErrPageNotAllocated = errors.KernelError("attempted to free non-allocated page")
)

type buddyAllocator struct {
	// freeCount stores the number of free pages for each allocation order.
	// Initially, only the last order contains free pages. Having a free
	// counter allows us to quickly detect when the lower orders have no
	// pages available so we can immediately start scanning the higher orders.
	freeCount [mem.MaxPageOrder + 1]uint32

	// freeBitmap stores the free page bitmap data for each allocation order.
	// The bitmap for each order is stored as a []uint64. This allows us
	// to quickly traverse the bitmap when we search for a page to allocate
	// by examining 64 pages at a time (using bitwise ANDs) and only scan
	// individual bits when we are sure that one of the blocks contains a
	// free page.
	freeBitmap [mem.MaxPageOrder + 1][]uint64

	// bitmapSlice stores the slice structures for the freeBitmap entries.
	// It allows us to perform 2 passes to allocate their content. The first
	// pass populates their Len and Cap values with the number of required bits.
	// After calculating the total required bits for all bitmaps we perform a
	// second pass where we scan the available memory blocks looking for a
	// block that can fit all bitmaps and adjust the slice Data pointers
	// accordingly.
	bitmapSlice [mem.MaxPageOrder + 1]reflect.SliceHeader
}

// Init bootstraps the physical page allocator. The initialization sequence
// consists of 3 phases:
//
// The available memory is converted into pages and then the allocator estimates
// the required space for storing the free page bitmaps for those pages. The
// allocator then scans through the available memory blocks looking for the first
// available free region that is large enough to store the bitmap data.
//
// Once a large-enough memory block has been found, the allocator will setup the
// bitmap slices so that their contents are mapped to the selected memory block.
//
// Finally, the allocator will perform another pass where all memory regions are
// examined and the appropriate bitmaps are marked as free or reserved.
//
// If the allocator cannot find a free memory block for storing its bitmaps, an
// error will be returned.
func (alloc *buddyAllocator) Init(totalMemory mem.Size) error {
	var alignment = 8 * mem.Byte

	alloc.setBitmapSizes(totalMemory.Pages())

	// Each slice entry is a uint64 and takes 8 bytes of space
	var requiredSpace uint64
	for _, slice := range alloc.bitmapSlice {
		requiredSpace += uint64(slice.Len << 3)
	}

	// Find a block large enough to hold the bitmap data
	var foundRegion bool
	var alignedBitmapAddr uint64
	visitMemRegionFn(func(entry *multiboot.MemoryMapEntry) {
		if foundRegion || entry.Type != multiboot.MemAvailable {
			return
		}

		// Our bitmap data needs to be aligned to a qword boundary; when
		// we align the physAddr, the actual available region length may
		// decrease so we need to take that into account
		alignedAddr := mem.Align(entry.PhysAddress, alignment)
		if entry.Length-(alignedAddr-entry.PhysAddress) < requiredSpace {
			return
		}

		foundRegion = true
		alignedBitmapAddr = alignedAddr
	})

	if !foundRegion {
		return mem.ErrOutOfMemory
	}

	// Overlay the bitmaps to the selected region
	alloc.setBitmapPointers(uintptr(alignedBitmapAddr))

	// Mark all bitmaps as reseved
	mem.Memset(uintptr(alignedBitmapAddr), 0xFF, uint32(requiredSpace))

	// Scan all free memory regions marking their MaxPageOrder block as available
	maxOrderPageSize := uint64(mem.PageSize << mem.MaxPageOrder)
	visitMemRegionFn(func(entry *multiboot.MemoryMapEntry) {
		if entry.Type != multiboot.MemAvailable {
			return
		}

		// Align physical address and decrease its available length if this
		// is where we store our bitmap data.
		alignedAddr := mem.Align(entry.PhysAddress, alignment)
		if alignedAddr == alignedBitmapAddr {
			alignedAddr = mem.Align(alignedAddr+requiredSpace, alignment)
		}
		regionLen := entry.Length - (alignedAddr - entry.PhysAddress)

		pageBlocks := regionLen / maxOrderPageSize
		for index := uint64(0); index < pageBlocks; index++ {
			blockAddr := alignedAddr + (index * maxOrderPageSize)
			bitIndex := bitmapIndex(uintptr(blockAddr), mem.MaxPageOrder)
			block := bitIndex >> 6
			blockOffset := bitIndex & 63

			alloc.freeBitmap[mem.MaxPageOrder][block] &^= (1 << (63 - blockOffset))
			alloc.freeCount[mem.MaxPageOrder]++
		}
	})

	return nil
}

// AllocatePage allocates a page with the given size (order) and returns back
// its address or an error if no free pages are available.
func (alloc *buddyAllocator) AllocatePage(order mem.PageOrder, flags Flag) (uintptr, error) {
	// Sanity checks
	if order > mem.MaxPageOrder {
		return uintptr(0), errors.ErrInvalidParamValue
	}

	// If no pages are free at the requested order we may need to split a
	// higher order page to make some room.
	if alloc.freeCount[order] == 0 {
		err := alloc.splitHigherOrderPage(order)
		if err != nil {
			return uintptr(0), err
		}
	}

	// Since we are guaranteed to find a free page this call can never fail
	addr, _ := alloc.reserveFreePage(order)

	alloc.updateLowerOrderBitmaps(addr, order, markReserved)
	alloc.updateHigherOrderBitmaps(addr, order)

	if (flags & (FlagClear | FlagDoNotClear)) == FlagClear {
		memsetFn(addr, 0, uint32(mem.PageSize)<<order)
	}

	return addr, nil
}

// FreePage releases a previously allocated page with the given size/order.
func (alloc *buddyAllocator) FreePage(addr uintptr, order mem.PageOrder) error {
	// Sanity checks
	if order > mem.MaxPageOrder {
		return errors.ErrInvalidParamValue
	}

	bitIndex := bitmapIndex(addr, order)
	block := bitIndex >> 6
	mask := uint64(1 << (63 - (bitIndex & 63)))
	if alloc.freeBitmap[order][block]&mask != mask {
		return ErrPageNotAllocated
	}

	// Clear the allocated bit and increase free count for this order
	alloc.freeBitmap[order][block] &^= mask
	alloc.freeCount[order]++

	// Propagate the changes to the other orders
	alloc.updateLowerOrderBitmaps(addr, order, markFree)
	alloc.updateHigherOrderBitmaps(addr, order)

	return nil
}

// splitHigherOrderPage searches for the first available page with order greater
// than the requested order. If a free page is found, it is marked as reserved and
// the free counts for the orders below it are updated accordingly.
func (alloc *buddyAllocator) splitHigherOrderPage(order mem.PageOrder) error {
	for order = order + 1; order <= mem.MaxPageOrder; order++ {
		if alloc.freeCount[order] == 0 {
			continue
		}

		// This order has free pages. Reserve the first available and
		// make its space available to the order below it
		alloc.reserveFreePage(order)
		alloc.incFreeCountForLowerOrders(order)
		return nil
	}

	return mem.ErrOutOfMemory
}

// reserveFreePage scans the free page bitmaps for the given order, reserves the
// first available page and returns its address. If no pages at this order are
// available then this method returns ErrOutOfMemory.
func (alloc *buddyAllocator) reserveFreePage(order mem.PageOrder) (uintptr, error) {
	if order > mem.MaxPageOrder {
		return uintptr(0), errors.ErrInvalidParamValue
	}

	for blockIndex, block := range alloc.freeBitmap[order] {
		// Entire block is allocated; skip it
		if block == math.MaxUint64 {
			continue
		}

		// Scan the individual bits to find the block and reserve it
		for bitIndex := uint8(0); bitIndex < 64; bitIndex++ {
			mask := uint64(1 << (63 - bitIndex))

			// Ignore used bits
			if (block & mask) != 0 {
				continue
			}

			// Mark page as allocated and decrement the free page count for this order
			alloc.freeBitmap[order][blockIndex] |= mask
			alloc.freeCount[order]--

			return uintptr(mem.PageSize) * ((uintptr(blockIndex) << 6) + uintptr(bitIndex)), nil
		}
	}

	return uintptr(0), mem.ErrOutOfMemory
}

// updateLowerOrderBitmaps hierarchically traverses the free bitmaps at the orders
// below the supplied order and depending on the requested reservation mode either
// sets or unsets the used bits that correspond to the supplied address.
func (alloc *buddyAllocator) updateLowerOrderBitmaps(addr uintptr, order mem.PageOrder, mode reservationMode) {
	order--

	var (
		firstBitIndex                     = bitmapIndex(addr, order)
		totalBitCount              uint32 = 2
		bitsToChange, lastBitIndex uint32
	)

	for ; order >= 0 && order <= mem.MaxPageOrder; order = order - 1 {
		lastBitIndex = firstBitIndex + totalBitCount
		for bitIndex := firstBitIndex; bitIndex < lastBitIndex; bitIndex += bitsToChange {
			block := bitIndex >> 6
			blockOffset := bitIndex & 63

			// We need to change min(64, lastBitIndex - bitIndex) bits in this block
			bitsToChange = lastBitIndex - bitIndex
			if bitsToChange > 64 {
				bitsToChange = 64
			}

			// To build the block mask we start with a value with the
			// bitsToChange LSB set and shift it right so it alignts with
			// the offset position in the block
			blockMask := uint64(((1 << (bitsToChange)) - 1) << (64 - blockOffset - bitsToChange))

			// Mark either as reserved (set to 1) or free (set to 0)
			if mode == markReserved {
				alloc.freeBitmap[order][block] |= blockMask
			} else {
				alloc.freeBitmap[order][block] &^= blockMask
			}
		}

		switch {
		// Initially only the MaxPageOrder free count is > 0; all lower-order free counts are 0.
		// If we directly allocate a MaxPageOrder page, this can cause an underflow
		case mode == markReserved && alloc.freeCount[order] >= totalBitCount:
			alloc.freeCount[order] -= totalBitCount
		case mode == markFree:
			alloc.freeCount[order] += totalBitCount
		}

		// Each time we descend an order the first bit index and the number
		// of bits we need to set/unset doubles
		firstBitIndex <<= 1
		totalBitCount <<= 1
	}
}

// updateHigherOrderBitmaps hierarchically traverses the free bitmaps from lower
// to higher orders and for each order, updates the page bit that corresponds
// to the supplied physical address based on the value of the 2 buddy pages of
// the order below it. The status of page at ord(N) is set to the OR-ed value
// of the 2 buddy pages at ord(N-1).
func (alloc *buddyAllocator) updateHigherOrderBitmaps(addr uintptr, order mem.PageOrder) {
	// sanity checks
	if order > mem.MaxPageOrder {
		return
	}

	// ord(0) has no children
	if order == 0 {
		order++
	}

	var bitIndex, block, childBitIndex, childBlock uint32
	var blockMask, childBlockMask uint64
	var wasReserved bool
	for bitIndex = bitmapIndex(addr, order); order <= mem.MaxPageOrder; order, bitIndex = order+1, bitIndex>>1 {
		block = bitIndex >> 6
		blockMask = 1 << (63 - (bitIndex & 63))
		wasReserved = (alloc.freeBitmap[order][block] & blockMask) == blockMask

		// This bit should be marked as used any of the (ord-1) bits:
		// (2*bit)+1 or (2*bit)+2 are marked as used. The child mask
		// that includes these bits is calculated by shifting the
		// value "3" (11b) left childBitIndex positions.
		childBitIndex = (bitIndex << 1) + 1
		childBlock = childBitIndex >> 6
		childBlockMask = 3 << (63 - (childBitIndex & 63))

		switch alloc.freeBitmap[order-1][childBlock] & childBlockMask {
		case 0: // both bits are not set; we just need to clear the bit
			alloc.freeBitmap[order][block] &^= blockMask

			if wasReserved {
				alloc.freeCount[order]++
			}
		default: // one or both bits are set; we just need to set the bit
			alloc.freeBitmap[order][block] |= blockMask

			if !wasReserved {
				alloc.freeCount[order]--
			}
		}
	}
}

// incFreeCountForLowerOrders is called when a free page at ord(N) is allocated
// to update all free page counters for all orders less than or equal to N. The
// number of free pages that are added to the counters doubles for each order less than N.
func (alloc *buddyAllocator) incFreeCountForLowerOrders(order mem.PageOrder) {
	// sanity check
	if order > mem.MaxPageOrder {
		return
	}

	// When ord reaches 0; ord - 1 will wrap to MaxUint32 so we need to check for that as well
	freeCount := uint32(2)
	for order = order - 1; order >= 0 && order <= mem.MaxPageOrder; order, freeCount = order-1, freeCount<<1 {
		alloc.freeCount[order] += freeCount
	}
}

// setBitmapSizes updates the Len and Cap fields of the allocator's bitmap slice
// headers to the required number of bits for each allocation order.
//
// Given N pages of size mem.Pagemem.PageOrder:
// the bitmap for order(0) uses align(N, 64) bits, one for each block with size (mem.Pagemem.PageOrder << 0)
// the bitmap for order(M) uses ceil(N / M) bits, one for each block with size (mem.Pagemem.PageOrder << M)
//
// Since we use []uint64 for our bitmap entries, this method will pad the required
// number of bits per order so they are multiples of 64.
func (alloc *buddyAllocator) setBitmapSizes(pageCount uint32) {
	for order := mem.PageOrder(0); order <= mem.MaxPageOrder; order++ {
		requiredUint64 := requiredUint64(pageCount, order)
		alloc.bitmapSlice[order].Cap, alloc.bitmapSlice[order].Len = requiredUint64, requiredUint64
	}
}

// setBitmapPointers updates the Data field for the allocator's bitmap slice
// headers so that each slice's data begins at a 8-byte aligned offset after the
// provided baseAddr value.
//
// This method also patches the freeBitmap slice entries so that they point to the
// populated slice header structs.
//
// After a call to setBitmapPointers, the allocator will be able to freely access
// all freeBitmap entries.
func (alloc *buddyAllocator) setBitmapPointers(baseAddr uintptr) {
	var dataPtr = baseAddr
	for ord := mem.PageOrder(0); ord <= mem.MaxPageOrder; ord++ {
		alloc.bitmapSlice[ord].Data = dataPtr
		alloc.freeBitmap[ord] = *(*[]uint64)(unsafe.Pointer(&alloc.bitmapSlice[ord]))

		// offset += ordLen * 8 bytes per uint64
		dataPtr += uintptr(alloc.bitmapSlice[ord].Len << 3)
	}
}

// bitmapIndex returns the index of bit in the bitmap for the given order that
// corresponds to the page located at the given address.
func bitmapIndex(addr uintptr, order mem.PageOrder) uint32 {
	return uint32(addr >> (mem.PageShift + order))
}

// requiredUint64 returns the number of uint64 required for storing a bitmap
// of order(ord) for pageCount pages.
func requiredUint64(pageCount uint32, order mem.PageOrder) int {
	// requiredBits = pageCount / (2*ord) + pageCount % (2*ord)
	requiredBits := uint64((pageCount >> order) + (pageCount & ((1 << order) - 1)))
	return int(mem.Align(requiredBits, 64*mem.Byte) >> 6)
}
