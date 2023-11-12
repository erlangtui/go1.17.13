// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Once 是一个对象，它将只执行一个操作。首次使用后不允许被复制
type Once struct {
	// done 表示是否已执行操作。
	// 它在结构中排在第一位，因为它在热路径中使用，整个结构体变量的地址也就是该字段的地址，无需地址偏移就可以获取该字段的值，热路径在每个呼叫站点上都内联。
	// 将 done 放在第一位允许在某些架构 （amd64386） 上更紧凑的指令，而在其他架构上更少的指令（用于计算偏移）。
	done uint32
	m    Mutex
}

// Do 当且仅当 Do 是首次为 Once 实例调用 Do 时，Do 才会调用函数 f。
// 换句话说，给定 var once Once，如果 once.Do(f) 被多次调用，只有第一次调用才会调用 f，即使 f 在每次调用中都有不同的值。
// 每个函数都需要一个新的 Once 实例才能执行。
// Do 用于必须只运行一次的初始化。config.once.Do（func（） { config.init（filename） }）
// 因为在对 f 的一次调用返回之前，对 Do 的调用不会返回，所以如果 f 导致调用 Do，它将死锁。
// 如果 f panic，Do 认为它已经返回了，未来再调用 Do 时将直接用返回而不调用 f。
func (o *Once) Do(f func()) {
	// 注意：这是 Do 的错误实现:
	//
	//	if atomic.CompareAndSwapUint32(&o.done, 0, 1) {
	//		f()
	//	}
	//
	// Do 保证当它返回时，f 已经完成。
	// 此实现不会实现该保证：给定两个同时调用，cas 的获胜者将调用 f，第二个将立即返回，而无需等待第一个调用完成，此时 f 还没有完成。
	// 这就是为什么慢速路径回落到互斥锁的原因，互斥锁能让第二个阻塞等待，获得锁后发现已经执行完再立即返回，
	// 以及为什么 atomic.StoreUint32 必须延迟到 f 返回之后，保证先执行 f 再执行原子操作，只要 f 执行了，无论是否 panic 都执行 原子操作。

	if atomic.LoadUint32(&o.done) == 0 {
		// 概述了慢速路径，以允许快速路径的内联。
		o.doSlow(f)
	}
}

func (o *Once) doSlow(f func()) {
	o.m.Lock()
	defer o.m.Unlock()
	if o.done == 0 {
		defer atomic.StoreUint32(&o.done, 1) // 即使 f panic 该 defer 也能执行成功，后续 Do 将不会再调用 f
		f()
	}
}
