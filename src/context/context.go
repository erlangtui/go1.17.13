// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package context 包定义了context类型，其中包含截止日期、取消信号以及其他跨API边界和进程之间的请求作用域值。
// 对服务器的传入请求应该创建上下文，对服务器的传出调用应该接受上下文。
// 它们之间的函数调用链必须传播上下文，可以选择将其替换为使用 WithCancel、WithDeadline、WithTimeout 或 WithValue 创建的派生上下文。
// 当 Context 被取消时，所有从它派生的上下文也被取消。
// WithCancel、WithDeadline 和 WithTimeout 函数接受一个 Context(父类) 并返回一个派生 Context(子类) 和一个 CancelFunc。
// 调用 CancelFunc 会取消子进程及其子进程，移除父进程对子进程的引用，并停止所有相关的计时器。
// 未能调用 CancelFunc 会泄漏子进程及其子进程，直到父进程被取消或计时器触发。
// go - vet 工具检查在所有控制流路径上是否使用了 CancelFuncs。
// 使用上下文的程序应该遵循这些规则，以保持包间接口的一致性，并使静态分析工具能够检查上下文传播:不要在结构类型中存储上下文;
// 相反，将上下文显式地传递给需要它的每个函数。Context应该是第一个参数，通常命名为ctx:
// 	func DoSomething(ctx context.Context, arg Arg) error {
// 		// ... use ctx ...
// 	}
// 即使函数允许，也不要传递nil上下文。通过上下文。如果您不确定要使用哪个上下文，则 TODO。
// 仅将上下文值用于传输进程和 api 的请求作用域数据，不能用于向函数传递可选参数。
// 同一个 Context 可以被传递给运行在不同协程中的函数；上下文对于多个协程同时使用是安全的；
// 请参阅https://blog.golang.org/context获取使用上下文的服务器的示例代码。
// 在 Go 语言中，context 包提供了一种在并发操作中传递请求作用域的方法。Context 可以用于控制并发操作、取消操作、超时处理等，并且是在 Go 语言中处理并发时非常重要和常用的工具。
// Context 的基本原理可以概括如下：
// Context 是一个接口类型，它定义了跟踪请求的截止日期、取消信号和请求范围值等功能。
// 通过 context.Background() 或 context.TODO() 创建一个根 Context 对象。
// 通过 context.WithCancel(parent Context)、context.WithDeadline(parent Context, time.Time)、context.WithTimeout(parent Context, time.Duration) 或 context.WithValue(parent Context, key interface{}, val interface{}) 等方法可以从父 Context 派生出新的 Context 对象。
// 任意一个 Context 都可以被用来取消自身或者派生出新的 Context，从而传播取消信号。
// 在并发操作中，可以通过 Context 对象的 .Done() 方法来获取一个通道（channel），当 Context 被取消或者超时时，这个通道会关闭，可以通过监听这个通道来处理取消或超时的情况。
// 总的来说，Context 的基本原理就是通过创建和传播 Context 对象来管理并发操作的请求作用域，实现控制和取消操作、处理超时等功能。这使得在 Go 中编写并发安全的代码变得更加方便和可靠。
package context

import (
	"errors"
	"internal/reflectlite"
	"sync"
	"sync/atomic"
	"time"
)

