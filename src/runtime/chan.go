// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	// 用来设置内存最大对齐值，对应就是64位系统下cache line的大小
	maxAlign = 8
	//  向上进行 8 字节内存对齐，假设hchan{}大小是14，hchanSize是16。
	hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1))
	debugChan = false
)

type hchan struct {
	qcount   uint           // chan 中的数据量
	dataqsiz uint           // chan 循环队列的长度，底层是数组
	buf      unsafe.Pointer // chan 指向底层缓冲数组的指针
	elemsize uint16         // chan 中元素大小
	closed   uint32         // chan 是否被关闭，非0表示关闭
	elemtype *_type         // chan 中元素类型
	sendx    uint           // 生产队列可发送的元素在数组中的索引，即从此处开始写入
	recvx    uint           // 消费队列可接收的元素在数组中的索引，即从此处开始消费
	recvq    waitq          // 等待接收数据的goroutine队列，消费队列
	sendq    waitq          // 等待发送数据的goroutine队列，生产队列

	// 锁定保护 hchan 中的所有字段，以及阻塞在此通道上的 sudogs 中的多个字段。
	// 在保持此锁时不要更改另一个 G 的状态（特别是不要把 G 转为准备状态），因为这可能会导致堆栈收缩而死锁。
	lock mutex
}

// goroutine 的生产队列或消费队列
type waitq struct {
	first *sudog // 指向goroutine队列的第一个
	last  *sudog // 指向goroutine队列的最后一个
}

// 将 sudog 添加到协程的等待队列的尾部
func (q *waitq) enqueue(sgp *sudog) {
	sgp.next = nil
	x := q.last
	if x == nil {
		sgp.prev = nil
		q.first = sgp
		q.last = sgp
		return
	}
	sgp.prev = x
	x.next = sgp
	q.last = sgp
}

// 从协程的等待队列首部取出 sudog
func (q *waitq) dequeue() *sudog {
	for {
		// 获取队列中的首个协程
		sgp := q.first
		if sgp == nil {
			// 为空则直接返回
			return nil
		}
		y := sgp.next
		if y == nil {
			// 如果该协程下个协程为空，则整个队列都为空
			q.first = nil
			q.last = nil
		} else {
			// 否则将下个协程的前置指针置空
			y.prev = nil
			// 将下个赋给首位
			q.first = y
			// 将要出队的协程的后置指针置空，切断与其他协程的联系
			sgp.next = nil
		}

		// 如果一个 goroutine 因为 select 而被放到这个队列上，那么在被不同情况唤醒的 goroutine 和它抓取通道锁之间有一个小窗口。
		// 一旦它有了锁，它就会将自己从队列中删除，所以之后我们不会看到它。
		// 我们在 G 结构中使用一个标志来发出这个 goroutine 的信号，其他人何时赢得了竞争但 goroutine 还没有将自己从队列中删除
		if sgp.isSelect && !atomic.Cas(&sgp.g.selectDone, 0, 1) {
			continue
		}

		return sgp
	}
}

//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
	return makechan(t, size)
}

func makechan64(t *chantype, size int64) *hchan {
	if int64(int(size)) != size {
		panic(plainError("makechan: size out of range"))
	}

	return makechan(t, int(size))
}

