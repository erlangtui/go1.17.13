// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// 此文件包含 Go 的 map 类型的实现。
// map 只是一个哈希表。数据被排列到一个存储桶数组中。每个桶最多包含 8 个键值对。
// 哈希值的低位用于选择存储桶。每个存储桶都包含每个哈希值的几个高阶位，以区分单个存储桶中的条目。
// 如果一个存储桶的哈希值超过 8 个，我们会链接额外的存储桶。
// 当哈希表扩容时，我们会分配一个两倍大的新存储桶数组。
// 存储桶将以翻倍更新的方式从旧存储桶数组复制到新存储桶数组。
//
// map 迭代器遍历存储桶数组，并按遍历顺序返回键（存储桶，然后是溢出链顺序，然后是存储桶索引）。
// 为了维护迭代语义，我们从不在其存储桶中移动 key（如果这样做，key可能会返回 0 或 2 次）。
// 在哈希表扩容时，迭代器会继续遍历旧表，并且必须检查它们正在遍历的存储桶是否已移动（“撤出”）到新表。
// 选择 loadFactor：太大，有很多溢流桶，太小，浪费了很多空间。
// 以下是不同负载的一些统计数据：
//  loadFactor    %overflow  bytes/entry     hitprobe    missprobe
//        4.00         2.13        20.77         3.00         4.00
//        4.50         4.05        17.30         3.25         4.50
//        5.00         6.85        14.77         3.50         5.00
//        5.50        10.55        12.94         3.75         5.50
//        6.00        15.27        11.67         4.00         6.00
//        6.50        20.90        10.79         4.25         6.50
//        7.00        27.14        10.15         4.50         7.00
//        7.50        34.03         9.73         4.75         7.50
//        8.00        41.10         9.40         5.00         8.00
//
// %overflow   = 具有溢出存储桶的存储桶百分比
// bytes/entry = 每个键值对使用的开销字节数
// hitprobe    = 查找当前 key 需要检查的条目数
// missprobe   = 查找缺失 key 需要检查的条目数
// 此数据适用于最大负载的表，即在表扩容之前。典型的表的负载会稍少一些。
import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// 每个桶能够存储的最多键值对数
	bucketCntBits = 3
	bucketCnt     = 1 << bucketCntBits

	// 平均每个桶的最大负载因子，loadFactorNum/loadFactorDen
	loadFactorNum = 13
	loadFactorDen = 2

	// 当键或值的大小小于等于此限制时，Go 语言的 map 实现会将它们直接保存在 map 结构体中，而不是为每个元素单独分配内存。
	// 如果键或值的大小超过了这个限制，在 map 内部实现时，会为每个元素单独分配内存，键和值将被存储在堆上，而不是直接存储在 map 结构中。
	// 这种设计是为了在性能和内存利用之间取得平衡。将较小的键和值直接保存在 map 结构体中可以提高访问速度，因为无需通过指针间接访问。
	// 而对于较大的键和值，为其分配单独的内存可以节省内存空间，并避免 map 结构体变得过大。
	maxKeySize  = 128 // 保证键值内联的最大尺寸
	maxElemSize = 128

	// bmap 经过编译后会扩充 keys、values 等字段，keys 是紧跟在 tophash 字段之后的
	// 通过这种方式能够计算出 keys 相对于 bmap 起始地址的偏移，也是 tophash 的大小
	dataOffset = unsafe.Offsetof(struct {
		b bmap
		v int64
	}{}.v)

	// 可能的 tophash 值
	emptyRest      = 0 // 此单元格为空，并且在较高的索引或溢出处不再有非空单元格
	emptyOne       = 1 // 此单元格为空，其他单元格未知
	evacuatedX     = 2 // 键值是有效的，已迁移到新桶的前半部分
	evacuatedY     = 3 // 键值是有效的，已迁移到新桶的后半部分
	evacuatedEmpty = 4 // 单元格为空，数据已经迁移走了
	minTopHash     = 5 // 正常填充单元格的最小顶部哈希

	// 标志位
	iterator     = 1 // 可能存在使用存储桶的迭代器
	oldIterator  = 2 // 可能存在使用旧的存储桶的迭代器
	hashWriting  = 4 // 有其他 goroutine 正在写该map
	sameSizeGrow = 8 // 当前 map 正在进行等量扩容

	// 用于迭代器检查的哨兵存储桶 ID，表示迭代过程中不需要重新检查该桶
	noCheck = 1<<(8*sys.PtrSize) - 1
)

// Go map 的头部，格式也在 cmd/compile/internal/reflect/data/reflect.go 中编码。确保这与编译器的定义保持同步。
type hmap struct {
	count      int            // map 元素个数，必须在第一位，由内置函数 len() 调用
	flags      uint8          // map 的标志，是否处于迭代器、写、扩容等状态
	B          uint8          // 存储桶的数量的对数值，桶数等于 2^B，如果在搬迁过程中则指的是新存储桶数量的对数
	noverflow  uint16         // 溢出桶的大概数量，为了保持hmap较小，noverflow 是一个uint16。
	hash0      uint32         // 哈希种子
	buckets    unsafe.Pointer // 指向存储桶数组的指针，多个桶在内存上是连续的，当 count 为 0 时，为 nil
	oldbuckets unsafe.Pointer // 在扩容时指向旧桶数组的指针，仅在扩容时不为空，扩容结束后置为空，以此判断是否处于扩容状态
	nevacuate  uintptr        // 迁移进度计数器（小于此值的索引对应的桶已清空）
	// 等量扩容时 buckets 数组与 oldbuckets 数组长度相等；翻倍扩容时，buckets 数组是 oldbuckets 数组长度的两倍

	extra *mapextra // 额外字段，主要存储溢出桶等信息
}

// mapextra 保存并非所有 map 都存在的字段
type mapextra struct {
	// 如果 key 和 elem 都不包含指针并且是内联的，那么我们将存储桶类型标记为不包含指针，这样可以避免gc扫描此类map
	// 内联存储：即 key 和 value 直接存储在哈希表的内存中，而不是存储在堆上
	// 为了使溢出桶保持活动状态，将指向所有溢出桶的指针存储在 hmap.extra.overflow 和 hmap.extra.oldoverflow 中
	// 仅当 key 和 elem 不包含指针时才使用 overflow 和 oldoverflow

	overflow     *[]*bmap // 存储所有存储桶 hmap.buckets 的溢出桶指针
	oldoverflow  *[]*bmap // 存储所有旧存储桶 hmap.oldbuckets 的溢出桶指针，扩容时才有
	nextOverflow *bmap    // 指向首个可用溢出桶的指针，在创建存储桶数组时，会额外创建多个溢出桶，这些溢出桶在内存上也是连续的
}

// 为桶 b 创建溢出桶对象
func (h *hmap) newoverflow(t *maptype, b *bmap) *bmap {
	var ovf *bmap
	if h.extra != nil && h.extra.nextOverflow != nil {
		// 已经在 makeBucketArray 函数中预分配了下一个溢出桶对象，直接使用
		ovf = h.extra.nextOverflow
		if ovf.overflow(t) == nil {
			// 详见 makeBucketArray 函数 last.setoverflow(t, (*bmap)(buckets)) 位置处
			// 还未到达最后一个预分配的溢出桶，让下一个溢出桶指针 nextOverflow 往后移
			h.extra.nextOverflow = (*bmap)(add(unsafe.Pointer(ovf), uintptr(t.bucketsize)))
		} else {
			// 已经是最后一个预分配的溢出存储桶，重置此溢出桶上的溢出指针为 nil，并将下一个溢出桶指针 nextOverflow 也置为 nil
			ovf.setoverflow(t, nil)
			h.extra.nextOverflow = nil
		}
	} else {
		// 没有可用的预先分配的溢出桶, 直接创建一个新桶
		ovf = (*bmap)(newobject(t.bucket))
	}
	h.incrnoverflow() // 增加溢出桶计数
	if t.bucket.ptrdata == 0 {
		h.createOverflow()
		*h.extra.overflow = append(*h.extra.overflow, ovf) // 将该溢出桶指针添加到溢出桶数组
	}
	b.setoverflow(t, ovf) // 将当前溢出桶地址存储到 b 的溢出桶指针上，即形成溢出桶链表
	return ovf
}

