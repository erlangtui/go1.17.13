// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Map 类似于 Go map[interface{}]interface{}，但可以安全地由多个 goroutines 并发使用，无需额外的锁定或协调。
// 加载、存储和删除在摊销的常量时间内运行。
// Map 类型是专用的。大多数代码应改用纯 Go 映射，具有单独的锁定或协调，以提高类型安全性，并更轻松地维护其他不变量以及映射内容。
// Map 类型针对两种常见用例进行了优化：
// （1） 当给定键的条目只写入一次但读取多次时，例如在仅增长的缓存中，
// （2） 当多个 goroutine 读取、写入和覆盖不相交键集的条目时。
// 在这两种情况下，与与单独的 Mutex 或 RWMutex 配对的 Go 映射相比，使用 Map 可以显著减少锁争用。
// 零 Map 为空，可供使用, Map 在首次使用后不得复制。
// 当 dirty 为 nil 的时候，read 就代表 map 所有的数据；当 dirty 不为 nil 的时候，dirty 才代表 map 所有的数据。
type Map struct {
	mu Mutex

	// read 包含映射内容中可安全并发访问的部分（保留或不保留 MU）。read 字段本身始终可以安全加载，但只能与 mu 一起存储。
	// 存储在 read 中的条目可以在没有 mu 的情况下同时更新，但更新以前删除的条目需要将该条目复制到 dirty map 中，并在保留 mu 的情况下取消删除。
	read atomic.Value // 存储 readOnly 结构体类型

	// dirty 包含 map 中需要保留 MU 的部分。为了确保 dirty map 可以快速提升为 read map，它还包括 read map 中所有未删除的条目。
	// 删除的条目不会存储在 dirty 中。清理 map 中已清除的条目必须取消删除并添加到 dirty map 中，然后才能将新值存储到 dirty map 中。
	// 如果 dirty 为 nil，则下次写入映射时将通过创建干净映射的浅拷贝来初始化它，省略过时的条目。
	dirty map[interface{}]*entry

	// misses 计算自上次更新 read map 以来需要锁定 MU 以确定 key 是否存在的负载数。
	// 一旦 misses 足够支付复制 dirty map 的成本，dirty map 将被提升为 read map（处于未修改状态），map 的下一个存储将创建一个新的 dirty 副本。
	misses int // 加锁则计数，查询dirty时需要加锁
}

// readOnly 是以原子方式存储在 Map.read 字段中的不可变结构
type readOnly struct {
	m       map[interface{}]*entry
	// 如果 dirty map 包含了不在 m 中的键，则为 true
	amended bool
}

// expunged 是一个任意指针，用于标记从 dirty map中删除的 entry 为删除状态
var expunged = unsafe.Pointer(new(interface{}))

// entry 是 map 中与特定键对应的插槽。
type entry struct {
	// p 为指向 interface{} 类型值的指针
	// 如果 p == nil，则该条目已被删除，并且 m.dirty == nil 或 m.dirty[key] 指向该 entry。
	// 如果 p == expunged，则条目已被删除，m.dirty != nil，并且 m.dirty 中没有这个key。
	// 否则，该 entry 有效并记录在 m.read.m[key] 中，如果 m.dirty ！= nil，则记录在 m.dirty[key] 中。
	// 可以通过用 nil 原子替换来删除 entry：下次创建 m.dirty 时，它将原子地将 nil 替换为 expunged，并保留 m.dirty[key] 未设置。
	// entry 的关联值可以通过原子替换来更新，前提是 p ！= 已清除。如果 p == 已清除，则只有在首次设置 m.dirty[key] = e 后才能更新 entry 的关联值，以便使用 dirty map 的查找找到该 entry。
	p unsafe.Pointer // *interface{}
}

func newEntry(i interface{}) *entry {
	return &entry{p: unsafe.Pointer(&i)}
}

// Load 返回存储在映射中的键值，如果不存在任何值，则返回 nil。ok 结果指示是否在映射中找到值。
func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		// read map 中没有该 key， 且 dirty map 中有 read map 中没有的 key
		m.mu.Lock()
		// 避免在上锁的过程中 dirty map 提升为 read map，在进行一次判断。如果不会错过同一键的进一步加载，则不值得复制此键的脏映射。
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// 不管 dirty 中有没有找到，都要"记一笔"，因为在 dirty 提升为 read 之前，都会进入这条路径
			// 如果 miss 值达到 dirty map 的长度时，将 dirty map 提升为 read map，并将 dirty map 置为 nil，miss 置为 0
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if !ok { // dirty map 中也没有该 key
		return nil, false
	}
	return e.load()
}

