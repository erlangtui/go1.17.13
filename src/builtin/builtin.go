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

// cap 内置函数根据其类型返回 v 的容量：
// Array：v 中的元素数量（与 len（v） 相同）。
// 数组指针：v 中的元素数（与 len（v） 相同）。
// 切片：切片重新切片时可以达到的最大长度；如果 v 为 nil，则 cap（v） 为零。
// 通道：通道缓冲容量，单位为元素;如果 v 为 nil，则 cap（v） 为零。
func cap(v Type) int

//
// make 内置函数分配并初始化 slice、map 或 chan 类型的对象（仅）。
// 与 new 一样，第一个参数是类型，而不是值。与 new 不同，make 的返回类型与其参数的类型相同，而不是指向它的指针。
// 结果的规范取决于类型：
// 切片：大小指定长度。切片的容量等于其长度。可以提供第二个整数参数来指定不同的容量;它必须不小于长度。
// 例如，make（[]int， 0， 10） 分配一个大小为 10 的基础数组，并返回由此基础数组支持的长度为 0 和容量为 10 的切片。
// 地图：为空地图分配足够的空间来容纳指定数量的元素。可以省略大小，在这种情况下，将分配较小的起始大小。
// 通道：通道的缓冲区使用指定的缓冲区容量进行初始化。如果为零或省略大小，则通道无缓冲。
func make(t Type, size ...IntegerType) Type

// new 内置函数分配内存。第一个参数是类型，而不是值，返回的值是指向该类型新分配的零值的指针。
func new(Type) *Type

// complex 内置函数从两个浮点值构造一个复数值。
// 实部和虚部必须具有相同的大小，即 float32 或 float64 ，
// 并且返回值将是相应的复数类型（float32 为 complex64，float64 为 complex128）。
func complex(r, i FloatType) ComplexType

// real 内置函数返回复数 c 的实数部分。返回值将是与 c 类型对应的浮点类型。
func real(c ComplexType) FloatType

// imag 内置函数返回复数 c 的虚部。返回值将是与 c 类型对应的浮点类型。
func imag(c ComplexType) FloatType

// close 内置函数关闭通道，该通道必须是双向的或仅发送的。
// 通道应该只由发送方执行，而不是由接收方执行，并且具有在接收到最后一个发送的值后关闭通道的效果。
// 从关闭后的通道 c 接收到最后一个值后，c 的任何接收 goroutine 都将成功接收到通道元素的零值，而不会被阻塞。
// 对于关闭后的通道，x, ok := <-c 会将 ok 设置为 false。
func close(c chan<- Type)

// panic 内置函数停止当前 goroutine 的正常执行。当函数 F 调用 panic 时，F 的正常执行会立即停止。
// 任何被 F 延迟执行的函数都以平常的方式运行，然后 F 返回给其调用方。
// 对于调用方 G，对 F 的调用就像对 panic 的调用一样，终止 G 的执行并运行任何延迟的函数。
// 这种情况一直持续到执行 goroutine 中的所有函数以相反的顺序停止为止。此时，程序将终止，并带有非零退出代码。
// 此终止序列称为 panicking，可通过内置函数 recover 进行控制。
func panic(v interface{})

// recover 内置函数允许程序管理 goroutine 的 panic 行为，且只能捕获当前 goroutine。
// 在延迟函数（但不是它调用的任何函数）中执行恢复调用会通过恢复正常执行来停止 panic 序列，并检索传递给 panic 调用的错误值。
// 如果在延迟函数之外调用 recover，则不会停止 panic 序列。
// 当 goroutine 没有 panic ，或者 panic 的参数为 nil，则 recover 返回 nil。
// recover 的返回值能够说明 goroutine 是否处于 panic 状态。
func recover() interface{}

// print 内置函数以特定地实现方式格式化其参数，并将结果写入标准错误。
// 打印对于引导和调试很有用，它不能保证保留在语言中。
func print(args ...Type)

// println 内置函数以特定地实现方式格式化其参数，并将结果写入标准错误。
// 始终在参数之间添加空格，并附加换行符。
// Println 对于引导和调试很有用，它不能保证保留在语言中。
func println(args ...Type)

// The error built-in interface type is the conventional interface for
// representing an error condition, with the nil value representing no error.
type error interface {
	Error() string
}