func makechan(t *chantype, size int) *hchan {
	elem := t.elem

	// 编译器校验类型，但是是非安全的
	if elem.size >= 1<<16 { // 元素类型判断
		throw("makechan: invalid channel element type")
	}
	// 元素对齐方式校验
	if hchanSize%maxAlign != 0 || elem.align > maxAlign {
		throw("makechan: bad alignment")
	}

	// chan大小乘以元素大小，得到要分配的内存大小，需要判断乘积是溢出，以及是否超过了最大内存分配等
	mem, overflow := math.MulUintptr(elem.size, uintptr(size))
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	// 当存储在 buf 中的元素不包含指针时，Hchan 不包含对 GC 感兴趣的指针。指向相同的分配的buf中elemtype是持久的。
	// SudoG's 是从其所属线程引用的，因此无法收集它们。
	// 如果 hchan 结构体中不含指针，GC 就不会扫描 chan 中的元素
	var c *hchan
	switch {
	case mem == 0:
		// 当chan为无缓冲或元素为空结构体时，需要分配的内存为0，仅分配需要存储chan的内存
		c = (*hchan)(mallocgc(hchanSize, nil, true))
		// Race detector uses this location for synchronization.
		c.buf = c.raceaddr()
	case elem.ptrdata == 0:
		// 元素不包含指针。在一次调用中分配 hchan 和 buf。
		// 当通道数据元素不含指针，hchan和buf内存空间调用mallocgc一次性分配完成，hchanSize用来分配通道的内存，mem用来分配buf的内存
		c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
		// c 为指向 hchan 的指针，再加上该结构的大小 hchanSize，即可得到指向 buf的指针，它们在内存上是连续的
		c.buf = add(unsafe.Pointer(c), hchanSize)
	default:
		// 通道中的元素包含指针，分别创建 chan 和 buf 的内存空间，方便 gc 进行内存回收
		c = new(hchan)
		c.buf = mallocgc(mem, elem, true)
	}

	c.elemsize = uint16(elem.size)   // 元素大小
	c.elemtype = elem                // 元素类型
	c.dataqsiz = uint(size)          // chan 的容量
	lockInit(&c.lock, lockRankHchan) // todo ？

	if debugChan {
		print("makechan: chan=", c, "; elemsize=", elem.size, "; dataqsiz=", size, "\n")
	}
	return c
}

//go:nosplit 编译代码中 c <- X 的入口点
func chansend1(c *hchan, elem unsafe.Pointer) {
	chansend(c, elem, true, getcallerpc())
}