// 从 entry 中加载值
func (e *entry) load() (value interface{}, ok bool) {
	p := atomic.LoadPointer(&e.p)
	if p == nil || p == expunged { // 该值为空或已删除，则返回false
		return nil, false
	}
	return *(*interface{})(p), true
}

// Store 存储设置键的值。
func (m *Map) Store(key, value interface{}) {
	read, _ := m.read.Load().(readOnly)
	// 获取只读的map，如果只读的map存在该key，则尝试更新其值，原子+自旋，无需调用 mu 锁
	if e, ok := read.m[key]; ok && e.tryStore(&value) {
		return
	}

	// read 中不存在该 key，或该 key 对应的 entry 已被删除
	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok { // 该 key 在只读的map中存在，那么一定在 dirty map 中存在
		// 要更新一个之前已被删除的 entry，则需要先将其状态从 expunged 改为 nil，再拷贝到 dirty 中，然后再更新。
		if e.unexpungeLocked() { // 如果 key 对应的 entry 已被删除
			// 该条目之前已被清除，这意味着存在一个非 nil dirty map，并且此条目不在其中。
			m.dirty[key] = e // 此时 entry 中的 p 已经置为 nil
		}
		e.storeLocked(&value) // 更新 entry 中 p 所指向的 value
	} else if e, ok := m.dirty[key]; ok { // 只在 dirty map 中，直接修改
		e.storeLocked(&value)
	} else { // 两个 map 都不在
		if !read.amended { // dirty 中没有比 read 中多的 key，则向 dirty map 中加入该 key，并置 amended = true
			// 表示往 dirty map 中第一次添加新 key
			m.dirtyLocked() // dirty map 为空，则对 read map 进行浅拷贝
			m.read.Store(readOnly{m: read.m, amended: true}) // 更新 amended
		}
		m.dirty[key] = newEntry(value)
	}
	m.mu.Unlock()
}

// tryStore 在 entry 尚未删除时存储一个值。如果该 entry 被清除，tryStore 将返回 false 并使该条目保持不变。
func (e *entry) tryStore(i *interface{}) bool {
	for { // 原子操作，for循环 + CAS 实现自旋锁
		p := atomic.LoadPointer(&e.p)
		if p == expunged {
			return false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
			return true
		}
	}
}

// unexpungeLocked 解除 entry 的删除状态
// 如果 entry 之前已被清除，则必须在解锁 m.mu 之前将其添加到脏地图中。
// 如果 key 对应的 entry 已被删除，则将 entry 置为 nil，并返回true
func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return atomic.CompareAndSwapPointer(&e.p, expunged, nil)
}

// storeLocked 无条件的将值存入 entry 中，必须知道该 entry 不会被删除
func (e *entry) storeLocked(i *interface{}) {
	atomic.StorePointer(&e.p, unsafe.Pointer(i))
}

// LoadOrStore 返回键的现有值（如果存在）。否则，它将存储并返回给定的值。
// 如果加载了值，则 loaded 结果为 true，如果存储，则为 false。
func (m *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	// Avoid locking if it's a clean hit.
	read, _ := m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		// ok 表示返回了值，loaded 表示返回值是旧值还是新值
		actual, loaded, ok := e.tryLoadOrStore(value)
		if ok {
			return actual, loaded
		}
	}

	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		if e.unexpungeLocked() {
			// e 被删除，则置为nil，并更新 dirty 中的值
			m.dirty[key] = e
		}
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok {
		actual, loaded, _ = e.tryLoadOrStore(value)
		m.missLocked() // 迁移判断
	} else {
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked() // dirty map 为空则浅拷贝
			m.read.Store(readOnly{m: read.m, amended: true}) // 更新 amended
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// 值没有被删除的话，就获取该值，没有该值则赋予新值
// 如果 entry 被删除了，tryLoadOrStore 则保留该 entry 不变，返回 ok 为false
// loaded 表示值是否存在，ok 表示是否返回了值
func (e *entry) tryLoadOrStore(i interface{}) (actual interface{}, loaded, ok bool) {
	p := atomic.LoadPointer(&e.p)
	if p == expunged { // 值被删除
		return nil, false, false
	}
	if p != nil { // 值存在，直接返回该值
		return *(*interface{})(p), true, true
	}


	// 在第一次加载后复制接口，使此方法更适合逃避分析：如果我们命中“load”路径或条目被删除，我们不应该费心分配堆。该值不存在，赋予新值
	ic := i
	for {
		// 自旋锁，以免被其他协程改掉，如果值为nil，直接赋予新值
		if atomic.CompareAndSwapPointer(&e.p, nil, unsafe.Pointer(&ic)) {
			return i, false, true
		}
		// 否则判断是否被删除
		p = atomic.LoadPointer(&e.p)
		if p == expunged {
			return nil, false, false
		}
		// 没有删除且不为nil，则直接返回
		if p != nil {
			return *(*interface{})(p), true, true
		}
	}
}

// LoadAndDelete 删除键的值，并返回以前的值（如果有）。loaded 的结果报告 key 是否存在。
func (m *Map) LoadAndDelete(key interface{}) (value interface{}, loaded bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			delete(m.dirty, key) // 直接从 dirty map 中删除
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked()
		}
		m.mu.Unlock()
	}
	// 如果 key 同时存在于 read 和 dirty 中时，删除只是做了一个标记，将 p 置为 nil
	if ok {
		return e.delete()
	}

	// read 中没有该 key，且read 和 dirty 一样
	return nil, false
}

