// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// 内置包为 Go 的预声明标识符提供文档。这里记录的项目实际上并不在内置的包中，但它们的描述允许 godoc 提供该语言特殊标识符的文档。
package builtin

// bool 是布尔值的集合，true 和 false。
type bool bool

// true 和 false 是两个非类型化的布尔值。
const (
	true  = 0 == 0 // Untyped bool.
	false = 0 != 0 // Untyped bool.
)

// uint8 是所有无符号 8 位整数的集合。范围：0 到 255。
type uint8 uint8

// uint16 是所有无符号 16 位整数的集合。范围：0 到 65535。
type uint16 uint16

// uint32 是所有无符号 32 位整数的集合。范围：0 到 4294967295。
type uint32 uint32

// uint64 is the set of all unsigned 64-bit integers.
// Range: 0 through 18446744073709551615.
type uint64 uint64

// int8 is the set of all signed 8-bit integers.
// Range: -128 through 127.
type int8 int8

// int16 is the set of all signed 16-bit integers.
// Range: -32768 through 32767.
type int16 int16

// int32 is the set of all signed 32-bit integers.
// Range: -2147483648 through 2147483647.
type int32 int32

//int64 是所有有符号 64 位整数的集合。范围：-9223372036854775808 到 9223372036854775807。
type int64 int64

// float32 是所有 IEEE-754 32 位浮点数的集合。
type float32 float32

// float64 是所有 IEEE-754 64 位浮点数的集合。
type float64 float64

// complex64 是具有 float32 实部和虚部的所有复数的集合。
type complex64 complex64

// complex128 是具有 float64 实部和虚部的所有复数的集合。
type complex128 complex128

// string 是所有 8 位字节字符串的集合，通常但不一定表示 UTF-8 编码的文本。字符串可以是空的，但不是零。字符串类型的值是不可变的。
type string string

// int 是大小至少为 32 位的有符号整数类型。但是，它是一种独特的类型，而不是 int32 的别名。
type int int

// uint 是大小至少为 32 位的无符号整数类型。但是，它是一种独特的类型，而不是 uint32 的别名。
type uint uint

// uintptr 是一个整数类型，它足够大，可以容纳任何指针的位模式。
type uintptr uintptr

// byte 是 uint8 的别名，在所有方面都等同于 uint8。按照惯例，它用于区分字节值和 8 位无符号整数值。
type byte = uint8

// rune 是 int32 的别名，在所有方面都等同于 int32。按照惯例，它用于区分字符值和整数值。
type rune = int32

// 是一个预先声明的标识符，表示（通常用括号括起来的）const 声明中当前 const 规范的无类型整数序号。它是零索引的。
const iota = 0 // Untyped int.

// nil 是一个预先声明的标识符，表示指针、通道、func、接口、映射或切片类型的零值。
var nil Type // Type must be a pointer, channel, func, interface, map, or slice type

// Type is here for the purposes of documentation only. It is a stand-in
// for any Go type, but represents the same type for any given function
// invocation.
type Type int

// Type1 is here for the purposes of documentation only. It is a stand-in
// for any Go type, but represents the same type for any given function
// invocation.
type Type1 int

// IntegerType is here for the purposes of documentation only. It is a stand-in
// for any integer type: int, uint, int8 etc.
type IntegerType int

// FloatType is here for the purposes of documentation only. It is a stand-in
// for either float type: float32 or float64.
type FloatType float32

// ComplexType is here for the purposes of documentation only. It is a
// stand-in for either complex type: complex64 or complex128.
type ComplexType complex64

// append 内置函数将元素追加到切片的末尾。如果它有足够的容量，则对目标进行重新切片以容纳新元素。
// 如果没有，将分配一个新的基础数组。Append 返回更新的切片。
// 因此，有必要将 append 的结果存储在保存切片本身的变量中：
// slice = append（slice， elem1， elem2） slice = append（slice， anotherSlice...）
// 作为一种特殊情况，将字符串附加到字节切片是合法的，如下所示：
// slice = append（[]byte（“hello ”）， “world”...）
func append(slice []Type, elems ...Type) []Type

// copy 内置函数将元素从源切片复制到目标切片中。（作为特殊情况，它还会将字节从字符串复制到字节切片。
// 源和目标可能重叠。copy 返回复制的元素数，即 len（src） 和 len（dst） 的最小值。
func copy(dst, src []Type) int

// delete 内置函数从映射中删除具有指定键 （m[key]） 的元素。如果 m 为 nil 或没有此类元素，则 delete 为空操作。
func delete(m map[Type]Type1, key Type)

// len 内置函数根据其类型返回 v 的长度：
// Array：v 中的元素数量。
// 指向数组的指针：v 中的元素数（即使 v 为 nil）。
// 切片或映射：v 中的元素数量；如果 v 为 nil，则 len（v） 为零。
// String：v中的字节数。
// Channel：通道缓冲区中排队（未读）的元素数；如果 v 为 nil，则 len（v） 为零。
// 对于某些参数（如字符串文本或简单数组表达式），结果可以是常量。有关详细信息，请参阅 Go 语言规范的“长度和容量”部分。
func len(v Type) int