// incrnoverflow 递增溢出存储桶的计数，当存储桶很少时，递增操作直接执行，noverflow 是一个精确的计数；
// 当存储桶很多时，递增操作以一定的概率执行，noverflow 是一个近似计数
func (h *hmap) incrnoverflow() {
	// 如果溢出存储桶与存储桶一样多，将触发相同大小的 map 扩容
	if h.B < 16 {
		h.noverflow++
		return
	}
	// 当存储桶的数量达到 1<<15-1 时，溢出桶的数量大概等于存储桶，以 1/(1<<(h.B-15)) 的概率增加溢出桶
	mask := uint32(1)<<(h.B-15) - 1
	// 举例: if h.B == 18, then mask == 7, and fastrand & 7 == 0 概率为 1/8.
	if fastrand()&mask == 0 {
		h.noverflow++
	}
}

// 没有溢出桶数组时，则创建溢出桶数组
func (h *hmap) createOverflow() {
	if h.extra == nil {
		h.extra = new(mapextra)
	}
	if h.extra.overflow == nil {
		h.extra.overflow = new([]*bmap)
	}
}

// 是否正在扩容，等量扩容或翻倍扩容，即旧存储桶数组指针 oldbuckets 不为空
func (h *hmap) growing() bool {
	return h.oldbuckets != nil
}

// 当前扩容是否是等量扩容
func (h *hmap) sameSizeGrow() bool {
	return h.flags&sameSizeGrow != 0 // 即 flags 的第 4 位不为 0
}

// 当前 map 扩容之前的存储桶数
func (h *hmap) noldbuckets() uintptr {
	// B 为当前存储桶数量对数值，如果是等量扩容则与旧的相等，如果是翻倍扩容则比旧的大 1
	oldB := h.B
	if !h.sameSizeGrow() {
		oldB-- // 对数值减一，桶数量减半
	}
	return bucketShift(oldB)
}

// 返回旧存储桶数量的掩码值，方便进行位与运算，以求出 key 的哈希值对旧存储桶数量的余数，来确定桶的索引
// eg：当 B 为 5 时，旧 B 为 4，旧存储桶数量为 16，生成的掩码值为 0b1111
// 如果哈希值为 0b1101001010，直接位与运算得到 0b1010，即等于 6，桶的索引号为 6
func (h *hmap) oldbucketmask() uintptr {
	return h.noldbuckets() - 1
}

// bucketShift 返回 2^b，针对代码生成进行了优化
func bucketShift(b uint8) uintptr {
	// Masking the shift amount allows overflow checks to be elided.
	return uintptr(1) << (b & (sys.PtrSize*8 - 1))
}

// bucketMask 返回 2^b-1，即对应的掩码值，针对代码生成进行了优化
func bucketMask(b uint8) uintptr {
	return bucketShift(b) - 1
}

// tophash 计算 hash 的顶部 8 位哈希值
func tophash(hash uintptr) uint8 {
	top := uint8(hash >> (sys.PtrSize*8 - 8))
	if top < minTopHash {
		top += minTopHash
	}
	return top
}

// isEmpty 报告给定的 tophash 数组元素是否为空
func isEmpty(x uint8) bool {
	return x <= emptyOne
}

// map 的桶
type bmap struct {
	// tophash 存储此存储桶中每个键的哈希值顶部 8 字节，如果 tophash[0] < minTopHash，则 tophash[0] 是存储桶迁移状态
	tophash [bucketCnt]uint8
	// 实际存储过程中，将所有的键连续存储在一起，将所有的值连续存储在一起，能够一定程度上避免 键值/键值 存取方式的内存对齐
}

/*
// 实际编译时会将 bmap 编译成以下结构
type bmap struct {
	tophash  [8]uint8     // 顶部哈希值数组
	keys     [8]keytype   // key 数组
	values   [8]valuetype // values 数组
	pad      uintptr      // 填充字段
	overflow uintptr      // 溢出桶指针
}
*/

// 获取当前桶的溢出桶指针
func (b *bmap) overflow(t *maptype) *bmap {
	// 指针运算，桶指针往后移动一个桶大小的距离，再往前移动一个指针的距离，就是其溢出桶指针的地址
	return *(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize))
}

// 将溢出桶的地址存储到当前桶的溢出桶指针上
func (b *bmap) setoverflow(t *maptype, ovf *bmap) {
	*(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize)) = ovf
}

// 获取当前桶的 keys 地址
func (b *bmap) keys() unsafe.Pointer {
	// 桶地址往后偏移 dataOffset
	return add(unsafe.Pointer(b), dataOffset)
}

// 通过顶部哈希值判断某个桶是否已经迁移走，true 表示迁移完成
func evacuated(b *bmap) bool {
	h := b.tophash[0]
	return h > emptyOne && h < minTopHash
}

// makemap_small 为 make(map[k]v)  和 make(map[k]v, hint) 实现 Go map 创建
// 前提是在编译时已知 hint 最多为 bucketCnt，并且需要在堆上分配映射
func makemap_small() *hmap {
	h := new(hmap)
	h.hash0 = fastrand()
	// 桶的创建是在 mapassign 中完成的
	return h
}

func makemap64(t *maptype, hint int64, h *hmap) *hmap {
	if int64(int(hint)) != hint {
		hint = 0
	}
	return makemap(t, int(hint), h)
}

// makemap make(map[k]v, hint) 的实现，如果编译器已经检测到能够在栈上创建 map 或第一个桶，则 h 或 bucket 非空
// 如果 h 非空，则能够直接在 h 上创建 map, 如果 h.buckets 非空，则指向的 bucket 可以用作第一个存储桶
func makemap(t *maptype, hint int, h *hmap) *hmap {
	// 计算需要申请的内存大小，并进行校验
	mem, overflow := math.MulUintptr(uintptr(hint), t.bucket.size)
	if overflow || mem > maxAlloc {
		hint = 0
	}

	// 初始化 hmap
	if h == nil {
		h = new(hmap)
	}
	h.hash0 = fastrand()

	// 确定 B 的值，以保证 2^B 个桶能够装下 hint 个元素，且负载因子在约定范围内
	B := uint8(0)
	for overLoadFactor(hint, B) {
		B++ // 桶的数量翻倍
	}
	h.B = B

	// 分配哈希表，如果 B == 0，则稍后会延迟分配 buckets 字段（在 mapassign 中） 如果 hint 很大，则将此内存归零可能需要一段时间
	if h.B != 0 {
		var nextOverflow *bmap
		// 获取存储存储桶数组指针和下一个溢出桶指针
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil)
		if nextOverflow != nil {
			h.extra = new(mapextra)
			h.extra.nextOverflow = nextOverflow
		}
	}

	return h
}

