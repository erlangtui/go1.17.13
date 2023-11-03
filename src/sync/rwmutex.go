// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// RWMutex runtimerwmutex.go 中有此文件的修改副本。如果您在此处进行任何更改，请查看是否应该在此处进行更改。
// RWMutex 是一个读写器互斥锁。锁可以由任意数量的读取器或单个写入器持有。
// RWMutex 的零值是解锁的互斥锁。RWMutex 在首次使用后不得复制。
// 如果一个 goroutine 持有一个 RWMutex 进行读取，而另一个 goroutine 可能会调用 Lock，
// 则在释放初始读锁之前，任何 goroutine 都不应期望能够获取读锁。特别是，这禁止递归读取锁定。
// 这是为了确保锁最终可用;被阻止的 Lock 调用会阻止新读取器获取锁定。
type RWMutex struct {
	w           Mutex  // 互斥锁
	writerSem   uint32 // 信号量，写等待读
	readerSem   uint32 // 信号量，读等待写
	readerCount int32  // 正在执行读操作的 goroutine 数量
	readerWait  int32  // 写操作被阻塞时等待的读操作个数
}

const rwmutexMaxReaders = 1 << 30

// Happens-before relationships are indicated to the race detector via:
// - Unlock  -> Lock:  readerSem
// - Unlock  -> RLock: readerSem
// - RUnlock -> Lock:  writerSem

// RLock 锁定 rw 进行读取，它不应用于递归读锁定，被阻塞的 Lock 调用会阻止新读取器获取 RWMutex
func (rw *RWMutex) RLock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	if atomic.AddInt32(&rw.readerCount, 1) < 0 {
		// 有写 goroutine 获得了锁，该读 goroutine 阻塞等待被唤醒
		runtime_SemacquireMutex(&rw.readerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// RUnlock 撤消单个 RLock 调用，不会影响其他同时读取器。如果 rw 在进入 RUnlock 时未被锁定以读取，则为运行时错误。
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}
	// 读者数量减一，小于 0，则有阻塞的写操作，便将 写操作被阻塞时等待的读操作个数 减一，为 0 时，便可以将写操作唤醒
	if r := atomic.AddInt32(&rw.readerCount, -1); r < 0 {
		// 将 readerCount 减一，如果此时为负数，说明有写 goroutine 正在调用 Lock 尝试获取互斥锁
		rw.rUnlockSlow(r)
	}
	if race.Enabled {
		race.Enable()
	}
}

func (rw *RWMutex) rUnlockSlow(r int32) {
	if r+1 == 0 || r+1 == -rwmutexMaxReaders {
		// 未加读锁便解锁，抛出错误
		race.Enable()
		throw("sync: RUnlock of unlocked RWMutex")
	}
	if atomic.AddInt32(&rw.readerWait, -1) == 0 {
		// 将 readerWait 减一，当等待的读操作个数为 0 时，将阻塞的写操作唤醒
		runtime_Semrelease(&rw.writerSem, false, 1)
	}
}

// Lock 锁定 rw 用于写入，如果锁已锁定以进行读取或写入，则 Lock 将被阻塞，直到锁可用
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// 通过互斥锁加写锁，如果已经有其他 goroutine 加上了写锁该 goroutine 就只能阻塞或自旋等待
	// 加上写锁后，互斥锁也会阻止其他 goroutine 继续加锁，其他 goroutine 只能阻塞或自旋等待
	rw.w.Lock()
	// 加上写锁后，将读操作的数量置为负数，用于阻塞继续添加读锁
	r := atomic.AddInt32(&rw.readerCount, -rwmutexMaxReaders) + rwmutexMaxReaders
	if r != 0 && atomic.AddInt32(&rw.readerWait, r) != 0 {
		// 当读操作的数量不为 0 且 读操作等待加读锁的数量不为 0 ，则将当前 goroutine 阻塞
		// 即使已经获得了互斥锁（能够阻止后续写操作继续获得互斥锁）
		// 直到等待加读锁的 readerWait 为 0 后被唤醒
		runtime_SemacquireMutex(&rw.writerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// Unlock 解锁 rw 进行写入。如果 rw 未被锁定以在进入 Unlock 时写入，则是一个运行时错误。
// 与互斥锁一样，锁定的 RWMutex 不与特定的 goroutine 相关联。
// 一个 goroutine 可以 RLock（锁定）一个 RWMutex，然后安排另一个 goroutine 来 RUnlock（解锁）它。
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}

	// 将 readerCount 从负的变为正的，以提示读 goroutine 互斥锁已经释放掉，RLock 时可以加锁成功
	r := atomic.AddInt32(&rw.readerCount, rwmutexMaxReaders)
	if r >= rwmutexMaxReaders {
		race.Enable()
		// 如果没有锁定就释放互斥锁，则直接抛出错误
		throw("sync: Unlock of unlocked RWMutex")
	}
	for i := 0; i < int(r); i++ {
		// 逐一唤醒阻塞等待中的读 goroutine，可以加读锁
		runtime_Semrelease(&rw.readerSem, false, 0)
	}
	// 释放互斥锁，允许其他读 goroutine 继续获取互斥锁
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// RLocker 返回一个 Locker 接口，该接口通过调用 rw.RLock 和 rw.RUnlock.来实现 Lock 和 Unlock 方法
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