// The cap built-in function returns the capacity of v, according to its type:
//	Array: the number of elements in v (same as len(v)).
//	Pointer to array: the number of elements in *v (same as len(v)).
//	Slice: the maximum length the slice can reach when resliced;
//	if v is nil, cap(v) is zero.
//	Channel: the channel buffer capacity, in units of elements;
//	if v is nil, cap(v) is zero.
// For some arguments, such as a simple array expression, the result can be a
// constant. See the Go language specification's "Length and capacity" section for
// details.
// cap 内置函数根据其类型返回 v 的容量：
// Array：v 中的元素数量（与 len（v） 相同）。
// 数组指针：v 中的元素数（与 len（v） 相同）。
// 切片：切片重新切片时可以达到的最大长度；如果 v 为 nil，则 cap（v） 为零。
// 通道：通道缓冲容量，单位为元素;如果 v 为 nil，则 cap（v） 为零。
func cap(v Type) int

// The make built-in function allocates and initializes an object of type
// slice, map, or chan (only). Like new, the first argument is a type, not a
// value. Unlike new, make's return type is the same as the type of its
// argument, not a pointer to it. The specification of the result depends on
// the type:
//	Slice: The size specifies the length. The capacity of the slice is
//	equal to its length. A second integer argument may be provided to
//	specify a different capacity; it must be no smaller than the
//	length. For example, make([]int, 0, 10) allocates an underlying array
//	of size 10 and returns a slice of length 0 and capacity 10 that is
//	backed by this underlying array.
//	Map: An empty map is allocated with enough space to hold the
//	specified number of elements. The size may be omitted, in which case
//	a small starting size is allocated.
//	Channel: The channel's buffer is initialized with the specified
//	buffer capacity. If zero, or the size is omitted, the channel is
//	unbuffered.
func make(t Type, size ...IntegerType) Type

// The new built-in function allocates memory. The first argument is a type,
// not a value, and the value returned is a pointer to a newly
// allocated zero value of that type.
func new(Type) *Type

// The complex built-in function constructs a complex value from two
// floating-point values. The real and imaginary parts must be of the same
// size, either float32 or float64 (or assignable to them), and the return
// value will be the corresponding complex type (complex64 for float32,
// complex128 for float64).
func complex(r, i FloatType) ComplexType

// The real built-in function returns the real part of the complex number c.
// The return value will be floating point type corresponding to the type of c.
func real(c ComplexType) FloatType

// The imag built-in function returns the imaginary part of the complex
// number c. The return value will be floating point type corresponding to
// the type of c.
func imag(c ComplexType) FloatType

// The close built-in function closes a channel, which must be either
// bidirectional or send-only. It should be executed only by the sender,
// never the receiver, and has the effect of shutting down the channel after
// the last sent value is received. After the last value has been received
// from a closed channel c, any receive from c will succeed without
// blocking, returning the zero value for the channel element. The form
//	x, ok := <-c
// will also set ok to false for a closed channel.
func close(c chan<- Type)

// The panic built-in function stops normal execution of the current
// goroutine. When a function F calls panic, normal execution of F stops
// immediately. Any functions whose execution was deferred by F are run in
// the usual way, and then F returns to its caller. To the caller G, the
// invocation of F then behaves like a call to panic, terminating G's
// execution and running any deferred functions. This continues until all
// functions in the executing goroutine have stopped, in reverse order. At
// that point, the program is terminated with a non-zero exit code. This
// termination sequence is called panicking and can be controlled by the
// built-in function recover.
func panic(v interface{})

// The recover built-in function allows a program to manage behavior of a
// panicking goroutine. Executing a call to recover inside a deferred
// function (but not any function called by it) stops the panicking sequence
// by restoring normal execution and retrieves the error value passed to the
// call of panic. If recover is called outside the deferred function it will
// not stop a panicking sequence. In this case, or when the goroutine is not
// panicking, or if the argument supplied to panic was nil, recover returns
// nil. Thus the return value from recover reports whether the goroutine is
// panicking.
func recover() interface{}

// The print built-in function formats its arguments in an
// implementation-specific way and writes the result to standard error.
// Print is useful for bootstrapping and debugging; it is not guaranteed
// to stay in the language.
func print(args ...Type)

// The println built-in function formats its arguments in an
// implementation-specific way and writes the result to standard error.
// Spaces are always added between arguments and a newline is appended.
// Println is useful for bootstrapping and debugging; it is not guaranteed
// to stay in the language.
func println(args ...Type)

// The error built-in interface type is the conventional interface for
// representing an error condition, with the nil value representing no error.
type error interface {
	Error() string
}