// makeBucketArray 用于创建一个存储桶数组，并返回该数组的地址和下一个溢出桶的地址
// 该存储桶数组在内存上是连续的，溢出桶在内存上也是连续的只是返回首个桶的地址
// 2^b 是需要分配存储桶的最小数量，当 b<4 时，溢出的可能性不大，不会创建溢出桶
// 当 b>=4 时，会总共申请 2^b+2^b/16 个桶，由于内存申请策略会向上对齐，实际申请的内存可能足以放下更多的桶
// 此时，取前 2^b 个桶作为存储桶，后面的为溢出桶，buckets 指向存储桶数组首位地址，nextOverflow 指向首位溢出桶
// dirtyalloc 应该是 nil 或指向之前由 makeBucketArray 以相同 t 和 b 参数分配的存储桶数组
// 如果 dirtyalloc 是 nil 则会重新申请内存分配一个新的存储桶数组，否则 dirtyalloc 指向的数组将会被清理掉并被重新被复用为底层数组
func makeBucketArray(t *maptype, b uint8, dirtyalloc unsafe.Pointer) (buckets unsafe.Pointer, nextOverflow *bmap) {
	base := bucketShift(b)
	nbuckets := base
	// 实际分配桶的数量是 nbuckets，是不小于 base，前 base 个桶依旧为存储桶，base 之后的用作溢出桶
	// 这些桶在内存上是连续的，每个桶的溢出桶指针是 nil，最后一个桶的溢出桶指针记为非 nil，以标记该桶是最后一个溢出桶了

	if b >= 4 { // 对于小 b，溢出桶的可能性不大，默认不创建溢出桶，添加该条件能够避免计算的开销
		nbuckets += bucketShift(b - 4) // 额外申请 1/16 存储桶数量的桶，2^b+2^b/16
		sz := t.bucket.size * nbuckets // nbuckets 个桶所需的内存大小
		up := roundupsize(sz)          // 将其对齐到最近的较大的2的幂次方数值
		if up != sz {                  // 如果对齐后的值与原始值不相等，则用对齐后的内存大小计算实际的桶数 nbuckets
			nbuckets = up / t.bucket.size
		}
	}

	if dirtyalloc == nil {
		// 直接分配一个新的底层数组
		buckets = newarray(t.bucket, int(nbuckets))
	} else {
		// 复用 dirtyalloc 指向的原数组，并将该数组内存清空，t 和 b 与申请 dirtyalloc 时相同，所以原数组大小是满足条件的
		buckets = dirtyalloc
		size := t.bucket.size * nbuckets
		if t.bucket.ptrdata != 0 {
			// 桶中的元素类型带有指针时，数组内存的清空方式
			memclrHasPointers(buckets, size)
		} else {
			// 桶中的元素类型没有指针时，数组内存的清空方式
			memclrNoHeapPointers(buckets, size)
		}
	}

	if base != nbuckets {
		// 预先分配了一些溢出桶，为了将跟踪这些溢出桶的开销降至最低，我们使用这样的约定:
		// 如果预分配的溢出桶的溢出指针为 nil，则通过向后移动指针可以获得更多可用的溢出桶；
		// 对于最后一个桶的溢出桶的溢出指针，需要一个安全的非空指针，直接用 buckets，以区分该桶为最后一个溢出桶；

		// 前 0 ~ base-1 个桶为存储桶，base ~ nbuckets-1 个桶为溢出桶，这些桶在内存上连续
		// 通过调用add函数计算下一个溢出桶 nextOverflow 的地址，并将其转换为*bmap类型的指针
		nextOverflow = (*bmap)(add(buckets, base*uintptr(t.bucketsize)))
		// 通过调用add函数计算出最后一个桶的地址，并将其转换为*bmap类型的指针
		last := (*bmap)(add(buckets, (nbuckets-1)*uintptr(t.bucketsize)))
		// 将 buckets 强制转换为*bmap类型的指针，并赋值给最后一个桶的溢出指针
		// 即第 nbuckets-1 个桶的溢出桶指针记为非 nil，以标记该桶是最后一个桶了，会在 newoverflow 函数中用到
		last.setoverflow(t, (*bmap)(buckets))
	}
	return buckets, nextOverflow
}

// mapaccess1 返回指向 h[key] 的指针，从不返回 nil，相反，如果键不在 map 中，它将返回对 elem 类型的零值对象的引用
// 返回的指针可能会使整个 map 保持活动状态，因此不要长时间保留它
func mapaccess1(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapaccess1)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}
	if h == nil || h.count == 0 {
		if t.hashMightPanic() { // hash 函数是否可能引发 panic
			t.hasher(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0])
	}
	if h.flags&hashWriting != 0 {
		// 并发读写，抛出错误
		throw("concurrent map read and map write")
	}
	hash := t.hasher(key, uintptr(h.hash0)) // 计算 key 的哈希值
	m := bucketMask(h.B)                    // 返回桶的数量减一
	// hash&m 计算哈希值对桶数量的余数，即哈希值的后 B 位对应的值
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.bucketsize))) // 计算该 key 经过哈希后应该落入的桶的地址
	if c := h.oldbuckets; c != nil {
		//  正在扩容中
		if !h.sameSizeGrow() {
			// 如果是翻倍扩容，旧容量的大小应该是现在的一半，即需要计算哈希值的后 B-1 位
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {
			// 当前旧桶没有迁移走，则直接选择旧桶
			b = oldb
		}
	}
	top := tophash(hash) // 计算顶部 8 位哈希值
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		// 从旧桶开始沿着溢出桶依次遍历
		for i := uintptr(0); i < bucketCnt; i++ {
			// 对每个桶依次遍历
			if b.tophash[i] != top {
				// 在桶中依次比较顶部哈希值，如果顶部哈希值不相等，则跳过当前值
				if b.tophash[i] == emptyRest {
					// 如果顶部哈希值为 emptyRest，则说明整个桶都为空，直接跳出当前桶
					break bucketloop
				}
				continue
			}
			// 顶部哈希值相等，根据顶部哈希值索引 i 计算该哈希值对应的 key 的地址
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				// 如果是存储 key 的指针而不是 key 本身，则解引用
				k = *((*unsafe.Pointer)(k))
			}
			if t.key.equal(key, k) {
				// 如果桶中存储的 key 与待查找的 key 相等，则计算该 key 对应的值的地址
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				if t.indirectelem() {
					// 如果存储 elem 的指针而不是 elem 本身，则解引用
					e = *((*unsafe.Pointer)(e))
				}
				// 找到 key 对应的 elem，直接返回
				return e
			}
			// 如果桶中存储的 key 与待查找的 key 相等，继续查找下一个元素
		}
		// 继续查找下一个桶，直至溢出桶结束
	}
	// 在 key 哈希值对应的桶及其溢出桶都没有找到，则直接返回零值指针
	return unsafe.Pointer(&zeroVal[0])
}

// mapaccess2 与 mapaccess1 相比，多返回的一个 bool 值，true 表示取到了真实的值而不是零值，false 表示取到了零值
func mapaccess2(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, bool) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapaccess2)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.hasher(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0]), false
	}
	if h.flags&hashWriting != 0 {
		throw("concurrent map read and map write")
	}
	hash := t.hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.bucketsize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.key.equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				if t.indirectelem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e, true
			}
		}
	}
	return unsafe.Pointer(&zeroVal[0]), false
}

// mapaccessK 与 mapaccess1 相比，同时返回了 key 和 elem 的指针，用于 map 迭代器
func mapaccessK(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, unsafe.Pointer) {
	if h == nil || h.count == 0 {
		return nil, nil
	}
	hash := t.hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.bucketsize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.key.equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				if t.indirectelem() {
					e = *((*unsafe.Pointer)(e))
				}
				return k, e
			}
		}
	}
	return nil, nil
}

func mapaccess1_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) unsafe.Pointer {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero
	}
	return e
}