// Delete 删除 key 对应的值
func (m *Map) Delete(key interface{}) {
	m.LoadAndDelete(key)
}

func (e *entry) delete() (value interface{}, ok bool) {
	for {
		p := atomic.LoadPointer(&e.p)
		if p == nil || p == expunged { // 值不存在或已经被删除
			return nil, false
		}
		// 将 p 置为 nil，m.dirty == nil 或 m.dirty[key] 为 e
		// 当 p 为 expunged 时，表示它已经不在 dirty 中了。这是 p 的状态机决定的，在 tryExpungeLocked 函数中，会将 nil 原子地设置成 expunged
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return *(*interface{})(p), true
		}
	}
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, Range may reflect any mapping for that key
// from any point during the Range call.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
// 对映射中存在的每个键和值按顺序调用 f。如果 f 返回 false，则范围停止迭代。
// Range 不一定对应于映射内容的任何一致快照：不会多次访问任何键，但如果同时存储或删除任何键的值，
// 则 Range 可能会反映该键在 Range 调用期间从任何点开始的任何映射。
// Range 可以是 O（N） 与映射中的元素数，即使 f 在恒定的调用次数后返回 false。
// 在 range 过程中，如果删除或存储某个key，是无法被遍历到的
func (m *Map) Range(f func(key, value interface{}) bool) {
	// We need to be able to iterate over all of the keys that were already
	// present at the start of the call to Range.
	// If read.amended is false, then read.m satisfies that property without
	// requiring us to hold m.mu for a long time.
	read, _ := m.read.Load().(readOnly)
	// 如果 dirty 和 read 不一样，则提升 dirty 为 read
	if read.amended {
		// m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
		// (assuming the caller does not break out early), so a call to Range
		// amortizes an entire copy of the map: we can promote the dirty copy
		// immediately!
		// 当 amended 为 true 时，说明 dirty 中含有 read 中没有的 key，因为 Range 会遍历所有的 key，是一个 O(n) 操作。
		// 将 dirty 提升为 read，会将开销分摊开来，所以这里直接就提升了
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		if read.amended {
			read = readOnly{m: m.dirty}
			m.read.Store(read)
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	// 再继续遍历 read map，遍历的是 m.read 值的副本，中间添加或删除key是无法被遍历到的
	for k, e := range read.m {
		v, ok := e.load()
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}

// misses 自增，并判断是否超过 dirty map 的长度，是则迁移 dirty map 到 read map
func (m *Map) missLocked() {
	m.misses++ // 加锁并操作 dirty map 则计数
	if m.misses < len(m.dirty) {
		return
	}
	m.read.Store(readOnly{m: m.dirty})
	m.dirty = nil
	m.misses = 0
}

// dirty map 为空，则对 read map 未删除的值进行浅拷贝到 dirty map
func (m *Map) dirtyLocked() {
	if m.dirty != nil {
		return
	}

	read, _ := m.read.Load().(readOnly)
	m.dirty = make(map[interface{}]*entry, len(read.m))
	for k, e := range read.m {
		if !e.tryExpungeLocked() { // 判断是否被删除
			m.dirty[k] = e
		}
	}
}

// 尝试将 entry 中的 p 标记 expunged，并返回是否成功
func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := atomic.LoadPointer(&e.p)
	for p == nil { // 如果值为空，尝试删除
		// 原子层面的删除
		// 如果原来是 nil，说明原 key 已被删除，则将其转为 expunged
		if atomic.CompareAndSwapPointer(&e.p, nil, expunged) {
			return true
		}
		// 重新加载，判断是否已经被其他协程删除或赋值
		p = atomic.LoadPointer(&e.p)
	}
	return p == expunged // 是否被删除
}