// Context 在 Go 服务器中，每个传入的请求都在自己的 goroutine 中处理。
// 请求处理程序通常会启动额外的 goroutine 来访问后端，例如 数据库和 RPC 服务。
// 处理请求的 goroutine 集通常需要访问 特定于请求的值，例如最终用户的身份、授权 令牌和请求的截止时间。
// 当请求被取消或超时时，所有 goroutine 都会处理该请求 请求应快速退出，以便系统可以回收它们所在的任何资源 用。
// Context 是一个携带了截止时间、取消信息、以及其他值的、跨API界限的、可以被多个线程同时调用的上下文
type Context interface {
	// Deadline 当将要取消代表此上下文完成的工作时，返回截止时间。截止时间在未设置时返回 ok==false。对 Deadline 的连续调用将返回相同的结果。
	Deadline() (deadline time.Time, ok bool)

	// 	// Stream 使用 DoSomething 生成值并将它们发送到 out，直到 DoSomething 返回错误或 ctx.Done() 是关闭的。
	//  func Stream(ctx context.Context, out chan<- Value) error {
	//  	for {
	//  		v, err := DoSomething(ctx)
	//  		if err != nil {
	//  			return err
	//  		}
	//  		select {
	//  		case <-ctx.Done():
	//  			return ctx.Err()
	//  		case out <- v:
	//  		}
	//  	}
	//  }
	// 当将要取消代表此上下文完成的工作时，返回一个关闭的通道。
	// 如果此上下文永远无法取消，则可能会返回 nil。连续调用返回相同的值。
	// 在取消函数返回后，完成通道的关闭可能会异步发生。
	// WithCancel 安排在调用 cancel 时关闭 Done；
	// WithDeadline 安排在截止日期到期时关闭 Done；
	// WithTimeout 安排在超时过后关闭 Done；
	// 该通道是只读的
	// 在用户执行 case: <- ctx.Done() 时调用，调用Done函数时会惰性创建 chan
	Done() <-chan struct{}

	// Err 如果 Done 尚未关闭，则 Err 返回 nil。如果 Done 已关闭，则 Err 将返回一个非 nil 错误。
	// 如果上下文已取消，则为“Canceled”；
	// 如果上下文的截止时间已过，则为“DeadlineExceeded”；
	// 在 Err 返回非 nil 错误后，对 Err 的连续调用将返回相同的错误。
	Err() error

	// Value 返回与此上下文关联的键值，如果没有值与键关联，则返回 nil。具有相同键的连续调用 Value 将返回相同的结果。
	// 仅对传输进程和 API 边界的请求范围数据使用上下文值，而不对将可选参数传递给函数使用上下文值。
	// 键标识上下文中的特定值。希望在上下文中存储值的函数通常在全局变量中分配一个键，然后将该键用作 context.WithValue 和 Context.Value 的参数。
	// 键可以是任何支持相等的类型，包应将键定义为未导出的类型以避免冲突。定义上下文键的包应为使用该键存储的值提供类型安全的访问器：
	//
	// 	// Package 包 user 定义了一个 User 类型用于在上下文中存储
	// 	package user
	//
	// 	import "context"
	//
	// 	// User 是存储在上下文中的值的类型
	// 	type User struct {...}
	//
	// 	// key 是定义在这个包中未导出的类型，这样可以防止与其他包中定义的 key 发生冲突
	// 	type key int
	//
	// 	// userKey 是存储在上下文中 user.User 类型的值，是非导出的，用户不能直接使用，需要用下面的函数创建
	// 	var userKey key
	//
	// 	// NewContext 返回一个值为 u 的新 Context
	// 	func NewContext(ctx context.Context, u *User) context.Context {
	// 		return context.WithValue(ctx, userKey, u)
	// 	}
	//
	// 	// FromContext 返回一个类型为 User 存储在 ctx 中的值，如果有的话
	// 	func FromContext(ctx context.Context) (*User, bool) {
	// 		u, ok := ctx.Value(userKey).(*User)
	// 		return u, ok
	// 	}
	Value(key interface{}) interface{}
}

// Canceled 上下文取消的错误，当上下文取消时，在调用Context.Err()函数时返回
var Canceled = errors.New("context canceled")

// DeadlineExceeded 上下文过期的错误，当上下文过期时，在调用Context.Err()函数时返回
var DeadlineExceeded error = deadlineExceededError{}

// 实现了 net.Error 接口，能用于网络请求的上下文
type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true } // Is the error a timeout?
func (deadlineExceededError) Temporary() bool { return true } // Is the error temporary?

// 一个空的上下文，emptyCtx 永远不会取消，没有值，也没有截止日期。它不是 struct{}，因为这种类型的变量必须具有不同的地址。
type emptyCtx int

func (*emptyCtx) Deadline() (deadline time.Time, ok bool) {
	return
}
func (*emptyCtx) Done() <-chan struct{} {
	return nil
}
func (*emptyCtx) Err() error {
	return nil
}
func (*emptyCtx) Value(key interface{}) interface{} {
	return nil
}
func (e *emptyCtx) String() string {
	switch e {
	case background:
		return "context.Background"
	case todo:
		return "context.TODO"
	}
	return "unknown empty Context"
}

var (
	background = new(emptyCtx)
	todo       = new(emptyCtx)
)