func mapaccess2_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) (unsafe.Pointer, bool) {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero, false
	}
	return e, true
}

// mapassign 与 mapaccess 类似，但是如果 map 中没有该 key 时，会为其分配一个槽
func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if h == nil {
		// map 为 nil，直接 panic，无法向一个 nil 中写数据
		panic(plainError("assignment to entry in nil map"))
	}
	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(mapassign)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled {
		msanread(key, t.key.size)
	}
	if h.flags&hashWriting != 0 {
		// 并发写抛出错误
		throw("concurrent map writes")
	}
	hash := t.hasher(key, uintptr(h.hash0))

	// 调用 t.hasher 再后设置 hashWriting，因为 t.hasher 可能会 panic，在这种情况下，实际上还没有进行写入
	h.flags ^= hashWriting

	if h.buckets == nil {
		// 如果桶为空，则创建一个新的桶，初始状态为一个桶
		h.buckets = newobject(t.bucket) // newarray(t.bucket, 1)
	}

again:
	bucket := hash & bucketMask(h.B) // 计算该 key 对应的桶的索引号
	if h.growing() {
		// 如果 map 处于扩容过程中，则迁移该桶及其后续未迁移的桶（如果还有的话）
		growWork(t, h, bucket)
	}
	// 直接从存储桶数组 h.buckets 中获取 bucket 索引对应的桶（如果在搬迁，该存储桶数组是新的，h.B 也是新存储桶数组的长度对数）
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.bucketsize)))
	top := tophash(hash)

	var inserti *uint8         // 顶部哈希值数组中空单元格的地址
	var insertk unsafe.Pointer // key 数组中空单元格的地址
	var elem unsafe.Pointer    // elem 数组中空单元格的地址或是 k 对应的 elem 的地址

bucketloop: // 从桶中找到当前 k，或是找到一个空的单元格
	for {
		// 依次遍历存储桶及其溢出桶，在桶中依次遍历每个顶部哈希值、key、elem
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				// 顶部哈希值不相等，即不是要找的 k
				if isEmpty(b.tophash[i]) && inserti == nil {
					// 如果当前 b.tophash[i] 为空，且 inserti 为空，则记录当前 i 对应的 key、elem 对应的地址
					// 即 inserti 用于存储第一个为空的顶部哈希值数组单元格的地址
					inserti = &b.tophash[i]
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
					elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				}
				if b.tophash[i] == emptyRest {
					// 如果此单元格为空，并且在较高的索引或溢出处不再有非空单元格，则跳出大循环 bucketloop
					break bucketloop
				}
				// 如果此空单元格之后还有其他的非空单元格，继续往后遍历其他单元格
				continue
			}
			// 顶部哈希值相等，可能是要找到的 k，取 i 对应的位置的 key
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				// 存储的是指针，解引用
				k = *((*unsafe.Pointer)(k))
			}
			// 判断存储的 key 是否与查找的 k 相等
			if !t.key.equal(key, k) {
				// 不相等，继续往后遍历其他单元格
				continue
			}
			// key 相等，找到了该 key
			if t.needkeyupdate() {
				// 如果需要更新该 key 则直接更新
				typedmemmove(t.key, k, key)
			}
			// 获取该 key 对应的 elem 的地址，跳转到 done
			elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
			goto done
		}
		// 如果在该桶中既没有找到当前 k，也没有找到空的单元格或找到的空单元格后较高的索引或溢出处有非空单元格
		// 则继续往后遍历其他溢出桶
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// 执行到此处，说明没有找到相等的 key
	// 如果命中了负载因子阈值或是有太多的溢出桶，并且又没有正在扩容，则开始扩容
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		// 开始扩容，创建存储桶数组和溢出桶数组，并分布切换旧桶数组指针和旧溢出桶数组指针的指向，不做实际搬迁
		hashGrow(t, h)
		// 开始扩容后，存储桶数组地址发生改变，数组长度可能也会发生改变，需要跳转到 again 重新开始查找
		goto again
	}

	if inserti == nil {
		// 没有找到相等的 key，且 inserti 又为 nil，则说明当前桶及其溢出桶已经满了，直接创建一个新的溢出桶
		newb := h.newoverflow(t, b)
		inserti = &newb.tophash[0]
		insertk = add(unsafe.Pointer(newb), dataOffset)
		elem = add(insertk, bucketCnt*uintptr(t.keysize))
	}

	if t.indirectkey() {
		// 存储的是 key 指针，则从堆上申请内存，并赋值地址
		kmem := newobject(t.key)
		*(*unsafe.Pointer)(insertk) = kmem
		insertk = kmem
	}
	if t.indirectelem() {
		// 存储的是 elem 指针，则从堆上申请内存，并赋值地址
		vmem := newobject(t.elem)
		*(*unsafe.Pointer)(elem) = vmem
	}
	// 执行到此处说明是 map 中新增了元素，记录 key、top，并且 map 中的元素数量加一
	typedmemmove(t.key, insertk, key)
	*inserti = top
	h.count++

done: // 直接跳转到此处说明是找到了已经存储的 key
	if h.flags&hashWriting == 0 {
		// 有其他 goroutine 正在写，抛出错误
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting // 清除写标志
	if t.indirectelem() {
		elem = *((*unsafe.Pointer)(elem))
	}
	// 返回 value 的地址，直接更新或赋值
	// TODO 此处了已经解除了写标志，是否存在在更新过程中有其他 goroutine 正在读写的问题？
	return elem
}

// 从 map 中删除某个 key
func mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapdelete)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}

	// map 为 nil 或是没有 key 直接返回
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.hasher(key, 0) // see issue 23734
		}
		return
	}
	// 已经有其他 goroutine 正在写，直接抛出错误
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	// 计算哈希值
	hash := t.hasher(key, uintptr(h.hash0))

	// 调用 t.hasher 再后设置 hashWriting，因为 t.hasher 可能会 panic，在这种情况下，实际上还没有进行写入
	h.flags ^= hashWriting

	bucket := hash & bucketMask(h.B) // 计算该 key 对应的桶的索引号
	if h.growing() {
		// 如果 map 处于扩容过程中，则迁移该桶及其后续未迁移的桶（如果还有的话）
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.bucketsize)))
	bOrig := b
	top := tophash(hash)
