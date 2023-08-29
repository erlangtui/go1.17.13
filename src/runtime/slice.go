// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

type slice struct {
	array unsafe.Pointer // 指针，其值为实际存储数据的数组首地址
	len   int            // 长度，该切片实际存储数据的长度
	cap   int            // 容量，该切片在不扩容的情况下能够存储的数据长度
}

// A notInHeapSlice is a slice backed by go:notinheap memory.
type notInHeapSlice struct {
	array *notInHeap
	len   int
	cap   int
}

func panicmakeslicelen() {
	panic(errorString("makeslice: len out of range"))
}

func panicmakeslicecap() {
	panic(errorString("makeslice: cap out of range"))
}

// makeslicecopy allocates a slice of "tolen" elements of type "et",
// then copies "fromlen" elements of type "et" into that new allocation from "from".
func makeslicecopy(et *_type, tolen int, fromlen int, from unsafe.Pointer) unsafe.Pointer {
	var tomem, copymem uintptr
	if uintptr(tolen) > uintptr(fromlen) {
		var overflow bool
		tomem, overflow = math.MulUintptr(et.size, uintptr(tolen))
		if overflow || tomem > maxAlloc || tolen < 0 {
			panicmakeslicelen()
		}
		copymem = et.size * uintptr(fromlen)
	} else {
		// fromlen is a known good length providing and equal or greater than tolen,
		// thereby making tolen a good slice length too as from and to slices have the
		// same element width.
		tomem = et.size * uintptr(tolen)
		copymem = tomem
	}

	var to unsafe.Pointer
	if et.ptrdata == 0 {
		to = mallocgc(tomem, nil, false)
		if copymem < tomem {
			memclrNoHeapPointers(add(to, copymem), tomem-copymem)
		}
	} else {
		// Note: can't use rawmem (which avoids zeroing of memory), because then GC can scan uninitialized memory.
		to = mallocgc(tomem, et, true)
		if copymem > 0 && writeBarrier.enabled {
			// Only shade the pointers in old.array since we know the destination slice to
			// only contains nil pointers because it has been cleared during alloc.
			bulkBarrierPreWriteSrcOnly(uintptr(to), uintptr(from), copymem)
		}
	}

	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(makeslicecopy)
		racereadrangepc(from, copymem, callerpc, pc)
	}
	if msanenabled {
		msanread(from, copymem)
	}

	memmove(to, from, copymem)

	return to
}

func makeslice(et *_type, len, cap int) unsafe.Pointer {
	// Slice 的元素大小乘以容量
	mem, overflow := math.MulUintptr(et.size, uintptr(cap))
	if overflow || mem > maxAlloc || len < 0 || len > cap {
		// 判断是否需要分配的内存地址超过最大值、内存超过了最大分配内存等
		mem, overflow := math.MulUintptr(et.size, uintptr(len))
		if overflow || mem > maxAlloc || len < 0 {
			panicmakeslicelen()
		}
		panicmakeslicecap()
	}

	return mallocgc(mem, et, true)
}

func makeslice64(et *_type, len64, cap64 int64) unsafe.Pointer {
	len := int(len64)
	if int64(len) != len64 {
		panicmakeslicelen()
	}

	cap := int(cap64)
	if int64(cap) != cap64 {
		panicmakeslicecap()
	}

	return makeslice(et, len, cap)
}

func unsafeslice(et *_type, ptr unsafe.Pointer, len int) {
	if len == 0 {
		return
	}

	if ptr == nil {
		panic(errorString("unsafe.Slice: ptr is nil and len is not zero"))
	}

	mem, overflow := math.MulUintptr(et.size, uintptr(len))
	if overflow || mem > maxAlloc || len < 0 {
		panicunsafeslicelen()
	}
}

func unsafeslice64(et *_type, ptr unsafe.Pointer, len64 int64) {
	len := int(len64)
	if int64(len) != len64 {
		panicunsafeslicelen()
	}
	unsafeslice(et, ptr, len)
}

