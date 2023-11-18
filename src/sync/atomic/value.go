// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic

import (
	"unsafe"
)

// Value 提供一致类型化值的原子加载和存储。Value 的零值从 Load 返回 nil。
// 调用 Store 后，Value 不得被复制。首次使用后 Value 不得被复制。
type Value struct {
	v interface{}
}

// 接口的内部表达式
type ifaceWords struct {
	typ  unsafe.Pointer // 存储值的类型
	data unsafe.Pointer // 存储具体的值
}

// Load 返回由 Store 最新设置的值。如果没有为此值调用 Store，则返回 nil。
func (v *Value) Load() (val interface{}) {
	vp := (*ifaceWords)(unsafe.Pointer(v))
	typ := LoadPointer(&vp.typ)
	if typ == nil || uintptr(typ) == ^uintptr(0) {
		// 首次存储还没有完成
		return nil
	}
	data := LoadPointer(&vp.data)
	vlp := (*ifaceWords)(unsafe.Pointer(&val))
	vlp.typ = typ
	vlp.data = data
	return
}

// Store 将 Value 的值设置为 x。对给定 Value 的 Store 的所有调用都必须使用相同的具体类型的值。
// Store 不一致的类型会 panic，Store（nil） 也是如此。
func (v *Value) Store(val interface{}) {
	if val == nil {
		// 存储的值为 nil 直接p anic
		panic("sync/atomic: store of nil value into Value")
	}
	// 通过类型转换将 v 和 val 转换为指向 ifaceWords 结构体的指针，以便能够直接访问 v 和 val 的底层数据
	vp := (*ifaceWords)(unsafe.Pointer(v))
	vlp := (*ifaceWords)(unsafe.Pointer(&val))
	for {
		// 循环的目的是确保原子操作成功，避免竞态条件
		typ := LoadPointer(&vp.typ)
		if typ == nil {
			// 尝试启动第一次存储
			// 禁止抢占以便其他 goroutine 能够使用主动自旋等待来等待完成，这样 GC 就不会意外地看到假类型。
			runtime_procPin()
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(^uintptr(0))) {
				// 如果 vp 被其他 goroutine 初始化了，则继续 continue 等待
				runtime_procUnpin()
				continue
			}
			// 第一次存储已经完成，使用原子比较和交换（CompareAndSwapPointer）将 vp.typ 设置为 ^uintptr(0)，表示 vp 已经被初始化
			StorePointer(&vp.data, vlp.data)
			StorePointer(&vp.typ, vlp.typ)
			runtime_procUnpin()
			return
		}
		if uintptr(typ) == ^uintptr(0) {
			// 说明 vp 正在被其他 goroutine 初始化，此时暂时无法执行存储操作，需要继续循环等待
			continue
		}
		// 对于非首次存储，校验类型并重写数据
		if typ != vlp.typ {
			// 类型不一致，直接 panic
			panic("sync/atomic: store of inconsistently typed value into Value")
		}
		StorePointer(&vp.data, vlp.data)
		return
	}
}

// Swap 将 new 存储到 Value 中，并返回之前存储的值，如果 Value 为空，则返回 nil。
// 对给定值的 Swap 的所有调用都必须使用相同具体类型的值，不一致类型的 Swap 会 panic，Swap（nil） 也会panic
func (v *Value) Swap(new interface{}) (old interface{}) {
	if new == nil {
		panic("sync/atomic: swap of nil value into Value")
	}
	// 基本逻辑，同 Store
	vp := (*ifaceWords)(unsafe.Pointer(v))
	np := (*ifaceWords)(unsafe.Pointer(&new))
	for {
		typ := LoadPointer(&vp.typ)
		if typ == nil {
			runtime_procPin()
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(^uintptr(0))) {
				runtime_procUnpin()
				continue
			}
			StorePointer(&vp.data, np.data)
			StorePointer(&vp.typ, np.typ)
			runtime_procUnpin()
			return nil // 首次存储，说明当前 Value 为空，直接返回 nil
		}
		if uintptr(typ) == ^uintptr(0) {
			// 说明 vp 正在被其他 goroutine 初始化，此时暂时无法执行存储操作，需要继续循环等待
			continue
		}
		// 对于非首次存储，校验类型并重写数据
		if typ != np.typ {
			panic("sync/atomic: swap of inconsistently typed value into Value")
		}
		op := (*ifaceWords)(unsafe.Pointer(&old))
		// 用新值指针替换旧值指针，并返回旧值指针
		op.typ, op.data = np.typ, SwapPointer(&vp.data, np.data)
		return old
	}
}

// CompareAndSwap 对 Value 执行比较和交换操作。
// 对给定值的 CompareAndSwap 的所有调用都必须使用相同具体类型的值。
// 不一致类型的 CompareAndSwap 和 CompareAndSwap（old， nil） 一样会 panic。
func (v *Value) CompareAndSwap(old, new interface{}) (swapped bool) {
	if new == nil {
		panic("sync/atomic: compare and swap of nil value into Value")
	}
	vp := (*ifaceWords)(unsafe.Pointer(v))
	np := (*ifaceWords)(unsafe.Pointer(&new))
	op := (*ifaceWords)(unsafe.Pointer(&old))
	if op.typ != nil && np.typ != op.typ {
		// 类型为空或不一致，panic
		panic("sync/atomic: compare and swap of inconsistently typed values")
	}
	for {
		// 原子加载当前值的类型
		typ := LoadPointer(&vp.typ)
		if typ == nil {
			if old != nil {
				// 值不相等，返回 false
				return false
			}
			// 值相等，且为空，则赋新值
			runtime_procPin()
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(^uintptr(0))) {
				runtime_procUnpin()
				continue
			}
			// 旧值相等，且新值赋值成功，返回 true
			StorePointer(&vp.data, np.data)
			StorePointer(&vp.typ, np.typ)
			runtime_procUnpin()
			return true
		}
		if uintptr(typ) == ^uintptr(0) {
			// 说明 vp 正在被其他 goroutine 初始化，此时暂时无法执行存储操作，需要继续循环等待
			continue
		}
		// 当前值类型与新值类型不一致，panic
		if typ != np.typ {
			panic("sync/atomic: compare and swap of inconsistently typed value into Value")
		}
		// 原子加载获取当前值
		data := LoadPointer(&vp.data)
		var i interface{}
		(*ifaceWords)(unsafe.Pointer(&i)).typ = typ
		(*ifaceWords)(unsafe.Pointer(&i)).data = data
		if i != old {
			// 当前值不相等，直接返回 false
			return false
		}
		// 原子对比并替换值指针
		return CompareAndSwapPointer(&vp.data, data, np.data)
	}
}

// runtime_procPin 函数用于将当前的 goroutine 与所在的操作系统线程进行绑定，这意味着 goroutine 将会始终在该操作系统线程上执行
// 这种绑定关系在某些需要固定线程执行环境的场景中非常有用，比如需要与特定的操作系统资源绑定、避免线程切换开销等
func runtime_procPin()

// runtime_procUnpin 函数用于解除当前 goroutine 与操作系统线程的绑定，这样 goroutine 就可以自由地在不同的操作系统线程上执行
// 这通常用于需要动态调度 goroutine 到不同线程上执行的场景，以充分利用多核处理器和实现更好的并发性能
func runtime_procUnpin()
