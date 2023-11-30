// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// Pool 是一组可以单独保存和检索的临时对象的集合
// 存储在池中的任何项目都可能随时自动删除，不另行通知。发生这种情况时如果池保存唯一的引用，则可能会解除分配该项目。
// 池是多线程安全的，池的目的是缓存已分配但未使用的项目以供以后重用，从而减轻垃圾回收器的压力。
// 它可以轻松构建高效、线程安全的空闲列表。但是，它并不适合所有免费列表。
// 池的适当用法是管理一组临时项目，这些项目在包的并发独立客户端之间静默共享并可能由这些临时客户端重用。
// 池提供了一种在多个客户端之间摊销分配开销的方法。
// 很好地使用池的一个示例是 fmt 包，它维护一个动态大小的临时输出缓冲区存储，当许多 goroutine 打印时缓冲区变大，静止时变小
// 另一方面，作为短期对象的一部分维护的空闲列表不适合用于池，因为在这种情况下开销不能很好地摊销，让此类对象实现自己的自由列表会更有效。
// 首次使用后不得复制池。
type Pool struct {
	// 不消耗内存仅用于静态分析的结构，保证一个对象在第一次使用后不会发生复制
	noCopy noCopy

	// 指向本地 poolLocal 切片的第一个元素，每个 P 对应一个 poolLocal，多个 goroutine 对同一个 Pool 操作时，每个运行在 P 上的 goroutine 优先取该 P 上 poolLocal 中的元素，能够减少不同 goroutine 之间的竞争，提升性能
	local unsafe.Pointer
	// 本地切片 poolLocal 的大小，一般是系统核数，除非程序中自定义了运行核数
	localSize uintptr
	/*
		Victim cache（牺牲者缓存）是一种用于提高缓存性能的缓存内存类型，临时存储从主缓存中驱逐出来的数据，它通常位于主缓存和主存储器之间。
		当主缓存发生缓存未命中时，在访问主存储器之前会检查牺牲者缓存。如果请求的数据在牺牲者缓存中找到，就认为是缓存命中，并将数据返回给处理器，而无需访问主存储器。
		作为一种优化缓存的技术，助于减少平均内存访问时间，提高整体系统性能。
		当主缓存需要用新数据替换一个缓存行时，它会将最近最少使用（LRU）的缓存行放入牺牲者缓存中，以防近期再次需要该数据。
		牺牲者缓存通常比主缓存更小，关联度更低。它的目的是捕获那些可能在不久的将来再次访问的缓存行，但由于主缓存的大小限制而无法容纳。
		通过将这些被驱逐的缓存行保留在一个单独的缓存中，系统可以减少对主存储器的访问次数，提高整体性能。
		在一轮 GC 到来时，victim 和 victimSize 会分别接管 local 和 localSize
		当从 local 中未查询到时，会进一步在 victim 中查询；
		在 GC 后冷启动时，local 中没有缓存对象，victim 中有，能够避免冷启时大量创建对象导致性能抖动，让分配对象更平滑
	*/
	victim     unsafe.Pointer
	victimSize uintptr

	// 指定一个函数，用于在 Pool 中没有对象时创建新的对象
	New func() interface{}
}

// 每一个 P 所拥有的私有对象和共享对象链表
type poolLocalInternal struct {
	private interface{} // 当前 P 私有的对象，只能由其所属的当前 P 存储和获取
	shared  poolChain   // 当前 P 与其他 P 共有双向链表，链表中存储对象，当前 P 是生产者，能够 pushHead/popHead，其他 P 是消费者，只能 popTail.
}