// 返回 false 表示写入失败
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
	// 如果 block 为 false，goroutine 将不允许被阻塞，不等于非缓冲
	if c == nil {
		// 非阻塞模式下直接返回
		if !block {
			return false
		}
		// 阻塞模式下，直接阻塞当前写入协程
		// 调用 gopark 将当前 goroutine 休眠，关闭 nil 的 chan 会 panic，故当前 goroutine 会一直休眠，陷入死锁
		gopark(nil, nil, waitReasonChanSendNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	if debugChan {
		print("chansend: chan=", c, "\n")
	}

	if raceenabled {
		racereadpc(c.raceaddr(), callerpc, funcPC(chansend))
	}

	// 快速路径：在不获取锁的情况下检查失败的非阻塞操作。
	// 在观察到通道未关闭后，我们观察到通道尚未准备好发送。这些观察中的每一个都是单个字大小的读取（第一个c.closed和第二个full（））。
	// 由于闭合通道无法从“准备发送”过渡到“未准备好发送”，因此即使通道在两个观测值之间关闭，它们也意味着当通道尚未关闭且尚未准备好发送时，两者之间存在一个时刻。
	// 我们的行为就好像我们当时观察了通道，并报告发送无法继续。如果读取在这里重新排序是可以的：如果我们观察到通道尚未准备好发送，然后观察到它没有关闭，这意味着在第一次观察期间通道没有关闭。
	// 然而，这里没有任何东西能保证向前推进。我们依靠 chanrecv（） 和 closechan（） 中锁释放的副作用来更新这个线程对 c.closed 和 full（） 的看法。
	// c 不为 nil，非阻塞模式，且chan没有关闭，但已经满了
	if !block && c.closed == 0 && full(c) {
		return false
	}

	// 执行到此处说明是以下3种情况中的某一种或两种
	// 1，阻塞模式，block==true；
	// 2，chan 已经关闭；
	// 3，管道非满的情况
	// 	3.1 无缓冲管道，且接收队列不为空；
	// 	3.2 缓冲管道，但缓冲管道未满
	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)
	// 2，chan 已经关闭；
	if c.closed != 0 { // todo 向一个关闭的通道写入数据会panic
		unlock(&c.lock)
		panic(plainError("send on closed channel"))
	}

	// 执行到此处，说明管道是未关闭的，阻塞模式或管道非满
	// 从接收者队列 recvq 中取出一个接收者，接收者不为空情况下，直接将数据传递给该接收者
	// 3.1 无缓冲管道，且接收队列不为空；即使非阻塞能写则写
	if sg := c.recvq.dequeue(); sg != nil {
		// todo 非常细节，找到一个等待的接收器，将要发送的值直接传递给接收器，绕过通道缓冲区（如果有的话）。
		send(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true
	}

	// 3.2 缓冲管道，但没有接收者，且缓冲管道未满；即使非阻塞能写则写
	if c.qcount < c.dataqsiz {
		// 找到最新能够写入元素的位置
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}
		// 将要写入的元素的值拷贝到该处
		typedmemmove(c.elemtype, qp, ep)
		c.sendx++                  // 写入的位置往后移
		if c.sendx == c.dataqsiz { // 如果等于数组长度，则跳转到首位（循环队列）
			c.sendx = 0
		}
		c.qcount++ // chan 中的元素个数加一
		unlock(&c.lock)
		return true
	}

	// 执行到此处，说明如果是无缓冲管道则没有接收者，是缓冲管道则已经满了
	if !block {
		// 不允许阻塞，直接返回 false
		unlock(&c.lock)
		return false
	}
	// channel 未关闭，且已满，没有接收者，goroutine允许被阻塞，接下来让该 goroutine 阻塞等待

	// 获取当前发送数据的 goroutine，然后绑定到一个 sudog 结构体 (包装为运行时表示)
	gp := getg()
	mysg := acquireSudog()
	// 获取 sudog 结构体，并且设置相关字段 (包括当前的 channel，是否是 select 等)
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// 在 gp.waiting 上分配 elem 和将 mysg 排队之间没有堆栈拆分，copystack 可以找到它。
	mysg.elem = ep
	mysg.waitlink = nil
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.waiting = mysg
	gp.param = nil
	// 当前 goroutine 进入发送等待队列
	c.sendq.enqueue(mysg)
	// 向任何试图缩小我们的堆栈的人发出信号，表明我们将停在通道上。
	// 从这个 G 的状态更改到我们设置 gp.activeStackChans 之间的窗口对于堆栈收缩是不安全的。
	atomic.Store8(&gp.parkingOnChan, 1)
	// 挂起当前 goroutine, 进入休眠 (等待接收)
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanSend, traceEvGoBlockSend, 2)
	// 确保发送的值保持活动状态，直到接收方将其复制出来。sudog 具有指向堆栈对象的指针，但 sudog 不被视为堆栈跟踪器的根。
	KeepAlive(ep)

	// 从这里开始被唤醒了（channel 有机会可以发送了）
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	closed := !mysg.success // 如果 goroutine 因为通过通道 c 传递了值而被唤醒，则为 true，如果因为 c 被关闭而唤醒，则为 false。
	gp.param = nil
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	// 取消 sudog 和 channel 绑定关系
	mysg.c = nil
	releaseSudog(mysg) // 去掉 mysg 上绑定的 channel
	if closed {
		if c.closed == 0 {
			throw("chansend: spurious wakeup")
		}
		// 被唤醒后，管道关闭了，todo 向一个关闭的管道发送数据会panic
		panic(plainError("send on closed channel"))
	}
	return true
}

