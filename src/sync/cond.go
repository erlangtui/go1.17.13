// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Cond 实现了一个条件变量，它是等待或宣布事件发生的 goroutine 的集合点。
// 每个 Cond 都有一个关联的 Locker L（通常是 Mutex 或 RWMutex），
// 在更改条件和调用 Wait 方法时必须保留该 Locker L。首次使用后不得复制 Cond。
type Cond struct {
	noCopy noCopy

	// L 在等待或改变条件时持有，用于保护 notify
	L Locker

	notify  notifyList
	checker copyChecker
}

// NewCond returns a new Cond with Locker l.
func NewCond(l Locker) *Cond {
	return &Cond{L: l}
}

// Wait 原子解锁 c.L 并暂停调用的 goroutine 的执行。
// 稍后恢复执行后，Wait 在返回之前锁定 c.L。
// 与其他系统不同，除非被广播或信号唤醒，否则等待无法返回。
// 由于 c.L 在 Wait 首次恢复时未被锁定，因此当 Wait 返回时，
// 调用方通常不能假定条件为 true。相反，调用方应在循环中等待：
//
//    c.L.Lock()
//    for !condition() {
//        c.Wait()
//    }
//    ... make use of condition ...
//    c.L.Unlock()
//
func (c *Cond) Wait() {
	c.checker.check()
	t := runtime_notifyListAdd(&c.notify)
	c.L.Unlock()
	runtime_notifyListWait(&c.notify, t)
	c.L.Lock()
}

// Signal 唤醒一个等待 c 的 goroutine，如果有的话。对于调用者在调用期间持有 c.L是允许的但不是必需的。
func (c *Cond) Signal() {
	c.checker.check()
	runtime_notifyListNotifyOne(&c.notify)
}

// Broadcast 唤醒所有等待 c 的 goroutine，对于调用者在调用期间持有 c.L是允许的但不是必需的。
func (c *Cond) Broadcast() {
	c.checker.check()
	runtime_notifyListNotifyAll(&c.notify)
}

// copyChecker 将指针保留到自身以检测对象复制。
type copyChecker uintptr

func (c *copyChecker) check() {
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
		uintptr(*c) != uintptr(unsafe.Pointer(c)) {
		panic("sync.Cond is copied")
	}
}


// noCopy 可以嵌入到结构中，首次使用后不得复制，详见 https://golang.org/issues/8005#issuecomment-190753527
type noCopy struct{}

// Lock 是 'go vet' 的 -copylocks 检查器使用的 no-op。
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