type poolLocal struct {
	poolLocalInternal

	/*
	CPU 在访问数据是按照 64/128 字节作为一行一起加载的，如果某个变量不足一行，则会和其他变量同时加载进 CPU CacheLine，当一个变量失效时会导致该行其他变量也失效，这是一种伪共享现象，
	第一、二层 CPU 缓存是每个 CPU 各自独有的，第三层 CPU 缓存是不同 CPU 之间共享的，CPU CacheLine 中有变量失效时，会导致整个 CPU CacheLine 都需要从主存中重新加载，对性能有影响，
	如果没有 pad 字段，可能会导致一个 CPU CacheLine 中存在多个 poolLocal 对象，而这些对象又属于不同 CPU 上的 P，
	当某个 CPU 上的 P 修改了 CPU CacheLine 上的该 P 对应的 poolLocal 时，会导致其他 poolLocal 失效，那么该 poolLocal 对应的 P 所在的 CPU 就需要重新加载，
	所以，pad 的目的是让专属于某个 P 的 poolLocal 独占一整个 CPU CacheLine，避免使得其他 poolLocal 在 CPU CacheLine 中失效，毕竟该 P 是优先访问自己的 poolLocal
	*/
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrand() uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x interface{}) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put 往池子中添加 x
func (p *Pool) Put(x interface{}) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrand()%4 == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	// 将当前 G 绑定到 P，并返回 P 的 poolLocal 和 id（CPU序号）
	l, _ := p.pin()
	if l.private == nil {
		// 如果 P 的 poolLocal 的私有对象为空，则直接将 x 赋给它
		l.private = x
		x = nil
	}
	if x != nil {
		// 说明 P 的 poolLocal 的私有对象不为空，则将 x push 到其附属的链表的头部，因为该 P 是其 poolLocal 的生产者
		l.shared.pushHead(x)
	}
	runtime_procUnpin() // 接触 G 与 P 的绑定
	if race.Enabled {
		race.Enable()
	}
}

// Get 从池中选择任意项，将其从池中删除，然后将其返回给调用方
// Get 可以选择忽略池并将其视为空。调用方不应假定传递给 Put 的值与 Get 返回的值之间存在任何关系。
// 如果 Get 将要返回 nil 并且 p.New 为非 nil，则 Get 将返回调用 p.New 的结果。
func (p *Pool) Get() interface{} {
	if race.Enabled {
		race.Disable()
	}
	l, pid := p.pin()
	x := l.private
	l.private = nil
	if x == nil {

		// 尝试弹出本地分片的头部。对于重用的时间局部性，我们更喜欢头而不是尾。
		// 时间局部性是指处理器在短时间内多次访问相同的内存位置或附近的内存位置的倾向
		x, _ = l.shared.popHead()
		if x == nil {
			x = p.getSlow(pid)
		}
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}
	if x == nil && p.New != nil {
		x = p.New()
	}
	return x
}

func (p *Pool) getSlow(pid int) interface{} {
	// See the comment in pin regarding ordering of the loads.
	size := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	locals := p.local                            // load-consume
	// Try to steal one element from other procs.
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		return nil
	}
	locals = p.victim
	l := indexLocal(locals, pid)
	if x := l.private; x != nil {
		l.private = nil
		return x
	}
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// 将当前 goroutine 固定到 P，禁用抢占并返回 P 的 poolLocal 和 P 的 ID
// 调用方必须在处理完池后调用 runtime_procUnpin()
func (p *Pool) pin() (*poolLocal, int) {
	pid := runtime_procPin()
	// 在 pinSlow 中，我们存储到 local，然后存储到 localSize，这里我们以相反的顺序加载。
	// 由于我们禁用了抢占，因此 GC 不会在两者之间发生。因此，在这里我们必须观察到 local 至少和 localSize 一样大。
	// 我们可以观察到一个较新的局部，这很好（我们必须观察它的零初始化性）。
	s := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	l := p.local                              // load-consume
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	runtime_procUnpin()
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	s := p.localSize
	l := p.local
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	if p.local == nil {
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	size := runtime.GOMAXPROCS(0) // 只获取先前设置的最大并发数，不实际改变其值
	local := make([]poolLocal, size)
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	runtime_StoreReluintptr(&p.localSize, uintptr(size))     // store-release
	return &local[pid], pid
}

func poolCleanup() {
	// 在垃圾回收开始时，STW 的情况下调用此函数，它不能分配，也可能不应该调用任何运行时函数。

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).

	// Drop victim caches from all pools.
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	oldPools, allPools = allPools, nil
}

var (
	allPoolsMu Mutex

	// allPools 是具有非空主缓存的一组池。受 1) allPoolsMu and pinning or 2) STW 保护
	allPools []*Pool

	// oldPools 是具有非空 victim 缓存的一组池。受 STW 保护.
	oldPools []*Pool
)

func init() {
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())

// 获取当前 goroutine 所绑定的处理器 P 的 ID
func runtime_procPin() int
func runtime_procUnpin()

// The below are implemented in runtime/internal/atomic and the
// compiler also knows to intrinsify the symbol we linkname into this
// package.

//go:linkname runtime_LoadAcquintptr runtime/internal/atomic.LoadAcquintptr
func runtime_LoadAcquintptr(ptr *uintptr) uintptr

//go:linkname runtime_StoreReluintptr runtime/internal/atomic.StoreReluintptr
func runtime_StoreReluintptr(ptr *uintptr, val uintptr) uintptr