search: // 依次遍历当前桶及其溢出桶
	for ; b != nil; b = b.overflow(t) {
		// 依次遍历桶中的每个单元格
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				// 顶部哈希值不相等
				if b.tophash[i] == emptyRest {
					// 此单元格为空，并且在较高的索引或溢出处不再有非空单元格，说明没有该 key 直接 跳出
					break search
				}
				// 还有其他的顶部哈希值，继续往后查找其他单元格
				continue
			}
			// 顶部哈希值相等，可能能够找到
			// 获取 key 单元格的地址
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			k2 := k
			if t.indirectkey() {
				k2 = *((*unsafe.Pointer)(k2))
			}
			if !t.key.equal(key, k2) {
				// 存储的 key 与实际待查找的 key 不相等，继续往后查找其他单元格
				continue
			}
			// 存储的 key 相等，找到了，清除存储的 key
			if t.indirectkey() {
				*(*unsafe.Pointer)(k) = nil
			} else if t.key.ptrdata != 0 {
				memclrHasPointers(k, t.key.size)
			}
			// 获取 elem 的单元格地址，并清除 elem
			e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
			if t.indirectelem() {
				*(*unsafe.Pointer)(e) = nil
			} else if t.elem.ptrdata != 0 {
				memclrHasPointers(e, t.elem.size)
			} else {
				memclrNoHeapPointers(e, t.elem.size)
			}
			b.tophash[i] = emptyOne // 将顶部哈希值置为 emptyOne
			if i == bucketCnt-1 {
				// 如果位于当前桶的顶部哈希值的最后一个单元格
				if b.overflow(t) != nil && b.overflow(t).tophash[0] != emptyRest {
					// 如果溢出桶不为空且溢出桶的顶部哈希值第一个单元格不为 emptyRest，跳转到 notLast
					goto notLast
				}
			} else {
				// 如果不是位于当前桶的顶部哈希值的最后一个单元格
				if b.tophash[i+1] != emptyRest {
					// 且下一个单元格中顶部哈希值不为 emptyRest，则跳转
					goto notLast
				}
			}
			// 运行到此处说明：
			// 如果位于当前桶的顶部哈希值的最后一个单元格，要么没有溢出桶，要么溢出桶的顶部哈希值第一个单元格为 emptyRest，即下一个溢出桶单元格都为空
			// 如果不是位于当前桶的顶部哈希值的最后一个单元格，那么下一个单元格的顶部哈希值为 emptyRest，则下一个及其之后都为空
			for {
				// 当前索引 i 对应的 key、elem 都已经清除了，i 之后的也为空，所以顶部哈希值数组 i 处单元格可以置为 emptyRest
				b.tophash[i] = emptyRest
				// 继续向前查找其他单元格
				if i == 0 {
					// 如果已经的第一个单元格，则找前一个桶
					if b == bOrig {
						// 如果当前桶已经是第一个桶（存储桶），直接退出将其他单元格的顶部哈希值置为 emptyRest 的操作
						break
					}
					c := b
					for b = bOrig; b.overflow(t) != c; b = b.overflow(t) {
						// 	从第一个桶 bOrig 找到当前桶的前一个桶
					}
					// b 现在为前一个桶，将 i 置为顶部哈希值数组的最后一个单元格索引
					i = bucketCnt - 1
				} else {
					// 如果不是第一个单元格，直接往前移一个单元格
					i--
				}
				if b.tophash[i] != emptyOne {
					// 如果这个单元格不为空，直接退出将其他单元格的顶部哈希值置为 emptyRest 的操作
					break
				}
				// 	这个单元格为空，后面的也都为空，下次循环时将其顶部哈希值置为 emptyRest
			}
		notLast:
			h.count-- // 数量减一
			if h.count == 0 {
				// 数量为 0 时，重置哈希种子以防止攻击 See issue 25237.
				h.hash0 = fastrand()
			}
			break search // 找到了，跳出查找
		}
	}

	if h.flags&hashWriting == 0 {
		// 并发写，抛出错误
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting // 清除写标志
}

// map 的迭代器结构，如果修改 hiter，还要更改 cmd/compile/internal/reflect/data/reflect.go 以指示此结构的布局
type hiter struct {
	key         unsafe.Pointer // 必须首位，置为 nil 时表示到了 map 的末尾 (see cmd/compile/internal/walk/range.go).
	elem        unsafe.Pointer // 必须第二位 (see cmd/compile/internal/walk/range.go).
	t           *maptype       // 要遍历的 map 的类型
	h           *hmap          // 要遍历的 map
	buckets     unsafe.Pointer // map 的存储桶，在初始化 hiter 时被赋值
	bptr        *bmap          // 当前桶的指针
	overflow    *[]*bmap       // 记录溢出桶数组指针
	oldoverflow *[]*bmap       // 记录旧溢出桶数组指针
	startBucket uintptr        // 迭代器开始遍历的桶的序号
	offset      uint8          // 迭代器在桶中开始遍历的单元格的序号
	wrapped     bool           // 是否已经月过存储桶数组末尾，绕了一圈到头部了
	B           uint8          // map 存储桶长度的对数值
	i           uint8          // 当前遍历的单元格序号
	bucket      uintptr        // 当前遍历的桶的序号
	checkBucket uintptr        // 因为扩容需要检查的桶的序号
}

// mapiterinit 初始化用于在 map 上迭代的迭代器 hiter 结构
// “it”指向的迭代器结构由编译器顺序传递在栈上分配，或由reflect_mapiterinit在堆上分配。由于结构体包含指针，因此两者都需要有零值的迭代器
func mapiterinit(t *maptype, h *hmap, it *hiter) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(mapiterinit))
	}

	// map 为空或是没有元素，直接返回
	if h == nil || h.count == 0 {
		return
	}

	// hiter 结构体内存对齐后应该是 12 个sys.PtrSize 字节大小
	if unsafe.Sizeof(hiter{})/sys.PtrSize != 12 {
		throw("hash_iter size incorrect") // see cmd/compile/internal/reflectdata/reflect.go
	}
	it.t = t
	it.h = h

	// 抓取存储桶状态的快照
	it.B = h.B
	it.buckets = h.buckets
	if t.bucket.ptrdata == 0 {
		// This preserves all relevant overflow buckets alive even if the table grows and/or overflow buckets are added to the table while we are iterating.
		// 分配当前切片并记住指向当前和旧的指针。这样可以使所有相关的溢出存储桶保持活动状态，即使表扩容，并且在我们迭代时将溢出存储桶添加到表中也是如此。
		h.createOverflow()
		it.overflow = h.extra.overflow
		it.oldoverflow = h.extra.oldoverflow
	}

	// 决定从何处开始迭代
	r := uintptr(fastrand()) // 创建随机数
	if h.B > 31-bucketCntBits {
		r += uintptr(fastrand()) << 31
	}
	it.startBucket = r & bucketMask(h.B)          // 确定开始迭代的桶的索引
	it.offset = uint8(r >> h.B & (bucketCnt - 1)) // 确定桶中开始迭代的索引

	// 迭代器当前遍历的桶
	it.bucket = it.startBucket

	// 可以与其他迭代器 mapiterinit() 同时运行
	if old := h.flags; old&(iterator|oldIterator) != iterator|oldIterator {
		atomic.Or8(&h.flags, iterator|oldIterator)
	}

	mapiternext(it)
}

func mapiternext(it *hiter) {
	h := it.h
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(mapiternext))
	}
	// 有其他 goroutine 正在写，抛出错误
	if h.flags&hashWriting != 0 {
		throw("concurrent map iteration and map write")
	}
	t := it.t
	bucket := it.bucket // 当前遍历的溢出桶序号，第一次执行 mapiternext 是迭代器开始遍历的桶序号
	b := it.bptr        // 当前桶地址，未赋值默认为空
	i := it.i
	checkBucket := it.checkBucket

