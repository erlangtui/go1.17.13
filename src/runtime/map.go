// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// 此文件包含 Go 的 map 类型的实现。
// map 只是一个哈希表。数据被排列到一个存储桶数组中。每个桶最多包含 8 个键值对。
// 哈希值的低位用于选择存储桶。每个存储桶都包含每个哈希值的几个高阶位，以区分单个存储桶中的条目。
// 如果一个存储桶的哈希值超过 8 个，我们会链接额外的存储桶。
// 当哈希表扩容时，我们会分配一个两倍大的新存储桶数组。
// 存储桶将以增量更新的方式从旧存储桶数组复制到新存储桶数组。
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
	// 8，每个桶能够存储的最多键值对数
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

	// 数据偏移量应为 bmap 结构的大小，但需要正确对齐，即内存对齐后 bmap 占用的实际大小，对于 amd64p32，这意味着 64 位对齐，即使指针是 32 位。
	dataOffset = unsafe.Offsetof(struct {
		b bmap
		v int64
	}{}.v)

	// 每个桶(包括它的溢出桶，如果有的话)将在疏散状态中有全部或没有它的条目(除了在 evacuate() 方法期间，
	// 这只发生在map写入期间，因此在此期间没有其他人可以观察到地图)。
	// 可能的 tophash 值（顶部哈希值）
	emptyRest      = 0 // 此单元格为空，并且在较高的索引或溢出处不再有非空单元格。
	emptyOne       = 1 // 此单元格为空
	evacuatedX     = 2 // 键值是有效的，入口已疏散到大表的前半部分。
	evacuatedY     = 3 // 键值是有效的，入口已疏散到大表的后半部分。.
	evacuatedEmpty = 4 // 单元格为空，桶已经迁移走了
	minTopHash     = 5 // 正常填充单元格的最小顶部哈希。

	// flags
	iterator     = 1 // 可能存在使用存储桶的迭代器
	oldIterator  = 2 // 可能存在使用旧的存储桶的迭代器
	hashWriting  = 4 // 一个线程正在写该map
	sameSizeGrow = 8 // 当前map正在扩容至相同大小的map

	// 用于迭代器检查的哨兵存储桶 ID
	noCheck = 1<<(8*sys.PtrSize) - 1
)

// isEmpty 报告给定的 tophash 数组 entry 是否表示空存储桶 entry
func isEmpty(x uint8) bool {
	return x <= emptyOne
}

// Go map 的头部，格式也在 cmd/compile/internal/reflect/data/reflect.go 中编码。确保这与编译器的定义保持同步。
type hmap struct {
	count      int            // map 元素个数，必须在第一位，由内置函数 len() 调用
	flags      uint8          // 标志
	B          uint8          // 桶的数量的对数值，桶数：2^B
	noverflow  uint16         // 溢出桶的大概数量，为了保持hmap较小，noverflow 是一个uint16。
	hash0      uint32         // 哈希种子
	buckets    unsafe.Pointer // 指向桶的指针，多个桶在内存上是连续的，当 count 为 0 时，为 nil
	oldbuckets unsafe.Pointer // 在扩容时，指向旧桶的指针，仅在扩容时不为空
	nevacuate  uintptr        // 疏散进度计数器（小于此值的桶已清空）

	extra *mapextra // 额外字段
}

// mapextra 保存并非所有 map 都存在的字段
type mapextra struct {
	// 内联存储：即 key 和 value 直接存储在哈希表的内存中，而不是存储在堆上
	// 为了使溢出桶保持活动状态，将指向所有溢出桶的指针存储在 hmap.extra.overflow 和 hmap.extra.oldoverflow 中。
	// 仅当键和 elem 不包含指针时才使用 overflow 和 oldoverflow，overflow 包含 hmap.buckets 的溢出存储桶，oldoverflow 包含 hmap.oldbuckets 的溢出存储桶。
	// 间接寻址允许在命中器中存储指向切片的指针。

	// 如果 key 和 elem 都不包含指针并且是内联的，那么我们将存储桶类型标记为不包含指针，这样可以避免gc扫描此类map
	overflow     *[]*bmap // 溢出桶数组，存储所有溢出桶的指针
	oldoverflow  *[]*bmap // 扩容时，存储旧的所有溢出桶的指针
	nextOverflow *bmap    // 持有指向可用溢出存储桶的指针
}