// Background 它通常由 main 函数、初始化和测试使用，并用作传入请求的顶级上下文。
func Background() Context {
	// 返回一个非 nil 的空上下文。它永远不会被取消，没有值，也没有截止日期。
	return background
}

// TODO 当不清楚要使用哪个上下文或尚不可用时（因为周围的函数尚未扩展为接受上下文参数），则执行 TODO。
func TODO() Context {
	// 返回一个非 nil 的空上下文。代码应使用上下文。
	return todo
}

// CancelFunc 一个函数类型，表示取消函数的具体执行内容
// 告诉操作者放弃其工作，不会等待工作停止
// 能够被多个协程同时调用，当第一次被调用后，后续的调用不会做任何事情
type CancelFunc func()

// WithCancel 返回一个基于 parent 上下文创建的可以取消的子上下文和一个取消函数
// 当返回的取消函数被调用时，或 parent 取消函数中的 Done 管道被关闭时，返回的子上下文中的 Done 管道也会关闭，以先发生者为准
// 取消此上下文会释放与其关联的资源，因此代码应在此上下文中运行的操作完成后立即调用 cancel。
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	c := newCancelCtx(parent)
	propagateCancel(parent, &c)                    // 将自己挂载到 parent，当 parent 取消或管道被关闭时，能自动或手动关闭自己
	return &c, func() { c.cancel(true, Canceled) } // 该取消函数被执行时，一定返回了不为空的error
}

// newCancelCtx 返回一个初始化后的取消上下文
func newCancelCtx(parent Context) cancelCtx {
	return cancelCtx{Context: parent}
}

// goroutines 记录已经创建的 goroutine 的数量，用于测试
var goroutines int32

// propagateCancel 传播取消，安排父上下文被取消时，子上下文也被取消
func propagateCancel(parent Context, child canceler) {
	done := parent.Done()
	if done == nil { // 父节点为空，直接返回
		return
	}

	select {
	case <-done:
		// 该管道为只读，只有关闭后才会触发该条件，读到零值
		// 如果遍历子节点的时候，调用 child.cancel 函数传了 true，还会造成同时遍历和删除一个 map 的境地，会有问题的。
		// 自己会被父节点删除，并置为nil，自己的子节点会自动和自己断绝关系，没必要再传入true
		child.cancel(false, parent.Err()) // 表示父上下文已经取消，直接取消子上下文
		return
	default:
	}

	// 判断 parent 是否为可以取消的 context
	if p, ok := parentCancelCtx(parent); ok {
		// parent 是可以取消的
		p.mu.Lock()
		if p.err != nil { // 父上下文已经取消
			child.cancel(false, p.err) // 表示父上下文已经取消，直接取消子上下文
		} else {
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			// todo important，父节点未取消，将自己挂载到父节点上，才能在父上下文取消的时候自动取消自己
			p.children[child] = struct{}{}
		}
		p.mu.Unlock()
	} else {
		// parent 是不可以取消的
		// 此时 child 无法挂载到 parent，parent 取消时，无法自动取消child
		atomic.AddInt32(&goroutines, +1)
		go func() {
			// 同时监听 parent 和 child，监听到parent关闭时手动关闭child，监听到child被其他协程关闭时退出
			select {
			case <-parent.Done(): // 监视父上下文的管道是否关闭，关闭则取消子上下文并退出
				child.cancel(false, parent.Err())
			case <-child.Done(): // 监视子上下文的管道是否关闭，关闭则退出。若没有此条件，parent上下文也没关闭，则会一直阻塞
			}
		}()
	}
}

// cancelCtx 为自身返回的 key
var cancelCtxKey int

// parentCancelCtx 判断 parent 对象是否为可以取消的上下文，并返回该可取消的上下文 *cancelCtx，
func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	done := parent.Done()
	if done == closedchan || done == nil {
		return nil, false
	}
	// 通过 parent.Value(&cancelCtxKey) 找到里面封装的 *cancelCtx 并检查 parent.Done() 是否匹配 *cancelCtx
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx)
	if !ok { // 判断是否为可以断言为可以取消的上下文
		return nil, false
	}
	// 判断父子是否是同一个done
	pdone, _ := p.done.Load().(chan struct{})
	if pdone != done { // 判断可取消的上下文中的 done 值断言的管道
		return nil, false
	}
	return p, true
}