// send 处理空通道 c 上的写入操作，生产者 goroutine 上待写入的值 ep 被复制到消费者 sg，然后该消费者被唤醒
// 通道 c 必须为空并锁定（此时才能直接在生产者和消费者两个 goroutine 之间拷贝数据）
// send 通过 unlockf 解锁 c，sg 必须已从 c 的消费者队列中出队，ep 必须为非 nil 并指向堆或调用方的堆栈
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if raceenabled {
		if c.dataqsiz == 0 {
			racesync(c, sg)
		} else {
			// Pretend we go through the buffer, even though
			// we copy directly. Note that we need to increment
			// the head/tail locations only when raceenabled.
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
			c.recvx++
			if c.recvx == c.dataqsiz {
				c.recvx = 0
			}
			c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
		}
	}
	// sg.elem 指向接收到的值存放的位置，如 val <- ch，指的就是 &val
	if sg.elem != nil {
		// 直接拷贝内存（从发送者到接收者）
		sendDirect(c.elemtype, sg, ep)
		sg.elem = nil
	}
	gp := sg.g
	unlockf()
	gp.param = unsafe.Pointer(sg)
	sg.success = true // 因为用数据写入而唤醒 goroutine
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	// 唤醒接收的 goroutine. skip 和打印栈相关
	// 调用 goready 函数将接收方 goroutine 唤醒并标记为可运行状态
	// 并把其放入发送方所在处理器 P 的 runnext 字段等待执行，runnext 字段表示最高优先级的 goroutine
	goready(gp, skip+1)
}

// src 在当前 goroutine 的栈上，dst 是另一个 goroutine 的栈
func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
	// 一旦我们从 sg 中读取 sg.elem，如果 dst 的堆栈被复制（缩小），它将不再更新。
	// 因此，请确保在读取和使用之间不会发生抢占点，需要在读和写之前加上一个屏障

	// 向一个非缓冲型的 channel 发送数据、从一个无元素的（非缓冲型或缓冲型但空）的 channel
	// 接收数据，都会导致一个 goroutine 直接操作另一个 goroutine 的栈
	// 由于 GC 假设对栈的写操作只能发生在 goroutine 正在运行中并且由当前 goroutine 来写
	// 所以这里实际上违反了这个假设。可能会造成一些问题，所以需要用到写屏障来规避
	dst := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	// 不需要 cgo 写入屏障检查，因为 dst 始终是 Go 内存。
	memmove(dst, src, t.size)
}

// chanbuf 获取 buf 中第 i 个位置的元素
func chanbuf(c *hchan, i uint) unsafe.Pointer {
	return add(c.buf, uintptr(i)*uintptr(c.elemsize))
}

// full 函数检测 channel 缓冲区是否已满，主要分为两种情况:
// 如果 channel 没有缓冲区，查看是否存在接收者
// 如果 channel 有缓冲区, 比较元素数量和缓冲区长度是否一致
func full(c *hchan) bool {
	// c.dataqsiz 是不可变的（在创建通道后永远不会写入），因此在通道操作期间随时读取是安全的。
	if c.dataqsiz == 0 {
		// 无缓冲，且没有消费队列
		return c.recvq.first == nil
	}
	// 有缓冲，且缓冲区大小和chan中实际元素个数相等，即满了
	return c.qcount == c.dataqsiz
}