// incrnoverflow 递增溢出存储桶的计数，当存储桶很少时，递增操作直接执行，noverflow 是一个精确的计数；当存储桶很多时，递增操作以一定的概率执行，noverflow 是一个近似计数
func (h *hmap) incrnoverflow() {
	// 如果溢出存储桶与存储桶一样多，将触发相同大小的 map 扩容
	if h.B < 16 {
		h.noverflow++
		return
	}
	// 当存储桶的数量达到 1<<15-1 时，溢出桶的数量大概等于存储桶，以 1/(1<<(h.B-15)) 的概率增加溢出桶
	mask := uint32(1)<<(h.B-15) - 1
	// Example: if h.B == 18, then mask == 7, and fastrand & 7 == 0 概率为 1/8.
	if fastrand()&mask == 0 {
		h.noverflow++
	}
}

// 为某个桶创建溢出桶对象
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
			// 已经是最后一个预分配的溢出存储桶，重置此溢出桶上的溢出指针，并将下一个溢出桶指针 nextOverflow 置为 nil
			ovf.setoverflow(t, nil)
			h.extra.nextOverflow = nil
		}
	} else {
		// 没有可用的预先分配的溢出桶, 创建一个新桶
		ovf = (*bmap)(newobject(t.bucket))
	}
	h.incrnoverflow() // 增加溢出桶计数
	if t.bucket.ptrdata == 0 {
		h.createOverflow()
		*h.extra.overflow = append(*h.extra.overflow, ovf) // 添加到溢出桶数组
	}
	b.setoverflow(t, ovf) // 将当前溢出桶地址存储到 b 的溢出桶指针上
	return ovf
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

// growing 是否正在扩容，向同尺寸的或更大尺寸的 map 扩容
func (h *hmap) growing() bool {
	return h.oldbuckets != nil
}

// sameSizeGrow 当前扩容是否是向同尺寸的 map 扩容
func (h *hmap) sameSizeGrow() bool {
	return h.flags&sameSizeGrow != 0
}

// noldbuckets 计算当前 map 扩容之前的存储桶数
func (h *hmap) noldbuckets() uintptr {
	oldB := h.B
	if !h.sameSizeGrow() {
		oldB--
	}
	return bucketShift(oldB)
}

// oldbucketmask 提供可用于计算 n % noldbuckets() 值的掩码，以方便进行位与运算
func (h *hmap) oldbucketmask() uintptr {
	return h.noldbuckets() - 1
}

// Go map 的桶
type bmap struct {
	// tophash 通常包含此存储桶中每个键的哈希值的顶部字节，如果 tophash[0] < minTopHash，则 tophash[0] 是存储桶疏散状态。
	tophash [bucketCnt]uint8
	// 将所有的键连续存储在一起，将所有的值连续存储在一起，能够避免 键值/键值 存取方式的内存对齐
}

// type bmap struct {
// 	topbits  [8]uint8
// 	keys     [8]keytype
// 	values   [8]valuetype
// 	pad      uintptr
// 	overflow uintptr
// }

// 获取当前桶的溢出桶指针
func (b *bmap) overflow(t *maptype) *bmap {
	return *(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize))
}

// 将 ovf 存储到当前桶的溢出桶指针上
func (b *bmap) setoverflow(t *maptype, ovf *bmap) {
	*(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize)) = ovf
}

// 获取 keys 的地址
func (b *bmap) keys() unsafe.Pointer {
	return add(unsafe.Pointer(b), dataOffset)
}

