// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package context defines the Context type, which carries deadlines,
// cancellation signals, and other request-scoped values across API boundaries
// and between processes.
//
// Incoming requests to a server should create a Context, and outgoing
// calls to servers should accept a Context. The chain of function
// calls between them must propagate the Context, optionally replacing
// it with a derived Context created using WithCancel, WithDeadline,
// WithTimeout, or WithValue. When a Context is canceled, all
// Contexts derived from it are also canceled.
//
// The WithCancel, WithDeadline, and WithTimeout functions take a
// Context (the parent) and return a derived Context (the child) and a
// CancelFunc. Calling the CancelFunc cancels the child and its
// children, removes the parent's reference to the child, and stops
// any associated timers. Failing to call the CancelFunc leaks the
// child and its children until the parent is canceled or the timer
// fires. The go vet tool checks that CancelFuncs are used on all
// control-flow paths.
//
// Programs that use Contexts should follow these rules to keep interfaces
// consistent across packages and enable static analysis tools to check context
// propagation:
//
// Do not store Contexts inside a struct type; instead, pass a Context
// explicitly to each function that needs it. The Context should be the first
// parameter, typically named ctx:
//
// 	func DoSomething(ctx context.Context, arg Arg) error {
// 		// ... use ctx ...
// 	}
//
// Do not pass a nil Context, even if a function permits it. Pass context.TODO
// if you are unsure about which Context to use.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The same Context may be passed to functions running in different goroutines;
// Contexts are safe for simultaneous use by multiple goroutines.
//
// See https://blog.golang.org/context for example code for a server that uses
// Contexts.
package context

import (
	"errors"
	"internal/reflectlite"
	"sync"
	"sync/atomic"
	"time"
)

// A Context carries a deadline, a cancellation signal, and other values across API boundaries.
// Context's methods may be called by multiple goroutines simultaneously.
// Context's methods may be called by multiple goroutines simultaneously.
//  一个携带了截止时间、取消信息、以及其他值的跨API界限的上下文可以被多个线程同时调用
type Context interface {
	// Deadline returns the time when work done on behalf of this context
	// should be canceled. Deadline returns ok==false when no deadline is set.
	// Successive calls to Deadline return the same results.
	// 当将要取消代表此上下文完成的工作时，返回截止时间。截止时间在未设置时返回 ok==false。对 Deadline 的连续调用将返回相同的结果。
	Deadline() (deadline time.Time, ok bool)

	// Done returns a channel that's closed when work done on behalf of this
	// context should be canceled. Done may return nil if this context can
	// never be canceled. Successive calls to Done return the same value.
	// The close of the Done channel may happen asynchronously,
	// after the cancel function returns.
	//
	// WithCancel arranges for Done to be closed when cancel is called;
	// WithDeadline arranges for Done to be closed when the deadline
	// expires; WithTimeout arranges for Done to be closed when the timeout
	// elapses.
	//
	// Done is provided for use in select statements:
	//
	//  // Stream generates values with DoSomething and sends them to out
	//  // until DoSomething returns an error or ctx.Done is closed.
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
	//
	// See https://blog.golang.org/pipelines for more examples of how to use
	// a Done channel for cancellation.
	// 当将要取消代表此上下文完成的工作时，返回一个关闭的通道。
	// 如果此上下文永远无法取消，则可能会返回 nil。连续调用返回相同的值。
	// 在取消函数返回后，完成通道的关闭可能会异步发生。
	// WithCancel 安排在调用 cancel 时关闭 Done;WithDeadline 安排在截止日期到期时关闭 Done; WithTimeout 安排在超时过后关闭 Done。
	// 该通道是只读的
	Done() <-chan struct{}

	// If Done is not yet closed, Err returns nil.
	// If Done is closed, Err returns a non-nil error explaining why:
	// Canceled if the context was canceled
	// or DeadlineExceeded if the context's deadline passed.
	// After Err returns a non-nil error, successive calls to Err return the same error.
	// 如果 Done 尚未关闭，则 Err 返回 nil。如果 Done 已关闭，则 Err 将返回一个非 nil 错误。
	// 如果上下文已取消，则为“Canceled”;如果上下文的截止时间已过，则为“DeadlineExceeded”。
	// 在 Err 返回非 nil 错误后，对 Err 的连续调用将返回相同的错误。
	Err() error

	// Value returns the value associated with this context for key, or nil
	// if no value is associated with key. Successive calls to Value with
	// the same key returns the same result.
	//
	// Use context values only for request-scoped data that transits
	// processes and API boundaries, not for passing optional parameters to
	// functions.
	//
	// A key identifies a specific value in a Context. Functions that wish
	// to store values in Context typically allocate a key in a global
	// variable then use that key as the argument to context.WithValue and
	// Context.Value. A key can be any type that supports equality;
	// packages should define keys as an unexported type to avoid
	// collisions.
	//
	// Packages that define a Context key should provide type-safe accessors
	// for the values stored using that key:
	//
	// 	// Package user defines a User type that's stored in Contexts.
	// 	package user
	//
	// 	import "context"
	//
	// 	// User is the type of value stored in the Contexts.
	// 	type User struct {...}
	//
	// 	// key is an unexported type for keys defined in this package.
	// 	// This prevents collisions with keys defined in other packages.
	// 	type key int
	//
	// 	// userKey is the key for user.User values in Contexts. It is
	// 	// unexported; clients use user.NewContext and user.FromContext
	// 	// instead of using this key directly.
	// 	var userKey key
	//
	// 	// NewContext returns a new Context that carries value u.
	// 	func NewContext(ctx context.Context, u *User) context.Context {
	// 		return context.WithValue(ctx, userKey, u)
	// 	}
	//
	// 	// FromContext returns the User value stored in ctx, if any.
	// 	func FromContext(ctx context.Context) (*User, bool) {
	// 		u, ok := ctx.Value(userKey).(*User)
	// 		return u, ok
	// 	}
	// Value 返回与此上下文关联的键值，如果没有值与键关联，则返回 nil。具有相同键的连续调用 Value 将返回相同的结果。
	// 仅对传输进程和 API 边界的请求范围数据使用上下文值，而不对将可选参数传递给函数使用上下文值。
	// 键标识上下文中的特定值。希望在上下文中存储值的函数通常在全局变量中分配一个键，然后将该键用作context.WithValue 和 Context.Value的参数。
	// 键可以是支持相等的任何类型;包应将键定义为未导出的类型以避免冲突。定义上下文键的包应为使用该键存储的值提供类型安全的访问器：
	Value(key interface{}) interface{}
}