// close 函数用于关闭管道 c，如果 c 是 nil 管道，直接 panic，关闭一个 nil 的 chan 会 panic；
// close 函数先上一把大锁，将 c 设置为关闭状态，将所有阻塞等待的消费者 goroutine 出队，赋给其一个相应类型的零值，并设置 sg.success 为 false，表示是因为管道关闭而被唤醒的，然后将该 goroutine加入到一个链表中；
// 将所有阻塞等待的生产者 goroutine 出队，同样设置 sg.success 为 false，表示是因为管道关闭而被唤醒的，然后将该 goroutine加入到一个链表中；解锁，然后将该链表中的所有 goroutine 唤醒；
// 被唤醒后，生产者 goroutine 会继续执行 chansend 函数里 goparkunlock 函数之后的代码，检测到自己是因为 chan 关闭而被唤醒的，直接 panic；
// 消费者 goroutine 同样会继续执行 chanrecv 函数里 goparkunlock 函数之后的代码，检测到自己是因为 chan 关闭而被唤醒的，返回 received 为 false，表示读取的是零值；
// 关闭 chan 后，对于阻塞等待的消费者而言，会收到一个相应类型的零值，对于阻塞等待的生产者而言，会直接 panic，所以，在不了解 chan 还有没有接收者的情况下，不能贸然关闭 chan；
func closechan(c *hchan) {
	if c == nil { // todo 关闭一个 nil 的 chan 会 panic
		panic(plainError("close of nil channel"))
	}
	// 加锁，这个锁的粒度比较大，会持续到释放完所有的 sudog 才解锁
	lock(&c.lock)
	if c.closed != 0 { // todo 关闭一个已经关闭的 chan 会 panic
		unlock(&c.lock)
		panic(plainError("close of closed channel"))
	}

	if raceenabled {
		callerpc := getcallerpc()
		racewritepc(c.raceaddr(), callerpc, funcPC(closechan))
		racerelease(c.raceaddr())
	}

	c.closed = 1    // 设置 channel 状态为已关闭
	var glist gList // 用于存放发送+接收队列中的所有 goroutine

	// 将接收队列中所有 goroutine 加入 gList 列表
	for {
		// 如果此通道的接收数据协程队列不为空（这种情况下，缓冲队列必为空），
		// 此队列中的所有协程将被依个弹出，并且每个协程将接收到此通道的元素类型的一个零值，然后恢复至运行状态
		sg := c.recvq.dequeue()
		if sg == nil {
			// 出队的 sudog 为 nil，说明发送队列为空，直接跳出循环
			break
		}
		// 如果 elem 不为空，说明此 receiver 未忽略接收数据，给它赋一个相应类型的零值
		if sg.elem != nil {
			typedmemclr(c.elemtype, sg.elem)
			sg.elem = nil
		}
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		// 取出 goroutine
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false // todo import 因为关闭而唤醒，设为false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp) // 将 sg 对应的 goroutine 添加到 glist 列表

	}

	// 将发送队列中所有 goroutine 加入 gList 列表
	// todo 如果存在，这些 goroutine 将会 panic，在写入处引发panic
	for {
		// 如果此通道的发送数据协程队列不为空，此队列中的所有协程将被依个弹出，并且每个协程中都将产生一个 panic（因为向已关闭的通道发送数据）。
		sg := c.sendq.dequeue()
		if sg == nil {
			break // 出队的 sudog 为 nil，说明发送队列为空，直接跳出循环
		}

		sg.elem = nil // 忽略发送协程的值
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false // todo import 因为关闭而唤醒，设为false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}

		glist.push(gp) // 将 sg 对应的 goroutine 添加到 glist 列表
	}

	unlock(&c.lock) // 解锁

	// 准备好所有 G，现在我们已经删除了通道锁。
	for !glist.empty() {
		gp := glist.pop()
		gp.schedlink = 0
		// 唤醒所有线程，接收队列里的协程获取零值，继续后续执行，发送队列里的协程，触发panic
		goready(gp, 3)
		// 	唤醒发送和接收协程，发送协程从 chansend 中的 gopark 后开始执行；接收协程从 chanrecv 中的 gopark 后开始执行
	}
}

// 判断 c 是否为空
func empty(c *hchan) bool {
	if c.dataqsiz == 0 {
		// 无缓冲 channel 并且没有发送方正在阻塞
		return atomic.Loadp(unsafe.Pointer(&c.sendq.first)) == nil
	}
	// 有缓冲 channel 并且缓冲区没有数据
	return atomic.Loaduint(&c.qcount) == 0
}

// entry points for <- c from compiled code
//go:nosplit
// <- c 代码的编译入口
func chanrecv1(c *hchan, elem unsafe.Pointer) {
	chanrecv(c, elem, true)
}

//go:nosplit
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
	_, received = chanrecv(c, elem, true)
	return
}