// 判断某个桶是否正在疏散中
func evacuated(b *bmap) bool {
	h := b.tophash[0]
	return h > emptyOne && h < minTopHash
}

// 哈希迭代结构。如果修改 hiter，还要更改 cmd/compile/internal/reflect/data/reflect.go 以指示此结构的布局。
type hiter struct {
	key         unsafe.Pointer // 必须首位 Write nil to indicate iteration end (see cmd/compile/internal/walk/range.go).
	elem        unsafe.Pointer // 必须第二位 (see cmd/compile/internal/walk/range.go).
	t           *maptype       // 要遍历的 map 的类型
	h           *hmap          // 要遍历的 map
	buckets     unsafe.Pointer // map 的常规桶，在初始化 hiter 时被赋值
	bptr        *bmap          // current bucket
	overflow    *[]*bmap       // 记录溢出桶数组指针
	oldoverflow *[]*bmap       // 记录旧溢出桶数组指针
	startBucket uintptr        // 迭代器开始遍历的桶的序号
	offset      uint8          // 迭代器在桶中开始遍历的索引
	wrapped     bool           // already wrapped around from end of bucket array to beginning
	B           uint8
	i           uint8
	bucket      uintptr // 当前迭代的桶
	checkBucket uintptr
}

// bucketShift 返回 1<<b，针对代码生成进行了优化
func bucketShift(b uint8) uintptr {
	// Masking the shift amount allows overflow checks to be elided.
	return uintptr(1) << (b & (sys.PtrSize*8 - 1))
}

// bucketMask 返回 1<<b - 1, 针对代码生成进行了优化
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

// makemap_small 为 make(map[k]v)  和 make(map[k]v, hint) 实现 Go map 创建，前提是在编译时已知 hint 最多为 bucketCnt，并且需要在堆上分配映射
func makemap_small() *hmap {
	h := new(hmap)
	h.hash0 = fastrand()
	return h
}

func makemap64(t *maptype, hint int64, h *hmap) *hmap {
	if int64(int(hint)) != hint {
		hint = 0
	}
	return makemap(t, int(hint), h)
}

// makemap make(map[k]v, hint) 的实现，如果编译器已经检测到能够在栈上创建 map 或 第一个桶，则 h 或 bucket 非空
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

	// 确定 B 的值，以保证 1<<B 个桶能够装下 hint 个元素，且负载因子在约定范围内
	B := uint8(0)
	for overLoadFactor(hint, B) {
		B++ // 桶的数量翻倍
	}
	h.B = B

	// 分配哈希表，如果 B == 0，则稍后会延迟分配 buckets 字段（在 mapassign 中） 如果 hint 很大，则将此内存归零可能需要一段时间
	if h.B != 0 {
		var nextOverflow *bmap
		// 获取常规存储桶数组和下一个溢出桶，常规存储桶数组中的最后一个桶的
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil)
		if nextOverflow != nil {
			h.extra = new(mapextra)
			h.extra.nextOverflow = nextOverflow
		}
	}

	return h
}

