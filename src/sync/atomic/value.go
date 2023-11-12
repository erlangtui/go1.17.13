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
	vp := (*ifaceWords)(unsafe.Pointer(v))
	vlp := (*ifaceWords)(unsafe.Pointer(&val))
	for {
		typ := LoadPointer(&vp.typ)
		if typ == nil {
			// Attempt to start first store.
			// Disable preemption so that other goroutines can use
			// active spin wait to wait for completion; and so that
			// GC does not see the fake type accidentally.
			// 尝试启动第一次存储
			// 禁止抢占以便其他 goroutine 能够使用主动自旋等待来等待完成，这样 GC 就不会意外地看到假类型。
			runtime_procPin()
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(^uintptr(0))) {
				runtime_procUnpin()
				continue
			}
			// 第一次存储已经完成
			StorePointer(&vp.data, vlp.data)
			StorePointer(&vp.typ, vlp.typ)
			runtime_procUnpin()
			return
		}
		if uintptr(typ) == ^uintptr(0) {
			// First store in progress. Wait.
			// Since we disable preemption around the first store,
			// we can wait with active spinning.
			continue
		}
		// First store completed. Check type and overwrite data.
		// 首次存储已经完成，校验类型并重写数据
		if typ != vlp.typ {
			// 类型不一致，直接 panic
			panic("sync/atomic: store of inconsistently typed value into Value")
		}
		StorePointer(&vp.data, vlp.data)
		return
	}
}

// Swap stores new into Value and returns the previous value. It returns nil if
// the Value is empty.
//
// All calls to Swap for a given Value must use values of the same concrete
// type. Swap of an inconsistent type panics, as does Swap(nil).
func (v *Value) Swap(new interface{}) (old interface{}) {
	if new == nil {
		panic("sync/atomic: swap of nil value into Value")
	}
	vp := (*ifaceWords)(unsafe.Pointer(v))
	np := (*ifaceWords)(unsafe.Pointer(&new))
	for {
		typ := LoadPointer(&vp.typ)
		if typ == nil {
			// Attempt to start first store.
			// Disable preemption so that other goroutines can use
			// active spin wait to wait for completion; and so that
			// GC does not see the fake type accidentally.
			runtime_procPin()
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(^uintptr(0))) {
				runtime_procUnpin()
				continue
			}
			// Complete first store.
			StorePointer(&vp.data, np.data)
			StorePointer(&vp.typ, np.typ)
			runtime_procUnpin()
			return nil
		}
		if uintptr(typ) == ^uintptr(0) {
			// First store in progress. Wait.
			// Since we disable preemption around the first store,
			// we can wait with active spinning.
			continue
		}
		// First store completed. Check type and overwrite data.
		if typ != np.typ {
			panic("sync/atomic: swap of inconsistently typed value into Value")
		}
		op := (*ifaceWords)(unsafe.Pointer(&old))
		op.typ, op.data = np.typ, SwapPointer(&vp.data, np.data)
		return old
	}
}

// CompareAndSwap executes the compare-and-swap operation for the Value.
//
// All calls to CompareAndSwap for a given Value must use values of the same
// concrete type. CompareAndSwap of an inconsistent type panics, as does
// CompareAndSwap(old, nil).
func (v *Value) CompareAndSwap(old, new interface{}) (swapped bool) {
	if new == nil {
		panic("sync/atomic: compare and swap of nil value into Value")
	}
	vp := (*ifaceWords)(unsafe.Pointer(v))
	np := (*ifaceWords)(unsafe.Pointer(&new))
	op := (*ifaceWords)(unsafe.Pointer(&old))
	if op.typ != nil && np.typ != op.typ {
		panic("sync/atomic: compare and swap of inconsistently typed values")
	}
	for {
		typ := LoadPointer(&vp.typ)
		if typ == nil {
			if old != nil {
				return false
			}
			// Attempt to start first store.
			// Disable preemption so that other goroutines can use
			// active spin wait to wait for completion; and so that
			// GC does not see the fake type accidentally.
			runtime_procPin()
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(^uintptr(0))) {
				runtime_procUnpin()
				continue
			}
			// Complete first store.
			StorePointer(&vp.data, np.data)
			StorePointer(&vp.typ, np.typ)
			runtime_procUnpin()
			return true
		}
		if uintptr(typ) == ^uintptr(0) {
			// First store in progress. Wait.
			// Since we disable preemption around the first store,
			// we can wait with active spinning.
			continue
		}
		// First store completed. Check type and overwrite data.
		if typ != np.typ {
			panic("sync/atomic: compare and swap of inconsistently typed value into Value")
		}
		// Compare old and current via runtime equality check.
		// This allows value types to be compared, something
		// not offered by the package functions.
		// CompareAndSwapPointer below only ensures vp.data
		// has not changed since LoadPointer.
		data := LoadPointer(&vp.data)
		var i interface{}
		(*ifaceWords)(unsafe.Pointer(&i)).typ = typ
		(*ifaceWords)(unsafe.Pointer(&i)).data = data
		if i != old {
			return false
		}
		return CompareAndSwapPointer(&vp.data, data, np.data)
	}
}

// 禁止与允许强张， 在 runtime 中实现
// runtime_procPin 函数用于将当前的 goroutine 与所在的操作系统线程进行绑定，这意味着 goroutine 将会始终在该操作系统线程上执行
// 这种绑定关系在某些需要固定线程执行环境的场景中非常有用，比如需要与特定的操作系统资源绑定、避免线程切换开销等
func runtime_procPin()

// runtime_procUnpin 函数用于解除当前 goroutine 与操作系统线程的绑定，这样 goroutine 就可以自由地在不同的操作系统线程上执行
// 这通常用于需要动态调度 goroutine 到不同线程上执行的场景，以充分利用多核处理器和实现更好的并发性能
func runtime_procUnpin()
