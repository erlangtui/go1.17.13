// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync sync 包提供了基本的同步原，如互斥锁。除了 Once 和 WaitGroup 类型之外，大多数类型都是为低级库协程使用准备的。
// 更高级别的同步最好通过通道和通信来完成。包含在此包中定义的类型的值不应该被复制。
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // 为运行时准备

// Mutex 是互斥锁，其零值是未锁的状态，首次使用后不能被复制
type Mutex struct {
	state int32 // 锁的状态
	sema  uint32
}

// Locker 表示一个可以加锁解锁的对象
type Locker interface {
	Lock()
	Unlock()
}

// Mutex 可以处于 2 种操作模式：正常和饥饿。
// 在正常模式下，等待者们按照先进先出的顺序排队，但是被唤醒的等待者不会拥有互斥锁，而是与新到来的协程竞争所有权。
// 新到来的goroutine有一个优势 - 它们已经在CPU上运行，并且可能有很多，所以醒来的等待者很有可能失败。
// 在这种情况下，它会在等待队列的前面排队。如果等待者超过 1 毫秒未能获取互斥锁，则会将互斥锁切换到饥饿模式。
// 在饥饿模式下，互斥锁的所有权直接从解锁协程移交给队列前面的等待者。
// 新到来的goroutines不会尝试获取互斥锁，即使它看起来已解锁，也不会尝试旋转。
// 相反，他们将自己排在等待队列的尾部。
// 如果等待者获得互斥锁的所有权，并看到（1）它是队列中的最后一个等待者，或（2）等待时间少于 1 毫秒，则会将互斥锁切换回正常操作模式。
// 正常模式具有更好的性能，因为即使有阻塞的等待者，goroutine也可以连续多次获取互斥锁。饥饿模式对于预防尾部延迟的病理性示例很重要。
const (
	mutexLocked           = 1 << iota // 1，互斥锁被锁定状态
	mutexWoken                        // 2，互斥锁被唤醒状态
	mutexStarving                     // 4，互斥锁处于饥饿模式
	mutexWaiterShift      = iota      // 3，互斥锁上等待的 goroutine 偏移量
	starvationThresholdNs = 1e6       // 进入饥饿模式等待的阈值，1e6纳秒，即1ms
)

// Lock 如果锁已经被使用了，则调用程序一直阻塞直到锁可用
func (m *Mutex) Lock() {
	// 锁当前状态是未锁定的，并且正好能够加锁成功，则直接返回
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// 当锁不是未锁定的状态时，慢速路径（概述以便可以内联快速路径）
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64 // 等待时间
	starving := false       // 是否处于饥饿模式
	awoke := false          // 是否有协程处于唤醒状态
	iter := 0               // 自旋次数
	old := m.state          // 锁的当前状态
	for {                   // 开始进行阻塞调用
		// 不要在饥饿模式下自旋，所有权会交给队列前面的等待者协程，所以我们无论如何都无法获得互斥锁。
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// 如果是锁定状态且不是处于饥饿模式，并且符合自旋条件，则开始自旋
			// 在自旋等待期间，当前 goroutine 会不断判断是否可以设置 mutexWoken 标志位来通知 Unlock() 方法不必唤醒其他阻塞的goroutine，避免不必要的唤醒和上下文切换。
			// 如果可以设置，就将 awoke 标识位置为 true，然后调用 runtime_doSpin() 进行自旋，逐渐增加 iter 计数器，更新 old Mutex 的状态。
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				// 没有协程被唤醒、锁也不是唤醒状态、还有协程等待的协程，且当前协程能够将锁的状态更新为 mutexWoken
				awoke = true
			}
			runtime_doSpin() // 开始自旋
			iter++           // 自旋计数
			old = m.state    // 重新读取互斥锁的状态 m.state，为了在下一次循环中重新检查互斥锁的状态，并决定是否继续自旋
			continue
		}
		// 以下非自旋状态，新到的goroutines不要试图获取饥饿模式下的互斥锁，必须排队
		new := old // 创建变量 new，用于存储新的Mutex状态
		if old&mutexStarving == 0 {
			// 互斥锁非饥饿模式，则将 new 置为 mutexLocked，否则不做处理，等待其他等待者获取Mutex
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 {
			// 互斥锁饥饿模式或已经是加锁状态，则将 new 的 mutexWaiterShift 位置为 1（表示当前goroutine也成为Mutex的等待者）
			new += 1 << mutexWaiterShift
		}

		// 如果当前 Mutex 处于饥饿状态，并且已经有其他goroutine持有该Mutex，则切换到饥饿模式。
		// 如果Mutex未被持有，则不切换到饥饿模式，因为Unlock期望饥饿模式下有等待者，但实际情况不一定有。
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		if awoke {
			// goroutine 已从睡眠状态唤醒，因此无论哪种情况，我们都需要重置标志。
			if new&mutexWoken == 0 {
				// 互斥锁状态不一致，抛出一个错误
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&(mutexLocked|mutexStarving) == 0 {
				break // 通过 CAS 方式已经加锁成功
			}
			// 如果已经处于等待的过程中，直接排在队列最前面
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				// 如果没有等待则记录开始等待时刻
				waitStartTime = runtime_nanotime()
			}
			// 通过信号量保证锁只能被 1 个 goroutine 获取到
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			// 如果等待时间超过了阈值，那么就进入饥饿模式
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				// 如果当前 goroutine 被唤醒并且锁处于饥饿模式
				// 控制权转交给了当前 goroutine，但是互斥锁处于某种不一致的状态：mutexLocked 标识未设置，仍然认为当前 goroutine 正在等待锁
				// 抛出一个 BUG: mutex 状态不一致
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 减少等待的 goroutine 数量 (注意偏移量使用方法)
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// 退出饥饿模式
					// 必须要在这里退出并且考虑等待时间
					// 饥饿模式效率很低，一旦 2 个 goroutine 同时将互斥锁切换到饥饿模式，可能会陷入无限循环
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			old = m.state // 获取锁失败，更新 old 的值，继续进行循环等待
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock 解锁 m。如果 m 在进入 Unlock 时未锁定，则为运行时错误。
// 锁定的 Mutex 不与特定的 goroutine 相关联。允许一个 goroutine 锁定一个互斥锁，然后安排另一个 goroutine 来解锁它。
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// 如果去除 mutexLocked 标识之后正好是 0, 说明当前 goroutine 成功解锁，直接返回即可
	// 否则，解锁失败，进入慢解锁路径
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		runtime_Semrelease(&m.sema, true, 1)
	}
}
