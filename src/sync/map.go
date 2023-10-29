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
// Map 类型是专用的。大多数代码应改用纯 Go  map ，具有单独的锁定或协调，以提高类型安全性，并更轻松地维护其他不变量以及 map 内容。
// Map 类型针对两种常见用例进行了优化：
// （1） 当给定键的条目只写入一次但读取多次时，例如在仅增长的缓存中，
// （2） 当多个 goroutine 读取、写入和覆盖不相交键集的条目时。
// 在这两种情况下，与与单独的 Mutex 或 RWMutex 配对的 Go  map 相比，使用 Map 可以显著减少锁争用。
// 零 Map 为空，可供使用, Map 在首次使用后不得复制。
// 当 dirty 为 nil 的时候，read 就代表 map 所有的数据；当 dirty 不为 nil 的时候，dirty 才代表 map 所有的数据。
type Map struct {
	mu Mutex // 当写 read 或者读写 dirty 时，需要加互斥锁

	// read 包含了 Map 中可安全并发访问的部分（无论是否有互斥锁 mu）
	// read 字段本身始终可以安全的执行 load 操作，但 store 操作时必须和互斥锁 mu 一起。
	// 存储在 read 中的值可以在没有 mu 的情况下并发更新，但更新以前删除的条目需要将该条目复制到 dirty map 中，并在保留 mu 的情况下取消删除
	read atomic.Value // 存储 readOnly 结构体类型

	// dirty 包含 Map 中需要持有 mu 才能访问的部分。为了确保 dirty map 可以快速提升为 read map，它还包括 read map 中所有未删除的条目。
	// 被标记为 expunged 的元素不会存储在 dirty 中
	// 被标记为 expunged 的元素如果要存储新的值，需要先执行 unexpunged 添加到 dirty, 然后再更新值
	// 新添加的元素会优先放入 dirty map
	// 如果 dirty 为 nil，则下次写入 Map 时将通过浅拷贝一个空的 map 来初始化它，忽略的条目。
	dirty map[interface{}]*entry

	// misses 计算自上次更新 read map 以来需要锁定 mu 以确定 key 是否存在的负载数。
	// 一旦 misses  dirty map 的成本，dirty map 将被提升为 read map（处于未修改状态），map 的下一个存储将创建一个新的 dirty 副本。
	misses int // 加锁则计数，查询dirty时需要加锁
}

// readOnly 是以原子方式存储在 Map.read 字段中的不可变结构
type readOnly struct {
	m map[interface{}]*entry
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

// 根据值 i 创建 entry 对象
func newEntry(i interface{}) *entry {
	return &entry{p: unsafe.Pointer(&i)}
}

// Load 返回存储在 map 中的键值，如果不存在任何值，则返回 nil。ok 结果指示是否在 map 中找到值。
func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		// read map 中没有该 key， 且 dirty map 中有 read map 中没有的 key
		m.mu.Lock()
		// 避免在上锁的过程中 dirty map 提升为 read map，再进行一次判断。
		// 如果不会错过同一键的进一步加载，则不值得复制此键的 dirty map 。
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

// Store 存储设置键的值
func (m *Map) Store(key, value interface{}) {
	read, _ := m.read.Load().(readOnly)
	// 获取只读的 map，如果只读的 map 存在该 key，则尝试更新其值，原子+自旋，无需调用 mu 锁
	if e, ok := read.m[key]; ok && e.tryStore(&value) {
		return
	}

	// read 中不存在该 key，或该 key 对应的 entry 已被删除
	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		// 如果 key 对应的值存在于 read map 中，但 p == expunged, 说明 dirty != nil 并且 key 对应的值不存在于 dirty map 中
		// 先将 p 的状态由 expunged 改为 nil，并在 dirty map 添加 key
		// 更新 entry.p = value (read map 和 dirty map 指向同一个entry)
		if e.unexpungeLocked() {
			m.dirty[key] = e // 此时 entry 中的 p 已经置为 nil
		}
		e.storeLocked(&value) // 更新 entry 中 p 所指向的 value
	} else if e, ok := m.dirty[key]; ok { // 只在 dirty map 中，直接修改
		e.storeLocked(&value)
	} else {               // 两个 map 都不在
		if !read.amended { // dirty 中没有比 read 中多的 key，则向 dirty map 中加入该 key，并置 amended = true
			// 表示往 dirty map 中第一次添加新 key
			m.dirtyLocked()                                  // dirty map 为空，则对 read map 进行浅拷贝
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
			// 已删除则直接返回 false
			return false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
			return true
		}
	}
}

// unexpungeLocked 解除 entry 的删除状态
// 如果 entry 之前已被清除，则必须在解锁 m.mu 之前将其添加到 dirty map 中。
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
	read, _ := m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		// ok 表示值是否被删除，loaded 表示返回值是旧值还是存储了新值
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
			m.dirtyLocked()                                  // dirty map 为空则浅拷贝
			m.read.Store(readOnly{m: read.m, amended: true}) // 更新 amended
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// 值没有被删除的话，就获取该值，没有该值则赋予新值
// 如果 entry 被删除了，tryLoadOrStore 则保留该 entry 不变，返回 ok 为 false
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
		// 再次校验，防止上锁期间 dirty map 被其他协程提升为 read map
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			delete(m.dirty, key) // 直接从 dirty map 中删除
			// 在 dirty map 被提升到 read map 之前，这个 key 对应的值会一直从 dirty map 中获取
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
		// 当 p 为 expunged 时，表示它已经不在 dirty 中了。
		// 这是 p 的状态机决定的，在 tryExpungeLocked 函数中，会将 nil 原子地设置成 expunged
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return *(*interface{})(p), true
		}
	}
}

// Range 对 map 中存在的每个键和值按顺序调用 f，如果 f 返回 false，则 Range 停止遍历；
// Range 不会多次访问任何键，如果某个 key 对应的 value 被并发地更新或者删除了，Range 可能返回修改前或修改后的值
// Range 复杂度是 O（N），即使 f 在恒定的调用次数后返回 false。
// Range 过程中，如果删除或存储某个key，可能无法被遍历到的
func (m *Map) Range(f func(key, value interface{}) bool) {
	read, _ := m.read.Load().(readOnly)
	// 如果 dirty 和 read 不一样，则提升 dirty 为 read
	if read.amended {
		// 当 amended 为 true 时，说明 dirty 中含有 read 中没有的 key
		// 因为 Range 会遍历所有的 key，是一个 O(n) 操作
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
		// 在拷贝 read map 时，将为 nil 的 entry 设为 expunged
		// 以便再次写入时，可以添加到 dirty map 时能够同步更新 read map 中的 entry
		if !e.tryExpungeLocked() { // 判断是否被删除
			// 只拷贝未删除的
			m.dirty[k] = e
		}
	}
}

// 尝试将 entry 中的 nil p 标记 expunged，并返回是否成功
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