// chanrecv 从管道 c 上接收数据并写到 ep 上，ep 可能为 nil，这种情况下接收到的数据会被忽略
// 如果 block 为 false 并且没有可用的元素，返回 false，false
// 否则，如果 c 是关闭的，则将 ep 置零，返回 true，false
// 否则，用一个元素填充 ep 并返回 true，true，一个非空的 ep 必须指向堆或调用者的栈
// 如果 channel == nil, 非阻塞模式直接返回，阻塞模式，休眠当前 goroutine，并报错
// 如果 channel 已经关闭或者缓冲区没有等待接收的数据，直接返回
// 如果 channel 发送队列不为空, 说明没有缓冲区或缓冲区已满
// 		从发送队列获取第一个发送者协程
// 		如果是无缓冲区，直接从发送 goroutine 拷贝数据到接收数据的地址
// 否则，缓冲区已满，从接收队列头部的 goroutine 开始接收数据，并将数据添加到发送队列尾部的 goroutine
// 如果 channel 缓冲区有数据，直接从缓冲区读取数据
// 如果以上条件都不满足，就获取一个新的 sudog 结构体并放入 channel 的接收队列，同时挂起当前发送数据的 goroutine, 进入休眠 (等待发送方发送数据)
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
	if debugChan {
		print("chanrecv: chan=", c, "\n")
	}

	if c == nil {
		if !block {
			return // channel 为 nil，非阻塞模式下直接返回
		}
		// 阻塞模式下，调用 gopark 将当前 goroutine休眠，调用 gopark 时候，将传入 unlockf 设置为 nil，当前 goroutine 会一直休眠
		gopark(nil, nil, waitReasonChanReceiveNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	// empty 函数返回 true 的情况:
	//    1. 无缓冲 channel 并且没有发送方正在阻塞
	//    2. 有缓冲 channel 并且缓冲区没有数据
	if !block && empty(c) {
		// 非阻塞模式，且 c 为空
		if atomic.Load(&c.closed) == 0 {
			// 非阻塞、无数据、且未关闭，直接返回
			// 因为 channel 关闭后就无法再打开，所以只要 channel 未关闭，上述方法都是原子操作 (看到的结果都是一样的)
			return
		}

		// channel 已经关闭，重新检查 channel 是否存在等待接收的数据
		if empty(c) {
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			if ep != nil {
				typedmemclr(c.elemtype, ep) // 没有任何等待接收的数据，清理 ep 指针中的数据
			}
			return true, false
		}
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	// channel 已经关闭，且没有数据
	if c.closed != 0 && c.qcount == 0 {
		if raceenabled {
			raceacquire(c.raceaddr())
		}
		// 解锁
		unlock(&c.lock)
		if ep != nil {
			// 清理 ep 指针中的数据
			typedmemclr(c.elemtype, ep)
		}
		return true, false
	}

	// channel 未关闭，或关闭了但是还有数据

	// 还有阻塞的发送者协程，说明没有缓冲区或是缓冲区已满
	if sg := c.sendq.dequeue(); sg != nil {
		// 从发送队列获取第一个发送者协程
		// 如果是无缓冲区，直接从发送 goroutine 拷贝数据到接收数据的地址
		// 否则，缓冲区已满，从接收队列头部的 goroutine 开始接收数据，并将数据添加到发送队列尾部的 goroutine
		recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true, true
	}

	// 没有阻塞的发送者协程，但是channel里面还有数据
	if c.qcount > 0 {
		// 根据可消费数据的索引直接获取到要消费的数据的地址
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
		}
		if ep != nil {
			// 直接从缓冲区的地址上拷贝数据到接收数据的地址
			typedmemmove(c.elemtype, ep, qp)
		}
		// 清除已经消费的数据
		typedmemclr(c.elemtype, qp)
		// 消费索引往后移
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		// 元素数量减一
		c.qcount--
		unlock(&c.lock)
		return true, true
	}

	// 没有等待的发送者协程，缓冲区没有数据，且非阻塞的，直接返回
	if !block {
		unlock(&c.lock)
		return false, false
	}

	// 没有等待的发送者协程，缓冲区没有数据，且阻塞的
	// 获取当前接收的协程 goroutine，然后绑定到一个 sudog 结构体 (包装为运行时表示)
	gp := getg()
	mysg := acquireSudog() // 获取 sudog 结构体，并设置相关参数
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	mysg.elem = ep // 设置接收数据的地址
	mysg.waitlink = nil
	gp.waiting = mysg
	mysg.g = gp           // 设置 goroutine
	mysg.isSelect = false // 设置是否 select
	mysg.c = c            // 设置当前的 channel
	gp.param = nil
	c.recvq.enqueue(mysg) // 进入接收队列等待

	// 给任何试图缩小我们堆栈的人发信号告诉他们我们要停在一个 chan 上。当这个G的状态改变时和我们设置 gp.activeStackChans 之间的窗口对于堆栈收缩是不安全的。
	atomic.Store8(&gp.parkingOnChan, 1)
	// 挂起当前 goroutine, 进入休眠 (等待发送方发送数据)，阻塞中
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanReceive, traceEvGoBlockRecv, 2)

	// 因为某种原因而被唤醒，重新获取gp
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	// todo 被唤醒的原因，true，因为写入了数据，false，因为关闭了管道
	success := mysg.success
	gp.param = nil
	mysg.c = nil       // 取消 sudog 和 channel 绑定关系
	releaseSudog(mysg) // 释放 sudog
	return true, success
}

