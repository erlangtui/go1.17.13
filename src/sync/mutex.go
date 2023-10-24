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
	// 慢速路径（概述以便可以内联快速路径）
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64 // 等待时间
	starving := false       // 是否处于饥饿模式
	awoke := false          // 是否处于唤醒状态
	iter := 0               // 自旋次数
	old := m.state          // 锁的当前状态
	for {
		// 不要在饥饿模式下自旋，所有权会交给队列前面的等待者协程，所以我们无论如何都无法获得互斥锁。
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// 如果是锁定状态且不是处于饥饿模式，并且符合自旋条件，则开始自旋
			// 自旋之前，尝试设置 mutexWoken 标志位，以通知解锁操作不要唤醒其他被阻塞的 goroutine，这是为了避免不必要的唤醒和上下文切换
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			runtime_doSpin() // 开始自旋
			iter++ // 自旋计数
			old = m.state // 重新读取互斥锁的状态 m.state，为了在下一次循环中重新检查互斥锁的状态，并决定是否继续自旋
			continue
		}
		new := old
		// 不要试图获取饥饿的互斥锁，新到的goroutines必须排队。
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
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