next:
	if b == nil {
		// b 为 nil，表示第一个要遍历的桶
		if bucket == it.startBucket && it.wrapped {
			// bucket 等于遍历的起始桶序号，并且已经绕了一圈了，说明其他桶都已经遍历过了，直接退出
			it.key = nil
			it.elem = nil
			return
		}
		if h.growing() && it.B == h.B {
			// 迭代器是在扩容过程中启动的，扩容尚未完成
			// bucket 是新存储桶数组中的索引，oldbucket 是 bucket 对应在旧存储桶数组中的索引，bucket 与旧存储桶数量的掩码值与运算能够得到 oldbucket
			// 扩容过程中他们是对应的，eg：bucket 为 6，B 为 3，2^B=8；如果是等量扩容，old B 也为 3，oldbucket 也为 6，即等量扩容时旧 6 号桶迁移到新 6 号桶；
			// 如果是翻倍扩容，old B 为 2，2^B=4, oldbucket = 6%4 = 2，2+4=6，即旧 2 号桶迁移到新 2 或 6 号桶，新 6 号桶中的数据只能来自旧 2 号桶；
			oldbucket := bucket & it.h.oldbucketmask()
			// 获取 oldbucket 对应的旧存储桶
			b = (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize)))
			if !evacuated(b) {
				// 旧桶 b 未迁移走，要遍历的对象 b 为旧桶，记录该旧桶如果迁移时对应的新存储桶索引
				checkBucket = bucket
			} else {
				// 旧桶 b 已经迁移到新桶了，要遍历的对象 b 为新桶，获取 bucket 对应的新存储桶
				// 如果查看的存储桶尚未填充（即旧存储桶尚未撤离），那么我们需要遍历旧存储桶，并且仅返回将迁移到此存储桶的存储桶。
				b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize)))
				checkBucket = noCheck
			}
		} else {
			// 获取对应 bucket 的新存储桶
			b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize)))
			checkBucket = noCheck
		}
		bucket++ // 桶索引号自增
		if bucket == bucketShift(it.B) {
			bucket = 0        // 已经到了最后一个桶了，重置为 0 从头开始
			it.wrapped = true // 置 wrapped 为 true，表示已经饶了一圈
		}
		i = 0
	}
	// 以上确定了要遍历的桶 b
	// i 遍历的元素个数，从 0 到 bucketCnt-1
	for ; i < bucketCnt; i++ {
		// 依次遍历桶内每个单元格
		offi := (i + it.offset) & (bucketCnt - 1) // 桶内单元格的偏移量
		if isEmpty(b.tophash[offi]) || b.tophash[offi] == evacuatedEmpty {
			// 如果单元格为空或是数据已经迁移走了，跳过
			// TODO: emptyRest 在这里很难使用，因为是在存储桶中间开始迭代。是可行的，只是很棘手。
			continue
		}
		// 获取 key 的地址
		k := add(unsafe.Pointer(b), dataOffset+uintptr(offi)*uintptr(t.keysize))
		if t.indirectkey() {
			k = *((*unsafe.Pointer)(k))
		}
		// 获取 elem 的地址
		e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+uintptr(offi)*uintptr(t.elemsize))
		if checkBucket != noCheck && !h.sameSizeGrow() {
			// checkBucket 是当前要遍历的新桶索引号，其所有数据来自旧桶 b，如果该旧桶 b 还未进行迁移
			// 并且 map 正在进行翻倍扩容，那么实际应该遍历的数据是旧桶 b 中将要迁移到索引号 checkBucket 对应的新桶的那部分键值对
			// 因为在翻倍扩容过程中，旧桶 b 中的数据会同时迁往两个不同的新桶，所以在对某个新桶进行遍历时，
			// 如果该新桶中的数据还没有迁移过来，那么只需要遍历该新桶对应的旧桶中将要迁移到这个新桶的那部分数据
			if t.reflexivekey() || t.key.equal(k, k) {
				// 如果 key 是相等的，计算 key 哈希值，并判断其是否会迁移到 checkBucket 对应的新桶，否则过滤
				hash := t.hasher(k, uintptr(h.hash0))
				if hash&bucketMask(it.B) != checkBucket {
					continue
				}
			} else {
				// key 是 math.NaN()，每次计算出来的哈希值不一样，与 evacuate 中的迁移逻辑一样，让低位的 tophash 来决定迁移目的桶
				// 对于翻倍扩容，使用 x 还是 y，取决于 key 的哈希值第 B 位是 0 还是 1，从右到左第 0 位开始
				// useY = uintptr(b.tophash[offi]&1) 表示是否搬到 y 桶
				// checkBucket>>(it.B-1) 表示该桶是否是 y 桶，对于具体的桶，该值是一样的，useY 是随 key 变化的
				if checkBucket>>(it.B-1) != uintptr(b.tophash[offi]&1) {
					continue
				}
			}
		}
		if (b.tophash[offi] != evacuatedX && b.tophash[offi] != evacuatedY) ||
			!(t.reflexivekey() || t.key.equal(k, k)) {
			// 如果数据还没有被迁移，或是因此 key!=key 该条目无法被删除或更新，可以将其返回？
			it.key = k
			if t.indirectelem() {
				e = *((*unsafe.Pointer)(e))
			}
			it.elem = e
		} else {
			// 如果该 key 已经迁移到 x 或是 y 桶，并且 key 是相等的
			// key 可能已经被删除、更新或删除并重新插入
			rk, re := mapaccessK(t, h, k)
			if rk == nil {
				continue // key has been deleted
			}
			it.key = rk
			it.elem = re
		}
		it.bucket = bucket // 记录当前遍历的桶的序号，以便下次遍历
		if it.bptr != b {  // 避免不必要的写屏障，see issue 14921
			it.bptr = b
		}
		it.i = i + 1
		it.checkBucket = checkBucket
		return // 获取到 key、value，返回，等待下一次 mapiternext 再继续遍历
	}
	b = b.overflow(t) // 当前桶已经遍历完，遍历其溢出桶
	i = 0
	goto next
}

// mapclear 从 map 中删除所有的 key
func mapclear(t *maptype, h *hmap) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapclear)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
	}

	// 如果 map 为 nil 或是没有元素，直接返回
	if h == nil || h.count == 0 {
		return
	}

	// 有其他 goroutine 正在写，抛出错误
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	h.flags ^= hashWriting   // 按位与添加写标志
	h.flags &^= sameSizeGrow // 按位清除等量扩容标记
	h.oldbuckets = nil       // 旧桶数组指针清空
	h.nevacuate = 0          // 迁移进度归零
	h.noverflow = 0          // 溢出桶数量归零
	h.count = 0              // 桶中元素数量归零

	// 重置哈希种子，使攻击者更难重复触发哈希冲突 See issue 25237.
	h.hash0 = fastrand()

	// 保留 mapextra 分配，但清除所有额外信息
	if h.extra != nil {
		*h.extra = mapextra{}
	}

	// makeBucketArray 清除 h.buckets 指向的内存，并通过生成任何溢出的存储桶来恢复它们，就像 h.buckets 是新分配的一样
	_, nextOverflow := makeBucketArray(t, h.B, h.buckets)
	if nextOverflow != nil {
		// 如果创建了溢出存储桶，则在初始存储桶创建期间将分配 h.extra
		h.extra.nextOverflow = nextOverflow
	}

	if h.flags&hashWriting == 0 {
		// hashWriting 位被其他 goroutine 置为 0 了，抛出错误
		throw("concurrent map writes")
	}
	// 清除 hashWriting 位标志
	h.flags &^= hashWriting
}

// 开始扩容，只创建存储桶数组和溢出桶数组，并分布切换旧桶数组指针和旧溢出桶数组指针的指向，不做实际搬迁
func hashGrow(t *maptype, h *hmap) {
	// 如果命中了负载的因子，则翻倍扩容，否则就是有太多的溢出桶，做等量扩容
	bigger := uint8(1)
	if !overLoadFactor(h.count+1, h.B) {
		// 没有超过负载因子，等量扩容
		bigger = 0
		h.flags |= sameSizeGrow
	}
	oldbuckets := h.buckets
	// 创建新存储桶数组和下一个溢出桶
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

	// 清除桶的迭代器的状态
	flags := h.flags &^ (iterator | oldIterator)
	if h.flags&iterator != 0 {
		flags |= oldIterator
	}
	h.B += bigger             // 更新桶数量
	h.flags = flags           // 更新标志，如果是 iterator 则更新为 oldIterator
	h.oldbuckets = oldbuckets // 旧桶数组指针指向的是当前存储桶数组
	h.buckets = newbuckets    // 当前存储桶数组指针指向的是新创建的存储数组
	h.nevacuate = 0           // 迁移进度数置为 0
	h.noverflow = 0           // 溢出桶数量置为 0

	if h.extra != nil && h.extra.overflow != nil {
		// 旧溢出桶数组指针不为空，说明上一次扩容可能还没有完成，抛出错误
		if h.extra.oldoverflow != nil {
			throw("oldoverflow is not nil")
		}
		// 将旧溢出桶数组指针指向当前溢出桶数组，并将当前溢出桶数组指针置为 nil
		h.extra.oldoverflow = h.extra.overflow
		h.extra.overflow = nil
	}
	if nextOverflow != nil {
		if h.extra == nil {
			h.extra = new(mapextra)
		}
		// 为下一个溢出桶指针赋值
		h.extra.nextOverflow = nextOverflow
	}

	// 哈希表数据的实际搬迁在 growWork() and evacuate() 中
}