// recv 在一个满的 c 上处理一个接收操作。
// 有两个部分:
// 	1) 发送方 sg 发送的值被放入 c，然后唤醒发送方，继续它的正常工作。
// 	2) 接收方接收到的值(当前G)写入 ep。
// 对于同步通道，两个值是相同的。对于异步信道，接收方从 c 缓冲区获取数据，发送方的数据放入 c 缓冲区。
// 通道 c 必须是满的和锁定的。recv 用 unlockf 解锁 c。
// sg 必须已经从 c 出队。非 nil 的 ep 必须指向堆或调用者的堆栈。
// * recv 处理无缓冲区或是满管道 c 上的读出操作，对于无缓冲管道，直接从生产者 goroutine 拷贝数据到消费者 goroutine
// * 对于缓冲性满管道，写入和读出索引是相同的，直接从管道的读出索引处拷贝数据到消费者者 goroutine，并从生产者 goroutine 拷贝数据到管道的该处，同时更新生产和写入索引；
// * 将发送者协程的数据指针置空，并将 sg.success 置为 true，表示该生产者 goroutine 是因为写入值成功而被唤醒，然后唤醒该 goroutine；
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	// 还有阻塞的发送者协程，说明没有缓冲区或是缓冲区已满
	if c.dataqsiz == 0 {
		// 无缓冲区
		if raceenabled {
			racesync(c, sg)
		}
		if ep != nil {
			recvDirect(c.elemtype, sg, ep) // 直接从发送者 goroutine 将数据拷贝到接收者 goroutine 的 ep 上
		}
	} else {
		// 缓冲区已满，从消费索引处获取数据的指针
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
		}
		if ep != nil {
			// 将消费索引处的数据拷贝到接收数据的指针
			typedmemmove(c.elemtype, ep, qp)
		}

		// 因为缓冲区已经满了，所以生产索引和消费索引是同一个位置，直接将发送者协程的数据拷贝到消费索引处
		typedmemmove(c.elemtype, qp, sg.elem)

		c.recvx++ // 消费索引加一
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		// 缓冲区是满的，两者相等，元素数量不变
		c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
	}
	sg.elem = nil // 发送者协程的数据指针置空
	gp := sg.g
	unlockf() // 解锁
	gp.param = unsafe.Pointer(sg)
	sg.success = true // 因为写入值成功而被唤醒
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	// 调用 goready 函数将接收方 goroutine 唤醒并标记为可运行状态，并把其放入发送方所在处理器 P 的 runnext 字段等待执行
	goready(gp, skip+1)
}

// dst 在当前 goroutine 的栈上，src 是另一个 goroutine 的栈
func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
	// channel 被锁定，因此 src 在此操作期间不会移动
	src := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	memmove(dst, src, t.size)
}

