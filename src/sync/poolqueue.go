// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// poolDequeue 是一个无锁、固定大小的单生产者、多使用者队列。
// 单一生产者既可以从头部推入，也可以从头部弹出，消费者可以从尾部弹出。
// 它有一个附加功能，即剔除未使用的插槽，以避免不必要的对象保留
// 这对于 sync.Pool 很重要，但在文献中通常不会考虑这种性质
type poolDequeue struct {
	// headTail 将一个 32 位头部索引和一个 32 位尾部索引打包在一起。这两个值都和 len(vals)-1 取模过。
	// tail 队列中最老数据的索引
	// head 将要填充的下一个槽的索引
	// 槽的范围是 [tail, head) ，为消费者拥有.
	// 消费者继续拥有这个范围之外的插槽，直到它耗尽该插槽，此时所有权转移给生产者。
	// 头索引存储在最高有效位中，因此我们可以自动添加它，并且溢出是无害的。
	// 队列为空时，head == tail；队列满了时，tail + len(vals) == head，dequeueLimit 保证其不会越界
	// head，tail 不会在超过队列长度时被赋值为其求余后的值，而是在要需要通过其取值时，用其求余后的余值作为索引
	headTail uint64

	// vals 是一个存储 interface{} 的环形队列，它的长度必须是 2 的幂
	// 如果 slot 为空，则 vals[i].typ 为空；否则，非空。
	// 由 consumer 设置成 nil，由 producer 读
	// 在 tail 不在指向 slot 并且vals[i].typ 为 nil 之前，slot 一直是有用的
	// 它由消费者自动设置为nil，由生产者自动读取。
	vals []eface
}

type eface struct {
	typ, val unsafe.Pointer
}

const dequeueBits = 32

// dequeueLimit 是 poolDequeue 的最大尺寸
// 这最多只能是 (1<<dequeueBits)/2，因为检测完整度取决于环形缓冲区的包装，而不包装索引。我们除以 4，因此这适合 32 位上的 int。
const dequeueLimit = (1 << dequeueBits) / 4

// dequeueNil 在 poolDequeue 中表示 interface{}(nil).
// 由于我们使用 nil 来表示空插槽，因此我们需要一个哨兵值来表示 nil，以区分空槽与空值。
type dequeueNil *struct{}

// 从 64 位 ptrs 中解出 32 位 head, tail 值
func (d *poolDequeue) unpack(ptrs uint64) (head, tail uint32) {
	const mask = 1<<dequeueBits - 1
	head = uint32((ptrs >> dequeueBits) & mask) // 右移 32 位后与 32 个 1 与运算，并 32 位截断
	tail = uint32(ptrs & mask)                  // 与 32 个 1 与运算后，32 位截断
	return
}

// 将 32 位 head, tail 值打包为 64 位值
func (d *poolDequeue) pack(head, tail uint32) uint64 {
	const mask = 1<<dequeueBits - 1
	return (uint64(head) << dequeueBits) |
		uint64(tail&mask)
}

// pushHead 在队列头部添加 val，如果队列已满，则返回false，必须被单生产者调用
func (d *poolDequeue) pushHead(val interface{}) bool {
	ptrs := atomic.LoadUint64(&d.headTail)
	head, tail := d.unpack(ptrs)
	if (tail+uint32(len(d.vals)))&(1<<dequeueBits-1) == head {
		// 队列已满
		return false
	}
	slot := &d.vals[head&uint32(len(d.vals)-1)]

	// 检查 popTail 是否释放了头插槽
	typ := atomic.LoadPointer(&slot.typ)
	if typ != nil {
		// 当前槽不为空，插入后会形成覆盖，说明另一个 goroutine 仍在清理尾部，因此队列实际上仍然已满。
		return false
	}

	// head 索引处的插槽为空，可以插入数据
	if val == nil {
		// 待插入的数据为 nil 时，设置为 dequeueNil，以区分空槽与空值
		val = dequeueNil(nil)
	}
	*(*interface{})(unsafe.Pointer(slot)) = val

	// 增加 head，这会将插槽的所有权传递给 popTail，并充当写入插槽的存储屏障。
	atomic.AddUint64(&d.headTail, 1<<dequeueBits)
	return true
}

// popHead 移除并返回队列首部的元素
// 如果队列为空，则返回 false，必须由但生产者调用
func (d *poolDequeue) popHead() (interface{}, bool) {
	var slot *eface
	for {
		ptrs := atomic.LoadUint64(&d.headTail)
		head, tail := d.unpack(ptrs)
		if tail == head {
			// 头尾相等，队列为空
			return nil, false
		}

		// 确认尾部并递减头。我们在读取值之前执行此操作，以收回此插槽的所有权。
		head--
		ptrs2 := d.pack(head, tail)
		if atomic.CompareAndSwapUint64(&d.headTail, ptrs, ptrs2) {
			// 成功回收插槽，head 对应的是将要插入的值的索引，减减后才是实际上要弹出的值
			slot = &d.vals[head&uint32(len(d.vals)-1)]
			break
		}
	}

	val := *(*interface{})(unsafe.Pointer(slot))
	if val == dequeueNil(nil) {
		val = nil
	}
	// 将插槽归零，避免在 pushHead 时发现不为 nil。
	// 与 popTail 不同的是，这不是与 pushHead 竞争，所以我们在这里不需要小心。
	*slot = eface{}
	return val, true
}