// 判断 count 元素放置在 2^B 个存储桶中是否超过负载因子
func overLoadFactor(count int, B uint8) bool {
	return count > bucketCnt && uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen)
}

// 是否有太多的溢出桶，即溢出桶的数量近似大于存储桶数量 2^B
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
	// 请注意，这些溢出桶中的大多数必须处于稀疏使用状态；如果使用密集，那么可能已经触发了翻倍扩容
	// 如果阈值太小，需要做无关的工作；如果阈值太大，map 的扩容和收缩保留了大量未使用的内存
	// "too many" 意味着与存储桶近似一样多的数量，可以参考 incrnoverflow 函数
	if B > 15 {
		B = 15
	}
	return noverflow >= uint16(1)<<(B&15)
}

// 迁移第 bucket 个桶及其溢出桶（如果有）
func growWork(t *maptype, h *hmap, bucket uintptr) {
	// 确保迁移的 oldbucket 桶与将要使用的 bucket 桶对应
	evacuate(t, h, bucket&h.oldbucketmask())

	// 经过 evacuate 迁移后，h.nevacuate 在函数 advanceEvacuationMark 更新
	if h.growing() {
		// 如果正在扩容，多迁移一个 h.nevacuate 索引处的桶；如果扩容完成，则 h.oldbuckets 为 nil，该语句进不来
		// 即每次最多只迁移 2 个 bucket
		evacuate(t, h, h.nevacuate)
	}
}

// 判断第 bucket 个桶是否已经迁移走，true 迁移完成
func bucketEvacuated(t *maptype, h *hmap, bucket uintptr) bool {
	b := (*bmap)(add(h.oldbuckets, bucket*uintptr(t.bucketsize)))
	return evacuated(b)
}

// evacDst 迁移目的地
type evacDst struct {
	b *bmap          // 当前迁移目的桶
	i int            // key/elem 将要迁往目的桶 b 中的索引 i 处
	k unsafe.Pointer // 目的桶 b 中第 i 个 key 的地址
	e unsafe.Pointer // 目的桶 b 中第 i 个 elem 的地址
}

// 完成一个存储桶及其溢出桶的数据迁移工作，它会将一个旧的 bucket 桶里面的数据分流到两个新的 bucket 桶
func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	// h.oldbuckets 桶数组中存放的是待迁移的数据
	// h.buckets  桶数组中存放的是需要从旧桶中迁移来的数据，在 hashGrow 函数中创建的
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize))) // 获取旧桶数组中第 oldbucket 个桶
	newbit := h.noldbuckets()                                        // 旧桶个数，2^B，B 是旧桶的 B
	if !evacuated(b) {                                               // 旧桶没有被迁移走

		// xy 包含 x 和 y (low 和 high) 迁移目的地，分别用于存储当前桶的前半部分数据和后半部分数据
		// 如果是等量扩容，x y 分别表示同一个新桶数组的前后半部分；
		// 如果是翻倍扩容，x y 分别表示新桶数组中前半部分和后半部分中的某个桶，桶的索引为 oldbucket、newbit+oldbucket
		var xy [2]evacDst
		x := &xy[0]
		// 等量扩容，新存储桶数组的长度是与旧桶长度相等，key 的哈希值对新存储桶数组的长度的余值是不变的，即桶的索引值不变
		// 所以直接从新桶数组的相同索引处的桶作为旧桶 oldbucket 的迁移目的地
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.bucketsize)))
		// k、e 分别指向新桶的首位 key、elem 地址
		x.k = add(unsafe.Pointer(x.b), dataOffset)
		x.e = add(x.k, bucketCnt*uintptr(t.keysize))

		if !h.sameSizeGrow() {
			// 只有翻倍扩容时才计算 y 指针，否则 GC 可能会看到错误的指针
			y := &xy[1]
			// 翻倍扩容，新桶数组的长度是 2*newbit，即两倍于旧桶长度
			// key 的哈希值对新存储桶数组的长度的余值可能是不变的，也可能是旧的余值加上 2^B（旧存储桶数组的 B 值），取决于 hash 值第 B 位是 0 还是 1，从右到左第 0 位开始
			// y 表示从新桶数组的 oldbucket+newbit 索引处的桶作为旧桶 oldbucket 的迁移目的地，以区别与 x
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.bucketsize)))
			y.k = add(unsafe.Pointer(y.b), dataOffset)
			y.e = add(y.k, bucketCnt*uintptr(t.keysize))
		}
		// x、y 分别对应的目的桶地址、key、elem 地址都已经计算完毕
		// 对于等量扩容，只能使用 x
		// 对于翻倍扩容，使用 x 还是 y，取决于 key 的哈希值第 B 位是 0 还是 1，从右到左第 0 位开始

		// 依次遍历旧桶及其溢出桶
		for ; b != nil; b = b.overflow(t) {
			k := add(unsafe.Pointer(b), dataOffset)
			e := add(k, bucketCnt*uintptr(t.keysize))
			// 依次遍历当前旧桶或溢出桶的 key、elem
			for i := 0; i < bucketCnt; i, k, e = i+1, add(k, uintptr(t.keysize)), add(e, uintptr(t.elemsize)) {
				top := b.tophash[i] // 获取当前 key 的顶部哈希值
				if isEmpty(top) {
					// 元素为空，更新顶部哈希值为已经搬走了，继续下一个
					b.tophash[i] = evacuatedEmpty
					continue
				}
				// 顶部哈希值异常，抛出错误
				if top < minTopHash {
					throw("bad map state")
				}
				k2 := k
				if t.indirectkey() {
					// 存储的是 key 的指针，解引用得到真正的key
					k2 = *((*unsafe.Pointer)(k2))
				}
				var useY uint8 // 0 或 1，默认是 0 ，即等量扩容
				if !h.sameSizeGrow() {
					// 翻倍扩容，计算哈希值，以做出迁移决策(是否需要将 key/elem 发送到 桶 x 或 桶 y)
					hash := t.hasher(k2, uintptr(h.hash0))
					if h.flags&iterator != 0 && !t.reflexivekey() && !t.key.equal(k2, k2) {
						// 有一种 key，每次对它计算 hash，得到的结果都不一样，这个 key 就是 math.NaN()，not a number，类型是 float64
						// 当它作为 map 的 key，在迁移的时候，会遇到一个问题：再次计算它的哈希值和它当初插入 map 时的计算出来的哈希值不一样
						// 此外，它是不可重复的。在迭代器存在的情况下，需要可重复性，因为迁移决策必须与迭代器做出的任何决策相匹配。
						// 幸运的是，无论哪种方式，都可以自由地发送这些 key。此外，tophash 对于这些类型的 key 毫无意义。
						// 让低位的 tophash 来决定迁移目的桶。为下一级别重新计算一个新的随机 tophash，以便这些 key 在多次扩容后均匀分布在所有存储桶中。
						useY = top & 1
						top = tophash(hash)
					} else {
						// 对于翻倍扩容，key 的哈希值对新存储桶数组的长度的余值可能是不变的，也可能是旧的余值加上 2^B（旧存储桶数组的 B 值）
						// 使用 x 还是 y，取决于 key 的哈希值第 B 位是 0 还是 1，从右到左第 0 位开始
						// eg: B=4, 2^B=16, 即二进制 10000, 如果 hash 的第 4 位不为 0 则 新余值=旧余值+16，使用后半部分的桶
						if hash&newbit != 0 {
							useY = 1
						}
					}
				}

				// 默认值校验
				if evacuatedX+1 != evacuatedY || evacuatedX^1 != evacuatedY {
					throw("bad evacuatedN")
				}

				// 根据 useY 更新旧桶的顶部哈希值，以及确定使用 x 还是 y 作为目的桶
				b.tophash[i] = evacuatedX + useY // evacuatedX + 1 == evacuatedY
				dst := &xy[useY]                 // evacuation destination

				if dst.i == bucketCnt {
					// 当前桶已经放满了，创建溢出桶，索引归零，并更新桶、key、elem 地址
					dst.b = h.newoverflow(t, dst.b)
					dst.i = 0
					dst.k = add(unsafe.Pointer(dst.b), dataOffset)
					dst.e = add(dst.k, bucketCnt*uintptr(t.keysize))
				}
				dst.b.tophash[dst.i&(bucketCnt-1)] = top // 屏蔽 dst.i 作为一种优化，以避免边界检查
				// 搬移 key 到新桶第 i 个 key 处
				if t.indirectkey() {
					*(*unsafe.Pointer)(dst.k) = k2 // copy pointer
				} else {
					typedmemmove(t.key, dst.k, k) // copy elem
				}
				// 搬移 elem 到新桶第 i 个 elem 处
				if t.indirectelem() {
					*(*unsafe.Pointer)(dst.e) = *(*unsafe.Pointer)(e)
				} else {
					typedmemmove(t.elem, dst.e, e)
				}
				// 更新目的桶中的索引、key、elem，即在目的桶中向后移
				// 这些更新可能会将这些指针越过 key 或 elem 数组的末尾，但是没关系，因为在存储桶的末端有溢出桶指针，可以防止指向存储桶的末端
				dst.i++
				dst.k = add(dst.k, uintptr(t.keysize))
				dst.e = add(dst.e, uintptr(t.elemsize))
			}
		}
		// 如果没有协程在使用旧的存储桶数组，就把该旧桶 oldbucket 清除掉，帮助gc
		if h.flags&oldIterator == 0 && t.bucket.ptrdata != 0 {
			b := add(h.oldbuckets, oldbucket*uintptr(t.bucketsize))
			// 保留 b.tophash，因为那里保持了迁移状态
			ptr := add(b, dataOffset)
			n := uintptr(t.bucketsize) - dataOffset
			memclrHasPointers(ptr, n)
		}
	}

	// 更新迁移进度
	if oldbucket == h.nevacuate {
		// 只有迁移进度等于当前桶的索引时，才会去判断是否所有桶都已经迁移完毕
		advanceEvacuationMark(h, t, newbit)
	}
}