func chanparkcommit(gp *g, chanLock unsafe.Pointer) bool {
	// 有未解锁的 sudogs 指向 gp 的堆栈。堆栈复制必须锁定那些 sudogs 的通道。
	// 在这里设置 activeStackChans，而不是在我们尝试 park 之前，因为我们可能在通道锁上的堆栈增长中陷入死锁。
	gp.activeStackChans = true
	// 标记栈收缩发生在现在是安全的，因为任何获取这个G的栈进行收缩的线程都保证会在这个存储之后观察到activeStackChans。
	atomic.Store8(&gp.parkingOnChan, 0)
	// 确保我们在设置activeStackChans和取消设置parkingOnChan后解锁。在我们解锁chanLock的那一刻，我们就冒着gp准备好通道操作的风险，所以gp可以在解锁可见之前(甚至对gp本身)继续运行。
	unlock((*mutex)(chanLock))
	return true
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
//
// select case 编译时，发送数据为非阻塞，即非阻塞型
// todo must important, 没有default时，select case 编译成 chansend1(c *hchan, elem unsafe.Pointer)，即阻塞型
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
	return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selected, ok = selectnbrecv(&v, c); selected {
//		... foo
//	} else {
//		... bar
//	}
// select case 编译时，接收数据为非阻塞，即非阻塞型
// todo must important, 没有default时，select case 编译成 chanrecv1(c *hchan, elem unsafe.Pointer)，即阻塞型
// 3. 所有case都未ready，且没有default语句
//   3.1 将当前协程加入到所有channel的等待队列
//   3.2 当将协程转入阻塞，等待被唤醒
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected, received bool) {
	return chanrecv(c, elem, false)
}

//go:linkname reflect_chansend reflect.chansend
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
	return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
	return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflectlite_chanlen internal/reflectlite.chanlen
func reflectlite_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
	closechan(c)
}

func (c *hchan) raceaddr() unsafe.Pointer {
	// Treat read-like and write-like operations on the channel to
	// happen at this address. Avoid using the address of qcount
	// or dataqsiz, because the len() and cap() builtins read
	// those addresses, and we don't want them racing with
	// operations like close().
	// 将通道上的类似读和类似写操作视为在此地址发生。
	// 避免使用 qcount 或 dataqsiz 的地址，因为 len（） 和 cap（） 内置会读取这些地址，我们不希望它们与 close（） 等操作竞争。
	return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
	racerelease(chanbuf(c, 0))
	raceacquireg(sg.g, chanbuf(c, 0))
	racereleaseg(sg.g, chanbuf(c, 0))
	raceacquire(chanbuf(c, 0))
}

// Notify the race detector of a send or receive involving buffer entry idx
// and a channel c or its communicating partner sg.
// This function handles the special case of c.elemsize==0.
func racenotify(c *hchan, idx uint, sg *sudog) {
	// We could have passed the unsafe.Pointer corresponding to entry idx
	// instead of idx itself.  However, in a future version of this function,
	// we can use idx to better handle the case of elemsize==0.
	// A future improvement to the detector is to call TSan with c and idx:
	// this way, Go will continue to not allocating buffer entries for channels
	// of elemsize==0, yet the race detector can be made to handle multiple
	// sync objects underneath the hood (one sync object per idx)
	qp := chanbuf(c, idx)
	// When elemsize==0, we don't allocate a full buffer for the channel.
	// Instead of individual buffer entries, the race detector uses the
	// c.buf as the only buffer entry.  This simplification prevents us from
	// following the memory model's happens-before rules (rules that are
	// implemented in racereleaseacquire).  Instead, we accumulate happens-before
	// information in the synchronization object associated with c.buf.
	if c.elemsize == 0 {
		if sg == nil {
			raceacquire(qp)
			racerelease(qp)
		} else {
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
		}
	} else {
		if sg == nil {
			racereleaseacquire(qp)
		} else {
			racereleaseacquireg(sg.g, qp)
		}
	}
}