// popTail 移除并返回队列尾部的元素
// 如果队列为空，则返回 false，可以被任意数量的消费者调用
func (d *poolDequeue) popTail() (interface{}, bool) {
	var slot *eface
	for {
		ptrs := atomic.LoadUint64(&d.headTail)
		head, tail := d.unpack(ptrs)
		if tail == head {
			// 头尾相等，队列为空
			return nil, false
		}

		// 增加尾部
		ptrs2 := d.pack(head, tail+1)
		if atomic.CompareAndSwapUint64(&d.headTail, ptrs, ptrs2) {
			// 成功拥有尾部的插槽
			slot = &d.vals[tail&uint32(len(d.vals)-1)]
			break
		}
	}

	// We now own slot.
	val := *(*interface{})(unsafe.Pointer(slot))
	if val == dequeueNil(nil) {
		val = nil
	}

	// 告诉pushHead，我们已经用完了这个插槽。将槽置零也很重要，这样我们就不会留下可能使该对象存活时间超过必要时间的引用。
	// 我们首先写入 val，然后通过原子写入 typ 来发布我们已经完成了这个插槽。
	slot.val = nil
	atomic.StorePointer(&slot.typ, nil)
	// At this point pushHead owns the slot.

	return val, true
}

// poolChain 是 poolDequeue 动态长度的版本
// 这是作为 poolDequeues 的双向链表队列实现的，其中每个队列的大小是前一个队列的两倍。
// 一旦排队填满，就会分配一个新的，并且只会推送到最新的排队。
// 弹出发生在列表的另一端，一旦排队用尽，它就会从列表中删除。
type poolChain struct {
	// head 是要从头部推入的 poolDequeue。这只能由生产者访问，因此不需要同步。所以 head 指向的是最新创建的队列，也是最大的队列。
	head *poolChainElt
	// tail 是要从尾部弹出的 poolDequeue。它由消费者访问，因此读取和写入必须是原子的。tail 指向的是最早创建的队列，也就是最小的队列。
	tail *poolChainElt
}

// poolChainElt 中每个 poolDequeue 队列的长度都是 2 幂，并且是前一个队列的两倍
type poolChainElt struct {
	poolDequeue

	// next、prev 链接到 poolChain 相邻的 poolChainElts
	// next 指向的方向为 head，由生产者以原子方式编写，由消费者以原子方式读取。它只从 nil 过渡到非 nil。
	// prev 指向的方向为 nil，由消费者以原子方式编写，由生产者以原子方式读取。它只从非 nil 过渡到 nil。
	next, prev *poolChainElt
}

func storePoolChainElt(pp **poolChainElt, v *poolChainElt) {
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(pp)), unsafe.Pointer(v))
}

func loadPoolChainElt(pp **poolChainElt) *poolChainElt {
	return (*poolChainElt)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(pp))))
}

func (c *poolChain) pushHead(val interface{}) {
	d := c.head
	if d == nil {
		// 初始化 chain.
		const initSize = 8 // 必须为 2 的幂
		d = new(poolChainElt)
		d.vals = make([]eface, initSize)
		c.head = d
		storePoolChainElt(&c.tail, d) // tail 的写入必须是原子方式的
	}

	if d.pushHead(val) {
		return
	}

	// 当前队列已满，新分配的队列长度是当前的两倍
	newSize := len(d.vals) * 2
	if newSize >= dequeueLimit {
		// Can't make it any bigger.
		newSize = dequeueLimit
	}

	d2 := &poolChainElt{prev: d}     // 新创建的 poolChainElt 的前一个指向当前的 poolChainElt
	d2.vals = make([]eface, newSize) // 新创建的 poolChainElt 尺寸翻倍
	c.head = d2                      // head 指向新创建的 poolChainElt
	storePoolChainElt(&d.next, d2)   // 当前的 poolChainElt 的下一个指向新创建的 poolChainElt
	d2.pushHead(val)                 // 将 val 插入新创建的 poolChainElt 的头部
}

// head 侧的空队列不会被删除
func (c *poolChain) popHead() (interface{}, bool) {
	d := c.head
	for d != nil {
		if val, ok := d.popHead(); ok {
			return val, ok
		}
		// 获取前一个队列，尝试从前面的队列中弹出
		d = loadPoolChainElt(&d.prev)
	}
	return nil, false
}
// tail 侧的空队列会被删除
func (c *poolChain) popTail() (interface{}, bool) {
	d := loadPoolChainElt(&c.tail)
	if d == nil {
		return nil, false
	}

	for {
		// 在弹出尾部之前加载 next 指针是很重要的。
		// 一般来说，d 可能暂时为空，但如果 next 在弹出操作之前为非空，并且弹出操作失败，则 d 永久为空，这是将 d 从链中删除的唯一安全条件。
		d2 := loadPoolChainElt(&d.next)

		if val, ok := d.popTail(); ok {
			return val, ok
		}

		if d2 == nil {
			// 这是唯一的队列。它现在是空的，但将来可能会被推入数据。
			return nil, false
		}

		// 链条的尾部已弹空，尝试将 tail 指向当前队列的 next 队列，以从链表中删除当前队列，这样下一次弹出时就不必再次查看空队列
		if atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&c.tail)), unsafe.Pointer(d), unsafe.Pointer(d2)) {
			// CAS 成功，清除 prev 指针，当前队列的 next 队列的 pre 是指向当前队列的，此时需要置为 nil，以便垃圾回收期可以回收当前这个空队列
			// 逐步删除短的队列，可以保证所有的元素都在一个或多个连续的队列中，而队列的长度和元素的长度是相近的，可以避免内存浪费
			storePoolChainElt(&d2.prev, nil)
		}
		d = d2
	}
}