// 增加迁移进度计数器，并在迁移结束后释放旧存储桶数组 oldbuckets 和旧存储桶的溢出桶 extra.oldoverflow。
func advanceEvacuationMark(h *hmap, t *maptype, newbit uintptr) {
	// 如果此次搬迁的 oldbucket 等于当前进度，进度加一
	h.nevacuate++
	// 实验表明，1024 至少矫枉过正一个数量级。无论如何，把它放在那里作为保护措施，以确保 O(1) 行为
	stop := h.nevacuate + 1024
	if stop > newbit {
		stop = newbit // 旧桶数组的长度
	}
	// 在旧桶数组中寻找下一个还没有迁移的桶
	for h.nevacuate != stop && bucketEvacuated(t, h, h.nevacuate) {
		h.nevacuate++
	}
	if h.nevacuate == newbit {
		// 所有的桶都已经迁移完成，将旧桶数组指针和旧溢出桶指针置空，清除同步扩容的标志
		h.oldbuckets = nil
		if h.extra != nil {
			h.extra.oldoverflow = nil
		}
		h.flags &^= sameSizeGrow
	}
}

// Reflect stubs. Called from ../reflect/asm_*.s

//go:linkname reflect_makemap reflect.makemap
func reflect_makemap(t *maptype, cap int) *hmap {
	// Check invariants and reflects math.
	if t.key.equal == nil {
		throw("runtime.reflect_makemap: unsupported map key type")
	}
	if t.key.size > maxKeySize && (!t.indirectkey() || t.keysize != uint8(sys.PtrSize)) ||
		t.key.size <= maxKeySize && (t.indirectkey() || t.keysize != uint8(t.key.size)) {
		throw("key size wrong")
	}
	if t.elem.size > maxElemSize && (!t.indirectelem() || t.elemsize != uint8(sys.PtrSize)) ||
		t.elem.size <= maxElemSize && (t.indirectelem() || t.elemsize != uint8(t.elem.size)) {
		throw("elem size wrong")
	}
	if t.key.align > bucketCnt {
		throw("key align too big")
	}
	if t.elem.align > bucketCnt {
		throw("elem align too big")
	}
	if t.key.size%uintptr(t.key.align) != 0 {
		throw("key size not a multiple of key align")
	}
	if t.elem.size%uintptr(t.elem.align) != 0 {
		throw("elem size not a multiple of elem align")
	}
	if bucketCnt < 8 {
		throw("bucketsize too small for proper alignment")
	}
	if dataOffset%uintptr(t.key.align) != 0 {
		throw("need padding in bucket (key)")
	}
	if dataOffset%uintptr(t.elem.align) != 0 {
		throw("need padding in bucket (elem)")
	}

	return makemap(t, cap, nil)
}

//go:linkname reflect_mapaccess reflect.mapaccess
func reflect_mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	elem, ok := mapaccess2(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

//go:linkname reflect_mapassign reflect.mapassign
func reflect_mapassign(t *maptype, h *hmap, key unsafe.Pointer, elem unsafe.Pointer) {
	p := mapassign(t, h, key)
	typedmemmove(t.elem, p, elem)
}

//go:linkname reflect_mapdelete reflect.mapdelete
func reflect_mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	mapdelete(t, h, key)
}

//go:linkname reflect_mapiterinit reflect.mapiterinit
func reflect_mapiterinit(t *maptype, h *hmap) *hiter {
	it := new(hiter)
	mapiterinit(t, h, it)
	return it
}

//go:linkname reflect_mapiternext reflect.mapiternext
func reflect_mapiternext(it *hiter) {
	mapiternext(it)
}

//go:linkname reflect_mapiterkey reflect.mapiterkey
func reflect_mapiterkey(it *hiter) unsafe.Pointer {
	return it.key
}

//go:linkname reflect_mapiterelem reflect.mapiterelem
func reflect_mapiterelem(it *hiter) unsafe.Pointer {
	return it.elem
}

//go:linkname reflect_maplen reflect.maplen
func reflect_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(reflect_maplen))
	}
	return h.count
}

//go:linkname reflectlite_maplen internal/reflectlite.maplen
func reflectlite_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(reflect_maplen))
	}
	return h.count
}

const maxZero = 1024 // must match value in reflect/value.go:maxZero cmd/compile/internal/gc/walk.go:zeroValSize
var zeroVal [maxZero]byte