// Canceled is the error returned by Context.Err when the context is canceled.
// 上下文取消的错误，当上下文取消时，在调用Context.Err()函数时返回
var Canceled = errors.New("context canceled")

// DeadlineExceeded is the error returned by Context.Err when the context's deadline passes.
// 上下文过期的错误，当上下文过期时，在调用Context.Err()函数时返回
var DeadlineExceeded error = deadlineExceededError{}

type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }

// An emptyCtx is never canceled, has no values, and has no deadline. It is not
// struct{}, since vars of this type must have distinct addresses.
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

// Background returns a non-nil, empty Context. It is never canceled, has no
// values, and has no deadline. It is typically used by the main function,
// initialization, and tests, and as the top-level Context for incoming requests.
// Background 返回一个非 nil 的空上下文。它永远不会被取消，没有值，也没有截止日期。
// 它通常由 main 函数、初始化和测试使用，并用作传入请求的顶级上下文。
func Background() Context {
	return background
}

// TODO returns a non-nil, empty Context. Code should use context.TODO when
// it's unclear which Context to use or it is not yet available (because the
// surrounding function has not yet been extended to accept a Context parameter).
// TODO 返回一个非 nil 的空上下文。代码应使用上下文。当不清楚要使用哪个上下文或尚不可用时（因为周围的函数尚未扩展为接受上下文参数），则执行 TODO。
func TODO() Context {
	return todo
}

// A CancelFunc tells an operation to abandon its work.
// A CancelFunc does not wait for the work to stop.
// A CancelFunc may be called by multiple goroutines simultaneously.
// After the first call, subsequent calls to a CancelFunc do nothing.
// CancelFunc，一个函数类型，表示取消函数的具体执行内容
// 告诉操作者放弃其工作，不会等待工作停止
// 能够被多个协程同时调用，当第一次被调用后，后续的调用不会做任何事情
type CancelFunc func()

// WithCancel returns a copy of parent with a new Done channel. The returned
// context's Done channel is closed when the returned cancel function is called
// or when the parent context's Done channel is closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
// 返回一个基于 parent 上下文创建的可以取消的子上下文和一个取消函数
// 当返回的取消函数被调用时，或 parent 取消函数中的 Done 管道被关闭时，返回的子上下文中的 Done 管道也会关闭，以先发生者为准
// 取消此上下文会释放与其关联的资源，因此代码应在此上下文中运行的操作完成后立即调用 cancel。
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	c := newCancelCtx(parent)
	propagateCancel(parent, &c) // 将自己挂载到 parent，当 parent 取消或管道被关闭时，能自动或手动关闭自己
	return &c, func() { c.cancel(true, Canceled) } // 该取消函数被执行时，一定返回了不为空的error
}