// makeBucketArray 用于创建一个存储桶数组，并返回该数组以及下一个溢出桶的指针，1<<b 是需要分配桶的最小数量
// dirtyalloc 应该是 nil 或指向之前由 makeBucketArray 以相同 t 和 b 参数分配的存储桶数组
// 如果 dirtyalloc 是 nil 则会分配一个新的底层数组，否则 dirtyalloc 指向的数组将会被清理掉并被重新被复用为底层数组
func makeBucketArray(t *maptype, b uint8, dirtyalloc unsafe.Pointer) (buckets unsafe.Pointer, nextOverflow *bmap) {
	base := bucketShift(b)
	nbuckets := base
	// 实际分配桶的数量是 nbuckets，是不小于 base，前 base 个桶依旧为常规存储桶，base 之后的用作溢出桶
	// 这些桶在内存上是连续的，每个桶的溢出桶指针是 nil，最后一个桶的溢出桶指针记为非 nil，以标记该桶是最后一个了

	if b >= 4 { // 对于小 b，溢出桶的可能性不大，添加该条件能够避免计算的开销
		nbuckets += bucketShift(b - 4)
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
		// 复用 dirtyalloc 指向的原数组，并将该数组内存清空
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
		// 我们预先分配了一些溢出桶。为了将跟踪这些溢出桶的开销降至最低，我们使用这样的约定:
		// 如果预分配的溢出桶的溢出指针为nil，则通过碰撞指针可以获得更多可用的溢出桶。
		// 对于最后一个桶的溢出桶指针，我们需要一个安全的非空指针;，用 buckets 吧。

		// 通过调用add函数计算出下一个溢出桶的地址，并将其转换为*bmap类型的指针
		// 前 0 ~ base-1 个桶为常规桶，base ~ nbuckets-1 个桶为溢出桶，这些桶在内存上连续
		nextOverflow = (*bmap)(add(buckets, base*uintptr(t.bucketsize)))
		// 通过调用add函数计算出最后一个桶的地址，并将其转换为*bmap类型的指针
		last := (*bmap)(add(buckets, (nbuckets-1)*uintptr(t.bucketsize)))
		// 调用last.setoverflow方法，将 buckets 强制转换为*bmap类型的指针，并赋值到最后一个桶的溢出指针
		// 将第 nbuckets-1 个桶的溢出桶指针记为非 nil，以标记该桶是最后一个桶了，在 newoverflow 函数中用到
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
			// 当前旧桶没有处于搬迁过程中，则直接选择旧桶
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

// TODO mapassign 与 mapaccess 类似，但是如果 map 中没有该 key 时，会为其分配一个槽
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

	// 调用 t.hasher 后设置 hashWriting，因为 t.hasher 可能会 panic，在这种情况下，实际上还没有进行写入
	h.flags ^= hashWriting

	if h.buckets == nil {
		// 如果桶为空，则创建一个新的桶，初始状态为一个桶
		h.buckets = newobject(t.bucket) // newarray(t.bucket, 1)
	}

again:
	bucket := hash & bucketMask(h.B)
	if h.growing() {
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.bucketsize)))
	top := tophash(hash)

	var inserti *uint8
	var insertk unsafe.Pointer
	var elem unsafe.Pointer
bucketloop:
	for {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if isEmpty(b.tophash[i]) && inserti == nil {
					inserti = &b.tophash[i]
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
					elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				}
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if !t.key.equal(key, k) {
				continue
			}
			// already have a mapping for key. Update it.
			if t.needkeyupdate() {
				typedmemmove(t.key, k, key)
			}
			elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
			goto done
		}
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// Did not find mapping for key. Allocate new cell & add entry.

	// 如果命中了负载因子阈值或是有太多的溢出桶，并且又没有正在扩容，则开始扩容
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		hashGrow(t, h)
		goto again // Growing the table invalidates everything, so try again
	}

	if inserti == nil {
		// The current bucket and all the overflow buckets connected to it are full, allocate a new one.
		newb := h.newoverflow(t, b)
		inserti = &newb.tophash[0]
		insertk = add(unsafe.Pointer(newb), dataOffset)
		elem = add(insertk, bucketCnt*uintptr(t.keysize))
	}

	// store new key/elem at insert position
	if t.indirectkey() {
		kmem := newobject(t.key)
		*(*unsafe.Pointer)(insertk) = kmem
		insertk = kmem
	}
	if t.indirectelem() {
		vmem := newobject(t.elem)
		*(*unsafe.Pointer)(elem) = vmem
	}
	typedmemmove(t.key, insertk, key)
	*inserti = top
	h.count++

done:
	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting
	if t.indirectelem() {
		elem = *((*unsafe.Pointer)(elem))
	}
	return elem
}

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
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.hasher(key, 0) // see issue 23734
		}
		return
	}
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	hash := t.hasher(key, uintptr(h.hash0))

	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write (delete).
	h.flags ^= hashWriting

	bucket := hash & bucketMask(h.B)
	if h.growing() {
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.bucketsize)))
	bOrig := b
	top := tophash(hash)
search:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break search
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			k2 := k
			if t.indirectkey() {
				k2 = *((*unsafe.Pointer)(k2))
			}
			if !t.key.equal(key, k2) {
				continue
			}
			// Only clear key if there are pointers in it.
			if t.indirectkey() {
				*(*unsafe.Pointer)(k) = nil
			} else if t.key.ptrdata != 0 {
				memclrHasPointers(k, t.key.size)
			}
			e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
			if t.indirectelem() {
				*(*unsafe.Pointer)(e) = nil
			} else if t.elem.ptrdata != 0 {
				memclrHasPointers(e, t.elem.size)
			} else {
				memclrNoHeapPointers(e, t.elem.size)
			}
			b.tophash[i] = emptyOne
			// If the bucket now ends in a bunch of emptyOne states,
			// change those to emptyRest states.
			// It would be nice to make this a separate function, but
			// for loops are not currently inlineable.
			if i == bucketCnt-1 {
				if b.overflow(t) != nil && b.overflow(t).tophash[0] != emptyRest {
					goto notLast
				}
			} else {
				if b.tophash[i+1] != emptyRest {
					goto notLast
				}
			}
			for {
				b.tophash[i] = emptyRest
				if i == 0 {
					if b == bOrig {
						break // beginning of initial bucket, we're done.
					}
					// Find previous bucket, continue at its last entry.
					c := b
					for b = bOrig; b.overflow(t) != c; b = b.overflow(t) {
					}
					i = bucketCnt - 1
				} else {
					i--
				}
				if b.tophash[i] != emptyOne {
					break
				}
			}
		notLast:
			h.count--
			// Reset the hash seed to make it more difficult for attackers to
			// repeatedly trigger hash collisions. See issue 25237.
			if h.count == 0 {
				h.hash0 = fastrand()
			}
			break search
		}
	}

	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting
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

	// hiter 结构体内存对齐后应该是 12个sys.PtrSize 字节大小
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

	// 迭代器状态
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
	bucket := it.bucket
	b := it.bptr
	i := it.i
	checkBucket := it.checkBucket