// removeChild 从父上下文中移除子上下文
func removeChild(parent Context, child canceler) {
	// 判断 parent 是否为可以取消的上下文，只有 cancelCtx 才有子上下文
	p, ok := parentCancelCtx(parent)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.children != nil { // 从父上下文的map中移除自己
		delete(p.children, child)
	}
	p.mu.Unlock()
}

// 取消器是可以直接取消的上下文类型。实现者是 cancelCtx 和 timerCtx。
type canceler interface {
	cancel(removeFromParent bool, err error)
	Done() <-chan struct{}
}

// 可复用的关闭通道
var closedchan = make(chan struct{})

func init() {
	close(closedchan) // todo import, closed the chain at init
}

// cancelCtx 可以被取消。当被取消后，它也能取消所有实现了canceler的子项
type cancelCtx struct {
	Context

	mu       sync.Mutex            // 保护下述字段
	done     atomic.Value          // 原子类型的值，存储了空结构体管道，懒惰式被创建，该取消函数第一次被调用时关闭它
	children map[canceler]struct{} // 存储实现了 canceler 接口的子上下文，该取消函数第一次被调用时置为 nil
	err      error                 // 该取消函数第一次被调用时设置为非空的错误
}

// Value 通过 key 获取 Value，如果 key 是取消上下文的 cancelCtxKey，则返回自身
func (c *cancelCtx) Value(key interface{}) interface{} {
	// 用于判断父上下文对应的对象是否为自己，即是可取消的上下文
	if key == &cancelCtxKey {
		return c
	}
	return c.Context.Value(key)
}

// Done 函数返回的是一个只读的 channel，而且没有地方向这个 channel 里面写数据。
// 所以，直接调用读这个 channel，协程会被 block 住。一般通过搭配 select 来使用。一旦关闭，就会立即读出零值。
func (c *cancelCtx) Done() <-chan struct{} {
	d := c.done.Load() // c.done 是否有值，有则直接断言后返回
	if d != nil {
		return d.(chan struct{})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d = c.done.Load()
	if d == nil { // “懒汉式”创建，只有调用了 Done() 方法的时候才会被创建
		d = make(chan struct{})
		c.done.Store(d)
	}
	return d.(chan struct{})
}

func (c *cancelCtx) Err() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}

type stringer interface {
	String() string
}

func contextName(c Context) string {
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	return reflectlite.TypeOf(c).String()
}

// 获取上下文的名字
func (c *cancelCtx) String() string {
	return contextName(c.Context) + ".WithCancel"
}

// 该取消函数会关闭 c 中 done 管道，递归取消所有的子上下文，如果 removeFromParent 为真，则将 c 从父上下文中移除
func (c *cancelCtx) cancel(removeFromParent bool, err error) {
	if err == nil { // 从被执行的地方传入一个不为空的err，有可能是父上下文的err，有可能是DeadlineExceeded、Canceled
		panic("context: internal error: missing cancel error")
	}
	c.mu.Lock()
	if c.err != nil { // 该上下文的err不为空，说明已经被其他协程执行过取消函数了
		c.mu.Unlock()
		return // already canceled
	}
	c.err = err
	d, _ := c.done.Load().(chan struct{})
	// 关闭该上下文中的管道，通知其他协程
	if d == nil {
		// 表示 Done 函数没有没调用过，给 d 存储一个关闭的管道
		c.done.Store(closedchan)
	} else {
		close(d)
	}
	for child := range c.children {
		// 遍历所有子上下文，并递归执行子函数的取消函数
		child.cancel(false, err)
	}
	c.children = nil
	c.mu.Unlock()

	if removeFromParent { // 从父上下文中移除自己
		removeChild(c.Context, c)
	}
}

