// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// defined in package runtime

// Semacquire 一直等到 s > 0，然后以原子方式递减它。它旨在作为同步库使用的简单睡眠原语，不应直接使用。
func runtime_Semacquire(s *uint32)

// 如果 lifo 为 true，则将 waiter 排在等待队列的前面。
// skipframes 是在跟踪过程中要省略的帧数，从 runtime_SemacquireMutex 的调用方开始计数。
// runtime_SemacquireMutex 阻塞等待，直到被唤醒
func runtime_SemacquireMutex(s *uint32, lifo bool, skipframes int)

// runtime_Semrelease's caller.
// Semrelease 以原子方式递增 s，并在 Semacquire 中阻塞一个等待的 goroutine。它旨在作为供同步库使用的简单唤醒原语，不应直接使用。
// 如果 handoff 为 true，则将计数直接传递给第一个服务员。skipframes 是在跟踪过程中要省略的帧数，从 runtime_Semrelease 的调用方开始计算。
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