next:
	if b == nil {
		if bucket == it.startBucket && it.wrapped {
			// end of iteration
			it.key = nil
			it.elem = nil
			return
		}
		if h.growing() && it.B == h.B {
			// Iterator was started in the middle of a grow, and the grow isn't done yet.
			// If the bucket we're looking at hasn't been filled in yet (i.e. the old
			// bucket hasn't been evacuated) then we need to iterate through the old
			// bucket and only return the ones that will be migrated to this bucket.
			oldbucket := bucket & it.h.oldbucketmask()
			b = (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize)))
			if !evacuated(b) {
				checkBucket = bucket
			} else {
				b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize)))
				checkBucket = noCheck
			}
		} else {
			b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize)))
			checkBucket = noCheck
		}
		bucket++
		if bucket == bucketShift(it.B) {
			bucket = 0
			it.wrapped = true
		}
		i = 0
	}
	for ; i < bucketCnt; i++ {
		offi := (i + it.offset) & (bucketCnt - 1)
		if isEmpty(b.tophash[offi]) || b.tophash[offi] == evacuatedEmpty {
			// TODO: emptyRest is hard to use here, as we start iterating
			// in the middle of a bucket. It's feasible, just tricky.
			continue
		}
		k := add(unsafe.Pointer(b), dataOffset+uintptr(offi)*uintptr(t.keysize))
		if t.indirectkey() {
			k = *((*unsafe.Pointer)(k))
		}
		e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+uintptr(offi)*uintptr(t.elemsize))
		if checkBucket != noCheck && !h.sameSizeGrow() {
			// Special case: iterator was started during a grow to a larger size
			// and the grow is not done yet. We're working on a bucket whose
			// oldbucket has not been evacuated yet. Or at least, it wasn't
			// evacuated when we started the bucket. So we're iterating
			// through the oldbucket, skipping any keys that will go
			// to the other new bucket (each oldbucket expands to two
			// buckets during a grow).
			if t.reflexivekey() || t.key.equal(k, k) {
				// If the item in the oldbucket is not destined for
				// the current new bucket in the iteration, skip it.
				hash := t.hasher(k, uintptr(h.hash0))
				if hash&bucketMask(it.B) != checkBucket {
					continue
				}
			} else {
				// Hash isn't repeatable if k != k (NaNs).  We need a
				// repeatable and randomish choice of which direction
				// to send NaNs during evacuation. We'll use the low
				// bit of tophash to decide which way NaNs go.
				// NOTE: this case is why we need two evacuate tophash
				// values, evacuatedX and evacuatedY, that differ in
				// their low bit.
				if checkBucket>>(it.B-1) != uintptr(b.tophash[offi]&1) {
					continue
				}
			}
		}
		if (b.tophash[offi] != evacuatedX && b.tophash[offi] != evacuatedY) ||
			!(t.reflexivekey() || t.key.equal(k, k)) {
			// This is the golden data, we can return it.
			// OR
			// key!=key, so the entry can't be deleted or updated, so we can just return it.
			// That's lucky for us because when key!=key we can't look it up successfully.
			it.key = k
			if t.indirectelem() {
				e = *((*unsafe.Pointer)(e))
			}
			it.elem = e
		} else {
			// The hash table has grown since the iterator was started.
			// The golden data for this key is now somewhere else.
			// Check the current hash table for the data.
			// This code handles the case where the key
			// has been deleted, updated, or deleted and reinserted.
			// NOTE: we need to regrab the key as it has potentially been
			// updated to an equal() but not identical key (e.g. +0.0 vs -0.0).
			rk, re := mapaccessK(t, h, k)
			if rk == nil {
				continue // key has been deleted
			}
			it.key = rk
			it.elem = re
		}
		it.bucket = bucket
		if it.bptr != b { // avoid unnecessary write barrier; see issue 14921
			it.bptr = b
		}
		it.i = i + 1
		it.checkBucket = checkBucket
		return
	}
	b = b.overflow(t)
	i = 0
	goto next
}

// mapclear deletes all keys from a map.
func mapclear(t *maptype, h *hmap) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapclear)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
	}

	if h == nil || h.count == 0 {
		return
	}

	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	h.flags ^= hashWriting   // 按位与
	h.flags &^= sameSizeGrow // 按位清除
	h.oldbuckets = nil
	h.nevacuate = 0
	h.noverflow = 0
	h.count = 0

	// Reset the hash seed to make it more difficult for attackers to
	// repeatedly trigger hash collisions. See issue 25237.
	h.hash0 = fastrand()

	// Keep the mapextra allocation but clear any extra information.
	if h.extra != nil {
		*h.extra = mapextra{}
	}

	// makeBucketArray clears the memory pointed to by h.buckets
	// and recovers any overflow buckets by generating them
	// as if h.buckets was newly alloced.
	_, nextOverflow := makeBucketArray(t, h.B, h.buckets)
	if nextOverflow != nil {
		// If overflow buckets are created then h.extra
		// will have been allocated during initial bucket creation.
		h.extra.nextOverflow = nextOverflow
	}

	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting
}