// newCancelCtx returns an initialized cancelCtx.
// newCancelCtx 返回一个初始化后的取消上下文
func newCancelCtx(parent Context) cancelCtx {
	return cancelCtx{Context: parent}
}

// goroutines counts the number of goroutines ever created; for testing.
// goroutines 记录已经创建的 goroutine 的数量，用于测试
var goroutines int32

// propagateCancel arranges for child to be canceled when parent is.
// propagateCancel 传播取消，安排父上下文被取消时，子上下文也被取消
func propagateCancel(parent Context, child canceler) {
	done := parent.Done()
	if done == nil { // 父节点为空，直接返回
		return // parent is never canceled
	}

	select {
	case <-done: // 该管道为只读，只有关闭后才会触发该条件，读到零值
		// parent is already canceled
		// 如果遍历子节点的时候，调用 child.cancel 函数传了 true，还会造成同时遍历和删除一个 map 的境地，会有问题的。
		// 自己会被父节点删除，并置为nil，自己的子节点会自动和自己断绝关系，没必要再传入true
		child.cancel(false, parent.Err()) // 表示父上下文已经取消，直接取消子上下文
		return
	default:
	}

	// 找到可取消的父上下文
	if p, ok := parentCancelCtx(parent); ok {// 找到了可取消的父上下文
		p.mu.Lock()
		if p.err != nil { // 父上下文已经取消
			// parent has already been canceled
			child.cancel(false, p.err) // 表示父上下文已经取消，直接取消子上下文
		} else {
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			// todo import，父节点未取消，将自己挂载到父节点上，才能在父上下文取消的时候自动取消自己
			p.children[child] = struct{}{}
		}
		p.mu.Unlock()
	} else { // 未找到可取消的父上下文
		// 此时 child 无法挂载到 parent，parent 取消是，无法自动取消child
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

// &cancelCtxKey is the key that a cancelCtx returns itself for.
// cancelCtx 为自身返回的key
var cancelCtxKey int

// parentCancelCtx returns the underlying *cancelCtx for parent.
// It does this by looking up parent.Value(&cancelCtxKey) to find
// the innermost enclosing *cancelCtx and then checking whether
// parent.Done() matches that *cancelCtx. (If not, the *cancelCtx
// has been wrapped in a custom implementation providing a
// different done channel, in which case we should not bypass it.)
// 判断 parent 对象是否为可以取消的上下文，并返回该可取消的上下文 *cancelCtx，
// 通过 parent.Value(&cancelCtxKey) 找到里面封装的 *cancelCtx 并检查 parent.Done() 是否匹配 *cancelCtx
func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	done := parent.Done()
	if done == closedchan || done == nil {
		return nil, false
	}
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx)
	if !ok { // 判断是否为可以断言为可以取消的上下文
		return nil, false
	}
	pdone, _ := p.done.Load().(chan struct{})
	if pdone != done { // 判断可取消的上下文中的 done 值断言的管道
		return nil, false
	}
	return p, true
}

// removeChild removes a context from its parent.
// 从父上下文中移除子上下文
func removeChild(parent Context, child canceler) {
	p, ok := parentCancelCtx(parent) // 判断 parent 是否为可以取消的上下文
	if !ok {
		return
	}
	p.mu.Lock()
	if p.children != nil { // 从父上下文的map中移除自己
		delete(p.children, child)
	}
	p.mu.Unlock()
}

// A canceler is a context type that can be canceled directly. The
// implementations are *cancelCtx and *timerCtx.
// 取消器是可以直接取消的上下文类型。实现者是 cancelCtx 和 timerCtx。
type canceler interface {
	cancel(removeFromParent bool, err error)
	Done() <-chan struct{}
}

// closedchan is a reusable closed channel.
// 可复用的关闭通道
var closedchan = make(chan struct{})

func init() {
	close(closedchan) // todo import, closed the chain at init
}

