// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// WaitGroup 等待一组 goroutine 完成。
// 主 goroutine 调用 Add 来设置要等待的 goroutine 数量。
// 然后，每个 goroutine 都会运行，并在完成后调用 Done。
// 同时，Wait 可用于阻塞，直到所有 goroutines 完成。
// WaitGroup 首次使用后不允许被复制
type WaitGroup struct {
	noCopy noCopy

	// 64 位值：高 32 位为计数器，低 32 位为 waiter 计数。
	// 64 位原子操作需要 64 位对齐，但 32 位编译器不能确保这一点。
	// 因此，我们分配 12 个字节，然后使用其中对齐的 8 个字节作为状态，另外 4 个字节作为 sema 的存储。
	state1 [3]uint32
}

// state 返回指向存储在 wg.state1 中的 state 和 sema 字段的指针，计数与信号
func (wg *WaitGroup) state() (statep *uint64, semap *uint32) {
	// 将 waiter counter 两个计数器放进一个 uint64 变量，这样就可以在不加锁的情况下，支持并发场景下的原子操作了，极大地提高了性能
	if uintptr(unsafe.Pointer(&wg.state1))%8 == 0 {
		// 64 位，waiter counter sema
		return (*uint64)(unsafe.Pointer(&wg.state1)), &wg.state1[2]
	} else {
		// 32 位，sema waiter counter
		return (*uint64)(unsafe.Pointer(&wg.state1[1])), &wg.state1[0]
	}
}

// Add 将增量delta（可能为负数）添加到 WaitGroup 计数器。
// 如果计数器变为零，则释放所有在 Wait 上阻塞的 goroutine。如果计数器变为负数，则 Add panic。
// 当计数器为零时，delta 为正的 Add 调用必须在 Wait 之前发生。
// 当计数器大于零开始，负的 delta Add 调用可能随时发生。
// 对 Add 的调用应在创建要等待的 goroutine 或其他事件的语句之前执行。
// 如果重用 WaitGroup 来等待多个独立的事件集，则必须在返回所有以前的 Wait 调用后进行新的 Add 调用。
func (wg *WaitGroup) Add(delta int) {
	statep, semap := wg.state()
	if race.Enabled {
		_ = *statep // trigger nil deref early
		if delta < 0 {
			// Synchronize decrements with Wait.
			race.ReleaseMerge(unsafe.Pointer(wg))
		}
		race.Disable()
		defer race.Enable()
	}
	// statep 高位存储的是 counter，将 delta 左移 32 位，加到 statep 的高位上
	state := atomic.AddUint64(statep, uint64(delta)<<32)
	v := int32(state >> 32) // 右移 32 位 得到实际的 counter 值
	w := uint32(state)      // 直接用 32 位截断，得到低位存储的 waiter
	if race.Enabled && delta > 0 && v == int32(delta) {
		// The first increment must be synchronized with Wait.
		// Need to model this as a read, because there can be
		// several concurrent wg.counter transitions from 0.
		race.Read(unsafe.Pointer(semap))
	}
	if v < 0 {
		// 计数器小于 0 时，panic
		panic("sync: negative WaitGroup counter")
	}
	if w != 0 && delta > 0 && v == int32(delta) {
		// 已经调用了 Wait，但计数器为零，且 delta 为正，说明 Add 调用在 Wait 之后发生，panic
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	if v > 0 || w == 0 {
		// 计数器大于 0 或是没有调用 wait，不需要后续处理
		return
	}

	// 当 counter = 0，waiters > 0 时，现在不能同时发生状态突变：
	// - Add 不得与 Wait 同时发生，
	// - 如果 Wait 看到计数器 == 0，则不会增加 waiters。
	// 仍然做一个廉价的健全性检查来检测 WaitGroup 的滥用。
	if *statep != state {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// counter 为 0，说明所有 goroutine 已经调用了 done 操作，重置 waiter 为 0，并逐一唤醒调用 Wait 的 goroutine
	*statep = 0
	for ; w != 0; w-- {
		runtime_Semrelease(semap, false, 0)
	}
}

// Done WaitGroup 计数减一
func (wg *WaitGroup) Done() {
	wg.Add(-1)
}

// Wait 阻塞直到 WaitGroup 计数变为 0
func (wg *WaitGroup) Wait() {
	statep, semap := wg.state()
	if race.Enabled {
		_ = *statep // trigger nil deref early
		race.Disable()
	}
	for {
		state := atomic.LoadUint64(statep)
		v := int32(state >> 32)
		w := uint32(state)
		if v == 0 {
			// 计数器为 0，不需要等待，直接返回
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
		// 计数器不为 0，说明还有 goroutine 没有调用 Done
		// 等待者 waiters 计数加一
		if atomic.CompareAndSwapUint64(statep, state, state+1) {
			if race.Enabled && w == 0 {
				// Wait must be synchronized with the first Add.
				// Need to model this is as a write to race with the read in Add.
				// As a consequence, can do the write only for the first waiter,
				// otherwise concurrent Waits will race with each other.
				race.Write(unsafe.Pointer(semap))
			}
			// 计数成功后，阻塞等待
			runtime_Semacquire(semap)
			// 阻塞等待完成，其他 goroutine 均已返回，wait 结束，此时 statep 应该为 0
			if *statep != 0 {
				// 如果 statep 不为 0，说明前一次的 wait 还没有返回时，WaitGroup 被复用，直接 panic
				panic("sync: WaitGroup is reused before previous Wait has returned")
			}
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
	}
}