// 开始扩容，只创建桶数组和溢出桶数组，并分布切换旧的桶数组指针和旧的溢出桶数组指针的指向，不做实际搬迁
func hashGrow(t *maptype, h *hmap) {
	// 如果命中了负载的因子，则向更大的扩容，否则就是有太多的溢出桶，做同尺寸扩容
	bigger := uint8(1)
	if !overLoadFactor(h.count+1, h.B) {
		// 没有超过负载因子，同尺寸扩容
		bigger = 0
		h.flags |= sameSizeGrow
	}
	oldbuckets := h.buckets
	// 创建新的存储桶数组和下一个溢出桶
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

	// 判断桶的迭代器的状态
	flags := h.flags &^ (iterator | oldIterator)
	if h.flags&iterator != 0 {
		flags |= oldIterator
	}
	h.B += bigger             // 更新桶数量
	h.flags = flags           // 更新标志
	h.oldbuckets = oldbuckets // 旧桶数组指针指向的是当前存储桶数组
	h.buckets = newbuckets    // 当前存储桶数组指针指向的是新创建的存储数组
	h.nevacuate = 0           // 疏散进度数置为 0
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

// overLoadFactor 判断 count 元素放置在 1<<B 个存储桶中是否超过负载因子
func overLoadFactor(count int, B uint8) bool {
	return count > bucketCnt && uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen)
}

// tooManyOverflowBuckets 报告对于拥有 1<<B 个桶的 map 是否有太多的溢出桶，即近似大于常规桶数量
// 请注意，这些溢出存储桶中的大多数必须处于稀疏使用状态；如果使用密集，那么我们已经触发了常规的地图增长。
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
	// 如果阈值太小，我们需要做无关的工作；如果阈值太大，map 的扩容和收缩保留了大量未使用的内存
	// "too many" 意味着与常规桶近似一样多的数量，可以参考 incrnoverflow 函数
	if B > 15 {
		B = 15
	}
	// 编译器在这里没有看到 B < 16，掩码 B 以生成较短的代码
	return noverflow >= uint16(1)<<(B&15)
}

func growWork(t *maptype, h *hmap, bucket uintptr) {
	// make sure we evacuate the oldbucket corresponding to the bucket we're about to use
	// 确保我们撤离了与我们将要使用的存储桶相对应的旧存储桶
	evacuate(t, h, bucket&h.oldbucketmask())

	// evacuate one more oldbucket to make progress on growing
	if h.growing() {
		evacuate(t, h, h.nevacuate)
	}
}

// 判断第 bucket 个桶是否在搬迁中
func bucketEvacuated(t *maptype, h *hmap, bucket uintptr) bool {
	b := (*bmap)(add(h.oldbuckets, bucket*uintptr(t.bucketsize)))
	return evacuated(b)
}

// evacDst 数算目的地
type evacDst struct {
	b *bmap          // 当前疏散目的桶
	i int            // key/elem 将要迁往 b 中的索引 i 处
	k unsafe.Pointer // 存储当前 key 的地址
	e unsafe.Pointer // 存档当前 elem 的地址
}