// A cancelCtx can be canceled. When canceled, it also cancels any children that implement canceler.
// cancelCtx 可以被取消。当被取消后，它也能取消所有实现了canceler的子项
type cancelCtx struct {
	Context

	mu sync.Mutex // protects following fields
	// 原子类型的值，存储了空结构体管道，懒惰式被创建，该取消函数第一次被调用时关闭它
	done atomic.Value // of chan struct{}, created lazily, closed by first cancel call
	// 存储实现了 canceler 接口的子上下文，该取消函数第一次被调用时置为nil
	children map[canceler]struct{} // set to nil by the first cancel call
	// 该取消函数第一次被调用时设置为非空的错误
	err error // set to non-nil by the first cancel call
}

func (c *cancelCtx) Value(key interface{}) interface{} {
	// 通过key获取Value，如果key是取消上下文的key，则返回自身
	// 用于判断父上下文对应的对象是否为自己，即是可取消的上下文
	if key == &cancelCtxKey {
		return c
	}
	return c.Context.Value(key)
}

// 函数返回的是一个只读的 channel，而且没有地方向这个 channel 里面写数据。
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

// cancel closes c.done, cancels each of c's children, and, if
// removeFromParent is true, removes c from its parent's children.
// 该取消函数会关闭 c 中 done 管道，取消所有的子上下文，如果 removeFromParent 为真，则将 c 从父上下文中移除
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
		c.done.Store(closedchan)
	} else {
		close(d)
	}
	for child := range c.children {
		// NOTE: acquiring the child's lock while holding parent's lock.
		// 遍历所有子上下文，并递归执行取消函数
		child.cancel(false, err)
	}
	c.children = nil
	c.mu.Unlock()

	if removeFromParent { // 从父上下文中移除自己
		removeChild(c.Context, c)
	}
}

// WithDeadline returns a copy of the parent context with the deadline adjusted
// to be no later than d. If the parent's deadline is already earlier than d,
// WithDeadline(parent, d) is semantically equivalent to parent. The returned
// context's Done channel is closed when the deadline expires, when the returned
// cancel function is called, or when the parent context's Done channel is
// closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
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
		// The current deadline is already sooner than the new one.
		// 父上下文设置的截止时间更早，则直接从父上下文中创建，用的是父上下文的截止时间
		return WithCancel(parent)
	}
	// 父上下文设置的截止时间要晚一些，重新从父上下文中创建，并设置自己的截止时间
	c := &timerCtx{
		cancelCtx: newCancelCtx(parent),
		deadline:  d,
	}
	propagateCancel(parent, c) // 将自己挂载到 parent，当 parent 取消或管道被关闭时，能自动或手动关闭自己
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

// A timerCtx carries a timer and a deadline. It embeds a cancelCtx to
// implement Done and Err. It implements cancel by stopping its timer then
// delegating to cancelCtx.cancel.
// timerCtx带有计时器和截止日期。它嵌入了一个 cancelCtx 来实现 Done 和 Err。
// 它通过停止其计时器然后委托给 cancelCtx.cancel 来实现取消。
type timerCtx struct {
	cancelCtx
	timer *time.Timer // Under cancelCtx.mu.
	// Timer 会在 deadline 到来时，自动取消 context。
	deadline time.Time
}

// 返回 timerCtx 的截止时间
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
	c.cancelCtx.cancel(false, err) // todo 执行 cancelCtx 的取消函数？
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

// WithValue returns a copy of parent in which the value associated with key is val.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The provided key must be comparable and should not be of type
// string or any other built-in type to avoid collisions between
// packages using context. Users of WithValue should define their own
// types for keys. To avoid allocating when assigning to an
// interface{}, context keys often have concrete type
// struct{}. Alternatively, exported context key variables' static
// type should be a pointer or interface.
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
	if !reflectlite.TypeOf(key).Comparable() {// key 是需要可以比较的类型
		panic("key is not comparable")
	}
	return &valueCtx{parent, key, val}
}

// A valueCtx carries a key-value pair. It implements Value for that key and
// delegates all other calls to the embedded Context.
// valueCtx 携带一个键值对。它实现该键的值，并将所有其他调用委托给嵌入式上下文。
// key 是需要可以比较的类型
type valueCtx struct {
	Context
	key, val interface{}
}

// stringify tries a bit to stringify v, without using fmt, since we don't
// want context depending on the unicode tables. This is only used by
// *valueCtx.String().
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
// 它会顺着链路一直往上找，比较当前节点的 key
// 是否是要找的 key，如果是，则直接返回 value。否则，一直顺着 context 往上，最终找到根节点（一般是 emptyCtx），直接返回一个 nil。
// 所以用 Value 方法的时候要判断结果是否为 nil。
func (c *valueCtx) Value(key interface{}) interface{} {
	if c.key == key {
		return c.val
	}
	return c.Context.Value(key)
}
