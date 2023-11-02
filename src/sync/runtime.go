// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// defined in package runtime

// Semacquire waits until *s > 0 and then atomically decrements it.
// It is intended as a simple sleep primitive for use by the synchronization
// library and should not be used directly.
func runtime_Semacquire(s *uint32)

// SemacquireMutex 与 Semacquire 类似，但用于分析有争议的互斥锁。
// 如果 lifo 为 true，则将 waiter 排在等待队列的前面。
// skipframes 是在跟踪过程中要省略的帧数，从 runtime_SemacquireMutex 的调用方开始计数。
// runtime_SemacquireMutex 函数是 Go 语言运行时（runtime）中的一个内部函数，用于实现互斥锁的获取操作。在 Go 语言中，互斥锁（Mutex）是一种同步原语，用于保护临界区资源的访问，确保同一时间只有一个线程可以访问该资源，从而避免数据竞争。
// 具体来说，runtime_SemacquireMutex 函数的作用是以非抢占方式获取互斥锁。当一个线程调用该函数时，如果互斥锁当前没有被其他线程持有，则该线程会成功获取互斥锁，并继续执行后续的代码。如果互斥锁已经被其他线程持有，那么当前线程将被阻塞，直到互斥锁变为可用状态。
// 这个函数通常在 Go 语言运行时的底层实现中使用，用于支持高效的并发编程。在具体的应用代码中一般不会直接使用到该函数，而是通过 sync.Mutex 或 sync.RWMutex 等高级抽象来管理互斥锁。
func runtime_SemacquireMutex(s *uint32, lifo bool, skipframes int)

// Semrelease atomically increments *s and notifies a waiting goroutine
// if one is blocked in Semacquire.
// It is intended as a simple wakeup primitive for use by the synchronization
// library and should not be used directly.
// If handoff is true, pass count directly to the first waiter.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_Semrelease's caller.
// Semrelease 以原子方式递增 s，并在 Semacquire 中阻塞一个等待的 goroutine。它旨在作为供同步库使用的简单唤醒原语，不应直接使用。
// 如果交接为 true，则将计数直接传递给第一个服务员。skipframes 是在跟踪过程中要省略的帧数，从 runtime_Semrelease 的调用方开始计算。
func runtime_Semrelease(s *uint32, handoff bool, skipframes int)

// See runtime/sema.go for documentation.
func runtime_notifyListAdd(l *notifyList) uint32

// See runtime/sema.go for documentation.
func runtime_notifyListWait(l *notifyList, t uint32)

// See runtime/sema.go for documentation.
func runtime_notifyListNotifyAll(l *notifyList)

// See runtime/sema.go for documentation.
func runtime_notifyListNotifyOne(l *notifyList)

// Ensure that sync and runtime agree on size of notifyList.
func runtime_notifyListCheck(size uintptr)
func init() {
	var n notifyList
	runtime_notifyListCheck(unsafe.Sizeof(n))
}

// runtime_canSpin 报道当前自旋是否有意义，链接到 runtime 中的 sync_runtime_canSpin 函数
func runtime_canSpin(i int) bool

// runtime_doSpin 执行实际自旋
func runtime_doSpin()

func runtime_nanotime() int64