// WithDeadline 返回父上下文的副本，并将截止日期调整为不晚于 d。
// 如果父级的截止时间已经早于 d，则 WithDeadline（parent， d） 在语义上等效于父级。
// 当截止时间到期、调用返回的取消函数或父上下文的 Done 通道关闭时，返回的上下文的 Done 通道将关闭，以先发生者为准。
// 取消此上下文会释放与其关联的资源，因此代码应在此上下文中运行的操作完成后立即调用 cancel。
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	// 判断父上下文是否设置了截止时间，以及截止时间是否早于当前设置的截止时间
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		// 父上下文设置的截止时间更早，则直接从父上下文中创建，用的是父上下文的截止时间
		return WithCancel(parent)
	}
	// 父上下文设置的截止时间要晚一些，重新从父上下文中创建，并设置自己的截止时间
	c := &timerCtx{
		cancelCtx: newCancelCtx(parent),
		deadline:  d,
	}
	// 将自己挂载到 parent，当 parent 取消或管道被关闭时，能自动或手动关闭自己
	propagateCancel(parent, c)
	dur := time.Until(d)
	if dur <= 0 {
		c.cancel(true, DeadlineExceeded) // deadline has already passed
		return c, func() { c.cancel(false, Canceled) }
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil { // 表示该上线文还没有被取消
		c.timer = time.AfterFunc(dur, func() { // 为计时器创建一个执行函数，即时间到期后执行该取消函数
			c.cancel(true, DeadlineExceeded)
		})
	}
	return c, func() { c.cancel(true, Canceled) }
}

// timerCtx 带有计时器和截止日期。它嵌入了一个 cancelCtx 来实现 Done 和 Err。
// 它通过停止其计时器然后委托给 cancelCtx.cancel 来实现取消。
type timerCtx struct {
	cancelCtx
	// timer 会在 deadline 到来时，自动取消 context。
	timer    *time.Timer
	deadline time.Time
}

// Deadline 返回 timerCtx 的截止时间
func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}

// 返回 timerCtx 上下文的名字
func (c *timerCtx) String() string {
	return contextName(c.cancelCtx.Context) + ".WithDeadline(" +
		c.deadline.String() + " [" +
		time.Until(c.deadline).String() + "])"
}

func (c *timerCtx) cancel(removeFromParent bool, err error) {
	c.cancelCtx.cancel(false, err) // 执行 cancelCtx 的取消函数
	if removeFromParent {
		// Remove this timerCtx from its parent cancelCtx's children.
		removeChild(c.cancelCtx.Context, c)
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop() // 停止计时
		c.timer = nil
	}
	c.mu.Unlock()
}

// WithTimeout returns WithDeadline(parent, time.Now().Add(timeout)).
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete:
//
// 	func slowOperationWithTimeout(ctx context.Context) (Result, error) {
// 		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
// 		defer cancel()  // releases resources if slowOperation completes before timeout elapses
// 		return slowOperation(ctx)
// 	}
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}

// WithValue 返回父级的副本，其中与 key 关联的值为 val。
// 仅对传输进程和 API 的请求范围的数据使用上下文值，而不对将可选参数传递给函数。
// todo 提供的key必须是可比的，并且不应是字符串类型或任何其他内置类型，以避免使用上下文的包之间发生冲突。
// WithValue 的用户应定义自己的键类型。为了避免在分配给接口{}时进行分配，上下文键通常具有具体的结构类型{}。
// 或者，导出的上下文键变量的静态类型应为指针或接口。
func WithValue(parent Context, key, val interface{}) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if key == nil {
		panic("nil key")
	}
	if !reflectlite.TypeOf(key).Comparable() { // key 是需要可以比较的类型
		panic("key is not comparable")
	}
	return &valueCtx{parent, key, val}
}

// valueCtx 携带一个键值对。它实现该键的值，并将所有其他调用委托给嵌入式上下文。
// key 是需要可以比较的类型
type valueCtx struct {
	Context
	key, val interface{}
}

// stringify 尝试在不使用 fmt 的情况下对 V 进行字符串化，因为我们不希望上下文依赖于 Unicode 表。这仅由 valueCtx.String（） 使用。
func stringify(v interface{}) string {
	switch s := v.(type) {
	case stringer:
		return s.String()
	case string:
		return s
	}
	return "<not Stringer>"
}

func (c *valueCtx) String() string {
	return contextName(c.Context) + ".WithValue(type " +
		reflectlite.TypeOf(c.key).String() +
		", val " + stringify(c.val) + ")"
}

// Value 它会顺着链路一直往上找，比较当前节点的 key
// 是否是要找的 key，如果是，则直接返回 value。否则，一直顺着 context 往上，最终找到根节点（一般是 emptyCtx），直接返回一个 nil。
// 所以用 Value 方法的时候要判断结果是否为 nil。
func (c *valueCtx) Value(key interface{}) interface{} {
	if c.key == key {
		return c.val
	}
	return c.Context.Value(key)
}