func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize)))
	newbit := h.noldbuckets()
	if !evacuated(b) {
		// 如果没有使用旧存储桶的迭代器，请重用溢出存储桶，而不是使用新存储桶(If !oldIterator.)

		// xy 包含 the x 和 y (low 和 high) 疏散目的地
		var xy [2]evacDst
		x := &xy[0]
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.bucketsize)))
		x.k = add(unsafe.Pointer(x.b), dataOffset)
		x.e = add(x.k, bucketCnt*uintptr(t.keysize))

		if !h.sameSizeGrow() {
			// 只有增量扩容时才计算 y 指针，否则 GC 可能会看到错误的指针
			y := &xy[1]
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.bucketsize)))
			y.k = add(unsafe.Pointer(y.b), dataOffset)
			y.e = add(y.k, bucketCnt*uintptr(t.keysize))
		}

		// 依次遍历当前该桶及其溢出桶
		for ; b != nil; b = b.overflow(t) {
			k := add(unsafe.Pointer(b), dataOffset)
			e := add(k, bucketCnt*uintptr(t.keysize))
			// 依次遍历当前桶的 key、elem
			for i := 0; i < bucketCnt; i, k, e = i+1, add(k, uintptr(t.keysize)), add(e, uintptr(t.elemsize)) {
				top := b.tophash[i] // 获取当前 key 的顶部哈希值
				if isEmpty(top) {
					// 当前键值对已经搬走了，更新顶部哈希值
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
				var useY uint8
				if !h.sameSizeGrow() {
					// 增量扩容
					// 计算哈希值以做出撤离决策(我们是否需要将 key/elem 发送到 桶 x 或 桶 y)
					hash := t.hasher(k2, uintptr(h.hash0))
					if h.flags&iterator != 0 && !t.reflexivekey() && !t.key.equal(k2, k2) {
						// If key != key (NaNs), then the hash could be (and probably
						// will be) entirely different from the old hash. Moreover,
						// it isn't reproducible. Reproducibility is required in the
						// presence of iterators, as our evacuation decision must
						// match whatever decision the iterator made.
						// Fortunately, we have the freedom to send these keys either
						// way. Also, tophash is meaningless for these kinds of keys.
						// We let the low bit of tophash drive the evacuation decision.
						// We recompute a new random tophash for the next level so
						// these keys will get evenly distributed across all buckets
						// after multiple grows.
						useY = top & 1
						top = tophash(hash)
					} else {
						if hash&newbit != 0 {
							useY = 1
						}
					}
				}

				if evacuatedX+1 != evacuatedY || evacuatedX^1 != evacuatedY {
					throw("bad evacuatedN")
				}

				b.tophash[i] = evacuatedX + useY // evacuatedX + 1 == evacuatedY
				dst := &xy[useY]                 // evacuation destination

				if dst.i == bucketCnt {
					// 当前桶已经放满了，创建溢出桶，并放入其中
					dst.b = h.newoverflow(t, dst.b)
					dst.i = 0
					dst.k = add(unsafe.Pointer(dst.b), dataOffset)
					dst.e = add(dst.k, bucketCnt*uintptr(t.keysize))
				}
				dst.b.tophash[dst.i&(bucketCnt-1)] = top // 屏蔽 dst.i 作为一种优化，以避免边界检查
				// 搬移 key
				if t.indirectkey() {
					*(*unsafe.Pointer)(dst.k) = k2 // copy pointer
				} else {
					typedmemmove(t.key, dst.k, k) // copy elem
				}
				// 搬移 elem
				if t.indirectelem() {
					*(*unsafe.Pointer)(dst.e) = *(*unsafe.Pointer)(e)
				} else {
					typedmemmove(t.elem, dst.e, e)
				}
				dst.i++
				// These updates might push these pointers past the end of the
				// key or elem arrays.  That's ok, as we have the overflow pointer
				// at the end of the bucket to protect against pointing past the
				// end of the bucket.
				dst.k = add(dst.k, uintptr(t.keysize))
				dst.e = add(dst.e, uintptr(t.elemsize))
			}
		}
		// 解除溢出桶的链接，并且帮助 GC清理 key/elem
		if h.flags&oldIterator == 0 && t.bucket.ptrdata != 0 {
			b := add(h.oldbuckets, oldbucket*uintptr(t.bucketsize))
			// 保留 b.tophash，因为那里保持了疏散状态
			ptr := add(b, dataOffset)
			n := uintptr(t.bucketsize) - dataOffset
			memclrHasPointers(ptr, n)
		}
	}

	if oldbucket == h.nevacuate {
		advanceEvacuationMark(h, t, newbit)
	}
}

func advanceEvacuationMark(h *hmap, t *maptype, newbit uintptr) {
	h.nevacuate++
	// Experiments suggest that 1024 is overkill by at least an order of magnitude.
	// Put it in there as a safeguard anyway, to ensure O(1) behavior.
	stop := h.nevacuate + 1024
	if stop > newbit {
		stop = newbit
	}
	for h.nevacuate != stop && bucketEvacuated(t, h, h.nevacuate) {
		h.nevacuate++
	}
	if h.nevacuate == newbit { // newbit == # of oldbuckets
		// Growing is all done. Free old main bucket array.
		h.oldbuckets = nil
		// Can discard old overflow buckets as well.
		// If they are still referenced by an iterator,
		// then the iterator holds a pointers to the slice.
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