func unsafeslicecheckptr(et *_type, ptr unsafe.Pointer, len64 int64) {
	unsafeslice64(et, ptr, len64)

	// Check that underlying array doesn't straddle multiple heap objects.
	// unsafeslice64 has already checked for overflow.
	if checkptrStraddles(ptr, uintptr(len64)*et.size) {
		throw("checkptr: unsafe.Slice result straddles multiple allocations")
	}
}

func panicunsafeslicelen() {
	panic(errorString("unsafe.Slice: len out of range"))
}

// growslice handles slice growth during append.
// It is passed the slice element type, the old slice, and the desired new minimum capacity,
// and it returns a new slice with at least that capacity, with the old data
// copied into it.
// The new slice's length is set to the old slice's length,
// NOT to the new requested capacity.
// This is for codegen convenience. The old slice's length is used immediately
// to calculate where to write new values during an append.
// TODO: When the old backend is gone, reconsider this decision.
// The SSA backend might prefer the new length or to return only ptr/cap and save stack space.
// Growslice 在追加期间处理切片增长。它传递切片元素类型、旧切片和所需的新最小容量，并返回至少具有该容量的新切片，并将旧数据复制到其中。
// 新切片的长度设置为旧切片的长度，而不是新请求的容量。这是为了代码生成方便。旧切片的长度将立即用于计算追加期间写入新值的位置。
// TODO：当旧的后端消失时，重新考虑这个决定。SSA 后端可能更喜欢新的长度，或者只返回 ptrcap 并节省堆栈空间。
// 元素类型、旧切片、新切片的最小容量(也即新切片的长度)
func growslice(et *_type, old slice, cap int) slice {
	if raceenabled {
		callerpc := getcallerpc()
		racereadrangepc(old.array, uintptr(old.len*int(et.size)), callerpc, funcPC(growslice))
	}
	if msanenabled {
		msanread(old.array, uintptr(old.len*int(et.size)))
	}

	// 新的容量小于老的容量时，直接panic
	if cap < old.cap {
		panic(errorString("growslice: cap out of range"))
	}

	// 元素大小为 0 时，直接返回一个 nil 切片
	if et.size == 0 {
		// 追加不应创建具有 nil 指针但非零长度的切片
		// 假设在这种情况下，append 不需要保留 old.array。
		return slice{unsafe.Pointer(&zerobase), old.len, cap}
	}

	newcap := old.cap
	doublecap := newcap + newcap
	if cap > doublecap {
		// 如果需要的容量大于旧容量的两倍，则新容量直接为该容量
		newcap = cap
	} else {
		// 需要的容量小于或等于旧容量的两倍
		if old.cap < 1024 {
			// 旧容量小于 1024 时，则新容量直接为旧容量的两倍
			newcap = doublecap
		} else {
			// 旧容量大于或等于1024
			for 0 < newcap && newcap < cap {
				// 直接对旧容量连续多次 1.25 倍进行扩容，直至大于需要的容量
				newcap += newcap / 4
			}
			// 如果计算出的新容量小于或等于0时，直接令其为需要的容量
			if newcap <= 0 {
				newcap = cap
			}
		}
	}

	// 计算出是否内存溢出、旧长度的内存大小、新长度的内存大小、新容量的内存大小、分配内存后的新容量
	var overflow bool
	var lenmem, newlenmem, capmem uintptr
	switch {
	case et.size == 1: // 对于 1，不需要任何除法乘法
		lenmem = uintptr(old.len)
		newlenmem = uintptr(cap)
		capmem = roundupsize(uintptr(newcap)) // 返回 mallocgc 将在请求大小时分配的内存块的大小
		overflow = uintptr(newcap) > maxAlloc
		newcap = int(capmem)
	case et.size == sys.PtrSize: // 对于PtrSize，编译器将优化除法乘法为一个常数的移位
		lenmem = uintptr(old.len) * sys.PtrSize
		newlenmem = uintptr(cap) * sys.PtrSize
		capmem = roundupsize(uintptr(newcap) * sys.PtrSize)
		overflow = uintptr(newcap) > maxAlloc/sys.PtrSize
		// 在内存分配、对齐后，实际分配的内存大小存储的元素可能大于 newcap，所以此处需要重新赋值
		newcap = int(capmem / sys.PtrSize)
	case isPowerOfTwo(et.size): // 对于 2 的幂，直接使用位运算
		var shift uintptr
		if sys.PtrSize == 8 {
			// 计算出 et.size 的值用二进制表示时，最右侧 1 的位置
			shift = uintptr(sys.Ctz64(uint64(et.size))) & 63
		} else {
			shift = uintptr(sys.Ctz32(uint32(et.size))) & 31
		}
		lenmem = uintptr(old.len) << shift
		newlenmem = uintptr(cap) << shift
		capmem = roundupsize(uintptr(newcap) << shift)
		overflow = uintptr(newcap) > (maxAlloc >> shift)
		newcap = int(capmem >> shift)
	default: // 对于其他长度，直接使用乘除法
		lenmem = uintptr(old.len) * et.size
		newlenmem = uintptr(cap) * et.size
		capmem, overflow = math.MulUintptr(et.size, uintptr(newcap))
		capmem = roundupsize(capmem)
		newcap = int(capmem / et.size)
	}

	// The check of overflow in addition to capmem > maxAlloc is needed
	// to prevent an overflow which can be used to trigger a segfault
	// on 32bit architectures with this example program:
	//
	// type T [1<<27 + 1]int64
	//
	// var d T
	// var s []T
	//
	// func main() {
	//   s = append(s, d, d, d, d)
	//   print(len(s), "\n")
	// }
	if overflow || capmem > maxAlloc {
		// 有内存溢出直接 panic
		panic(errorString("growslice: cap out of range"))
	}

	var p unsafe.Pointer
	if et.ptrdata == 0 { // 元素类型中没有指针
		p = mallocgc(capmem, nil, false)
		// 分配 capmem 内存，并清除从 newlenmem 到 capmem 的内容
		memclrNoHeapPointers(add(p, newlenmem), capmem-newlenmem)
	} else {
		// 注意：不能使用 rawmem（这样可以避免内存归零），因为这样 GC 可以扫描未初始化的内存。
		p = mallocgc(capmem, et, true)
		if lenmem > 0 && writeBarrier.enabled {
			// 只在 old.array 中对指针进行着色，因为目标切片 p 只包含 nil 指针，它在分配期间已被清除。
			bulkBarrierPreWriteSrcOnly(uintptr(p), uintptr(old.array), lenmem-et.size+et.ptrdata)
		}
	}
	// 内存迁移，从旧的数组迁移到新的数组
	memmove(p, old.array, lenmem)

	return slice{p, old.len, newcap}
}

// 通过位的运算符判断一个无符号整数 x 是否为2的幂
func isPowerOfTwo(x uintptr) bool {
	return x&(x-1) == 0
}

// slicecopy is used to copy from a string or slice of pointerless elements into a slice.
func slicecopy(toPtr unsafe.Pointer, toLen int, fromPtr unsafe.Pointer, fromLen int, width uintptr) int {
	if fromLen == 0 || toLen == 0 {
		return 0
	}

	n := fromLen
	if toLen < n {
		n = toLen
	}

	if width == 0 {
		return n
	}

	size := uintptr(n) * width
	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(slicecopy)
		racereadrangepc(fromPtr, size, callerpc, pc)
		racewriterangepc(toPtr, size, callerpc, pc)
	}
	if msanenabled {
		msanread(fromPtr, size)
		msanwrite(toPtr, size)
	}

	if size == 1 { // common case worth about 2x to do here
		// TODO: is this still worth it with new memmove impl?
		*(*byte)(toPtr) = *(*byte)(fromPtr) // known to be a byte pointer
	} else {
		memmove(toPtr, fromPtr, size)
	}
	return n
}
