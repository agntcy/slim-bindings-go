package slim_rpc

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo linux,amd64 LDFLAGS: -L${SRCDIR}/.. -L${SRCDIR}/../../../../../../../.cgo-cache/slim-bindings/v2.1.1 -lslim_bindings_x86_64_linux_gnu -lm
#cgo linux,arm64 LDFLAGS: -L${SRCDIR}/.. -L${SRCDIR}/../../../../../../../.cgo-cache/slim-bindings/v2.1.1 -lslim_bindings_aarch64_linux_gnu -lm
#cgo darwin,amd64 LDFLAGS: -L${SRCDIR}/.. -L${SRCDIR}/../../../../../../../.cgo-cache/slim-bindings/v2.1.1 -lslim_bindings_x86_64_darwin -Wl,-undefined,dynamic_lookup
#cgo darwin,arm64 LDFLAGS: -L${SRCDIR}/.. -L${SRCDIR}/../../../../../../../.cgo-cache/slim-bindings/v2.1.1 -lslim_bindings_aarch64_darwin -Wl,-undefined,dynamic_lookup
#cgo windows,amd64 LDFLAGS: -L${SRCDIR}/.. -L${SRCDIR}/../../../../../../../.cgo-cache/slim-bindings/v2.1.1 -lslim_bindings_x86_64_windows_gnu -lws2_32 -lbcrypt -ladvapi32 -luserenv -lntdll -lgcc_eh -lgcc -lkernel32 -lole32
#include <slim_rpc.h>
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"
	"runtime/cgo"
	slim_bindings "github.com/agntcy/slim-bindings-go/v2"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// This is needed, because as of go 1.24
// type RustBuffer C.RustBuffer cannot have methods,
// RustBuffer is treated as non-local type
type GoRustBuffer struct {
	inner C.RustBuffer
}

type RustBufferI interface {
	AsReader() *bytes.Reader
	Free()
	ToGoBytes() []byte
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

// C.RustBuffer fields exposed as an interface so they can be accessed in different Go packages.
// See https://github.com/golang/go/issues/13467
type ExternalCRustBuffer interface {
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

func RustBufferFromC(b C.RustBuffer) ExternalCRustBuffer {
	return GoRustBuffer{
		inner: b,
	}
}

func CFromRustBuffer(b ExternalCRustBuffer) C.RustBuffer {
	return C.RustBuffer{
		capacity: C.uint64_t(b.Capacity()),
		len:      C.uint64_t(b.Len()),
		data:     (*C.uchar)(b.Data()),
	}
}

func RustBufferFromExternal(b ExternalCRustBuffer) GoRustBuffer {
	return GoRustBuffer{
		inner: C.RustBuffer{
			capacity: C.uint64_t(b.Capacity()),
			len:      C.uint64_t(b.Len()),
			data:     (*C.uchar)(b.Data()),
		},
	}
}

func (cb GoRustBuffer) Capacity() uint64 {
	return uint64(cb.inner.capacity)
}

func (cb GoRustBuffer) Len() uint64 {
	return uint64(cb.inner.len)
}

func (cb GoRustBuffer) Data() unsafe.Pointer {
	return unsafe.Pointer(cb.inner.data)
}

func (cb GoRustBuffer) AsReader() *bytes.Reader {
	b := unsafe.Slice((*byte)(cb.inner.data), C.uint64_t(cb.inner.len))
	return bytes.NewReader(b)
}

func (cb GoRustBuffer) Free() {
	rustCall(func(status *C.RustCallStatus) bool {
		C.ffi_slim_rpc_rustbuffer_free(cb.inner, status)
		return false
	})
}

func (cb GoRustBuffer) ToGoBytes() []byte {
	return C.GoBytes(unsafe.Pointer(cb.inner.data), C.int(cb.inner.len))
}

func stringToRustBuffer(str string) C.RustBuffer {
	return bytesToRustBuffer([]byte(str))
}

func bytesToRustBuffer(b []byte) C.RustBuffer {
	if len(b) == 0 {
		return C.RustBuffer{}
	}
	// We can pass the pointer along here, as it is pinned
	// for the duration of this call
	foreign := C.ForeignBytes{
		len:  C.int(len(b)),
		data: (*C.uchar)(unsafe.Pointer(&b[0])),
	}

	return rustCall(func(status *C.RustCallStatus) C.RustBuffer {
		return C.ffi_slim_rpc_rustbuffer_from_bytes(foreign, status)
	})
}

type BufLifter[GoType any] interface {
	Lift(value RustBufferI) GoType
}

type BufLowerer[GoType any] interface {
	Lower(value GoType) C.RustBuffer
}

type BufReader[GoType any] interface {
	Read(reader io.Reader) GoType
}

type BufWriter[GoType any] interface {
	Write(writer io.Writer, value GoType)
}

func LowerIntoRustBuffer[GoType any](bufWriter BufWriter[GoType], value GoType) C.RustBuffer {
	// This might be not the most efficient way but it does not require knowing allocation size
	// beforehand
	var buffer bytes.Buffer
	bufWriter.Write(&buffer, value)

	bytes, err := io.ReadAll(&buffer)
	if err != nil {
		panic(fmt.Errorf("reading written data: %w", err))
	}
	return bytesToRustBuffer(bytes)
}

func LiftFromRustBuffer[GoType any](bufReader BufReader[GoType], rbuf RustBufferI) GoType {
	defer rbuf.Free()
	reader := rbuf.AsReader()
	item := bufReader.Read(reader)
	if reader.Len() > 0 {
		// TODO: Remove this
		leftover, _ := io.ReadAll(reader)
		panic(fmt.Errorf("Junk remaining in buffer after lifting: %s", string(leftover)))
	}
	return item
}

func rustCallWithError[E any, U any](converter BufReader[E], callback func(*C.RustCallStatus) U) (U, E) {
	var status C.RustCallStatus
	returnValue := callback(&status)
	err := checkCallStatus(converter, status)
	return returnValue, err
}

func checkCallStatus[E any](converter BufReader[E], status C.RustCallStatus) E {
	switch status.code {
	case 0:
		var zero E
		return zero
	case 1:
		return LiftFromRustBuffer(converter, GoRustBuffer{inner: status.errorBuf})
	case 2:
		// when the rust code sees a panic, it tries to construct a rustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{inner: status.errorBuf})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		panic(fmt.Errorf("unknown status code: %d", status.code))
	}
}

func checkCallStatusUnknown(status C.RustCallStatus) error {
	switch status.code {
	case 0:
		return nil
	case 1:
		panic(fmt.Errorf("function not returning an error returned an error"))
	case 2:
		// when the rust code sees a panic, it tries to construct a C.RustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{
				inner: status.errorBuf,
			})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		return fmt.Errorf("unknown status code: %d", status.code)
	}
}

func rustCall[U any](callback func(*C.RustCallStatus) U) U {
	returnValue, err := rustCallWithError[error](nil, callback)
	if err != nil {
		panic(err)
	}
	return returnValue
}

type NativeError interface {
	AsError() error
}

func writeInt8(writer io.Writer, value int8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint8(writer io.Writer, value uint8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt16(writer io.Writer, value int16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint16(writer io.Writer, value uint16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt32(writer io.Writer, value int32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint32(writer io.Writer, value uint32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt64(writer io.Writer, value int64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint64(writer io.Writer, value uint64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat32(writer io.Writer, value float32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat64(writer io.Writer, value float64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func readInt8(reader io.Reader) int8 {
	var result int8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint8(reader io.Reader) uint8 {
	var result uint8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt16(reader io.Reader) int16 {
	var result int16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint16(reader io.Reader) uint16 {
	var result uint16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt32(reader io.Reader) int32 {
	var result int32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint32(reader io.Reader) uint32 {
	var result uint32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt64(reader io.Reader) int64 {
	var result int64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint64(reader io.Reader) uint64 {
	var result uint64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat32(reader io.Reader) float32 {
	var result float32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat64(reader io.Reader) float64 {
	var result float64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func init() {

	FfiConverterStreamStreamHandlerINSTANCE.register()
	FfiConverterStreamUnaryHandlerINSTANCE.register()
	FfiConverterUnaryStreamHandlerINSTANCE.register()
	FfiConverterUnaryUnaryHandlerINSTANCE.register()
	uniffiCheckChecksums()
}

func uniffiCheckChecksums() {
	// Get the bindings contract version from our ComponentInterface
	bindingsContractVersion := 30
	// Get the scaffolding contract version by calling the into the dylib
	scaffoldingContractVersion := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.ffi_slim_rpc_uniffi_contract_version()
	})
	if bindingsContractVersion != int(scaffoldingContractVersion) {
		// If this happens try cleaning and rebuilding your project
		panic("slim_rpc: UniFFI contract version mismatch")
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_multicast_stream_stream()
		})
		if checksum != 20862 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_multicast_stream_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_multicast_stream_unary()
		})
		if checksum != 3931 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_multicast_stream_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_multicast_unary()
		})
		if checksum != 45399 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_multicast_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_multicast_unary_async()
		})
		if checksum != 41182 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_multicast_unary_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_multicast_unary_stream()
		})
		if checksum != 1791 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_multicast_unary_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_multicast_unary_stream_async()
		})
		if checksum != 51748 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_multicast_unary_stream_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_stream_stream()
		})
		if checksum != 17894 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_stream_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_stream_unary()
		})
		if checksum != 53775 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_stream_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_unary()
		})
		if checksum != 38109 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_unary_async()
		})
		if checksum != 4778 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_unary_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_unary_stream()
		})
		if checksum != 57715 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_unary_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_call_unary_stream_async()
		})
		if checksum != 60204 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_call_unary_stream_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_close_async()
		})
		if checksum != 50290 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_close_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_channel_close_blocking()
		})
		if checksum != 14890 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_channel_close_blocking: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_context_deadline()
		})
		if checksum != 24900 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_context_deadline: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_context_is_deadline_exceeded()
		})
		if checksum != 28770 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_context_is_deadline_exceeded: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_context_metadata()
		})
		if checksum != 27768 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_context_metadata: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_context_remaining_time()
		})
		if checksum != 37807 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_context_remaining_time: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_context_session_id()
		})
		if checksum != 20837 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_context_session_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_streamstreamhandler_handle()
		})
		if checksum != 2118 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_streamstreamhandler_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_streamunaryhandler_handle()
		})
		if checksum != 24345 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_streamunaryhandler_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_unarystreamhandler_handle()
		})
		if checksum != 50805 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_unarystreamhandler_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_unaryunaryhandler_handle()
		})
		if checksum != 28269 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_unaryunaryhandler_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_server_register_stream_stream()
		})
		if checksum != 8915 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_server_register_stream_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_server_register_stream_unary()
		})
		if checksum != 1327 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_server_register_stream_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_server_register_unary_stream()
		})
		if checksum != 36089 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_server_register_unary_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_server_register_unary_unary()
		})
		if checksum != 4683 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_server_register_unary_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_server_serve_async()
		})
		if checksum != 31185 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_server_serve_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_server_serve_blocking()
		})
		if checksum != 43892 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_server_serve_blocking: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_server_shutdown_async()
		})
		if checksum != 43873 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_server_shutdown_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_server_shutdown_blocking()
		})
		if checksum != 33785 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_server_shutdown_blocking: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_bidistreamhandler_close_send()
		})
		if checksum != 8554 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_bidistreamhandler_close_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_bidistreamhandler_close_send_async()
		})
		if checksum != 61385 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_bidistreamhandler_close_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_bidistreamhandler_recv()
		})
		if checksum != 22600 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_bidistreamhandler_recv: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_bidistreamhandler_recv_async()
		})
		if checksum != 32615 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_bidistreamhandler_recv_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_bidistreamhandler_send()
		})
		if checksum != 6430 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_bidistreamhandler_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_bidistreamhandler_send_async()
		})
		if checksum != 15973 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_bidistreamhandler_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_close_send()
		})
		if checksum != 43173 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_close_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_close_send_async()
		})
		if checksum != 23110 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_close_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_recv()
		})
		if checksum != 48140 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_recv: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_recv_async()
		})
		if checksum != 30100 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_recv_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_send()
		})
		if checksum != 9203 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_send_async()
		})
		if checksum != 44011 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_multicastbidistreamhandler_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_multicastresponsereader_next()
		})
		if checksum != 61701 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_multicastresponsereader_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_multicastresponsereader_next_async()
		})
		if checksum != 39972 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_multicastresponsereader_next_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_requeststream_next()
		})
		if checksum != 20595 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_requeststream_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_requeststream_next_async()
		})
		if checksum != 54349 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_requeststream_next_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_requeststreamwriter_finalize_async()
		})
		if checksum != 32632 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_requeststreamwriter_finalize_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_requeststreamwriter_finalize_stream()
		})
		if checksum != 54088 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_requeststreamwriter_finalize_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_requeststreamwriter_finalize_stream_async()
		})
		if checksum != 17032 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_requeststreamwriter_finalize_stream_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_requeststreamwriter_send()
		})
		if checksum != 12390 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_requeststreamwriter_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_requeststreamwriter_send_async()
		})
		if checksum != 8265 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_requeststreamwriter_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsesink_close()
		})
		if checksum != 38345 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsesink_close: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsesink_close_async()
		})
		if checksum != 40886 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsesink_close_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsesink_is_closed()
		})
		if checksum != 38814 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsesink_is_closed: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsesink_is_closed_async()
		})
		if checksum != 10047 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsesink_is_closed_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsesink_send()
		})
		if checksum != 20957 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsesink_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsesink_send_async()
		})
		if checksum != 42417 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsesink_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsesink_send_error()
		})
		if checksum != 19981 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsesink_send_error: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsesink_send_error_async()
		})
		if checksum != 13763 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsesink_send_error_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsestreamreader_next()
		})
		if checksum != 36597 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsestreamreader_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_method_responsestreamreader_next_async()
		})
		if checksum != 61931 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_method_responsestreamreader_next_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_constructor_channel_new()
		})
		if checksum != 61597 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_constructor_channel_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_constructor_channel_new_group()
		})
		if checksum != 40792 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_constructor_channel_new_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_constructor_channel_new_group_with_connection()
		})
		if checksum != 4099 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_constructor_channel_new_group_with_connection: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_constructor_channel_new_with_connection()
		})
		if checksum != 29890 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_constructor_channel_new_with_connection: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_constructor_server_new()
		})
		if checksum != 34121 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_constructor_server_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_rpc_checksum_constructor_server_new_with_connection()
		})
		if checksum != 20181 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_rpc: uniffi_slim_rpc_checksum_constructor_server_new_with_connection: UniFFI API checksum mismatch")
		}
	}
}

type FfiConverterUint64 struct{}

var FfiConverterUint64INSTANCE = FfiConverterUint64{}

func (FfiConverterUint64) Lower(value uint64) C.uint64_t {
	return C.uint64_t(value)
}

func (FfiConverterUint64) Write(writer io.Writer, value uint64) {
	writeUint64(writer, value)
}

func (FfiConverterUint64) Lift(value C.uint64_t) uint64 {
	return uint64(value)
}

func (FfiConverterUint64) Read(reader io.Reader) uint64 {
	return readUint64(reader)
}

type FfiDestroyerUint64 struct{}

func (FfiDestroyerUint64) Destroy(_ uint64) {}

type FfiConverterBool struct{}

var FfiConverterBoolINSTANCE = FfiConverterBool{}

func (FfiConverterBool) Lower(value bool) C.int8_t {
	if value {
		return C.int8_t(1)
	}
	return C.int8_t(0)
}

func (FfiConverterBool) Write(writer io.Writer, value bool) {
	if value {
		writeInt8(writer, 1)
	} else {
		writeInt8(writer, 0)
	}
}

func (FfiConverterBool) Lift(value C.int8_t) bool {
	return value != 0
}

func (FfiConverterBool) Read(reader io.Reader) bool {
	return readInt8(reader) != 0
}

type FfiDestroyerBool struct{}

func (FfiDestroyerBool) Destroy(_ bool) {}

type FfiConverterString struct{}

var FfiConverterStringINSTANCE = FfiConverterString{}

func (FfiConverterString) Lift(rb RustBufferI) string {
	defer rb.Free()
	reader := rb.AsReader()
	b, err := io.ReadAll(reader)
	if err != nil {
		panic(fmt.Errorf("reading reader: %w", err))
	}
	return string(b)
}

func (FfiConverterString) Read(reader io.Reader) string {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading string, expected %d, read %d", length, read_length))
	}
	return string(buffer)
}

func (FfiConverterString) Lower(value string) C.RustBuffer {
	return stringToRustBuffer(value)
}

func (c FfiConverterString) LowerExternal(value string) ExternalCRustBuffer {
	return RustBufferFromC(stringToRustBuffer(value))
}

func (FfiConverterString) Write(writer io.Writer, value string) {
	if len(value) > math.MaxInt32 {
		panic("String is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := io.WriteString(writer, value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing string, expected %d, written %d", len(value), write_length))
	}
}

type FfiDestroyerString struct{}

func (FfiDestroyerString) Destroy(_ string) {}

type FfiConverterBytes struct{}

var FfiConverterBytesINSTANCE = FfiConverterBytes{}

func (c FfiConverterBytes) Lower(value []byte) C.RustBuffer {
	return LowerIntoRustBuffer[[]byte](c, value)
}

func (c FfiConverterBytes) LowerExternal(value []byte) ExternalCRustBuffer {
	return RustBufferFromC(c.Lower(value))
}

func (c FfiConverterBytes) Write(writer io.Writer, value []byte) {
	if len(value) > math.MaxInt32 {
		panic("[]byte is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := writer.Write(value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing []byte, expected %d, written %d", len(value), write_length))
	}
}

func (c FfiConverterBytes) Lift(rb RustBufferI) []byte {
	return LiftFromRustBuffer[[]byte](c, rb)
}

func (c FfiConverterBytes) Read(reader io.Reader) []byte {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading []byte, expected %d, read %d", length, read_length))
	}
	return buffer
}

type FfiDestroyerBytes struct{}

func (FfiDestroyerBytes) Destroy(_ []byte) {}

type FfiConverterTimestamp struct{}

var FfiConverterTimestampINSTANCE = FfiConverterTimestamp{}

func (c FfiConverterTimestamp) Lift(rb RustBufferI) time.Time {
	return LiftFromRustBuffer[time.Time](c, rb)
}

func (c FfiConverterTimestamp) Read(reader io.Reader) time.Time {
	sec := readInt64(reader)
	nsec := readUint32(reader)

	var sign int64 = 1
	if sec < 0 {
		sign = -1
	}

	return time.Unix(sec, int64(nsec)*sign)
}

func (c FfiConverterTimestamp) Lower(value time.Time) C.RustBuffer {
	return LowerIntoRustBuffer[time.Time](c, value)
}

func (c FfiConverterTimestamp) LowerExternal(value time.Time) ExternalCRustBuffer {
	return RustBufferFromC(c.Lower(value))
}

func (c FfiConverterTimestamp) Write(writer io.Writer, value time.Time) {
	sec := value.Unix()
	nsec := uint32(value.Nanosecond())
	if value.Unix() < 0 {
		nsec = 1_000_000_000 - nsec
		sec += 1
	}

	writeInt64(writer, sec)
	writeUint32(writer, nsec)
}

type FfiDestroyerTimestamp struct{}

func (FfiDestroyerTimestamp) Destroy(_ time.Time) {}

// FfiConverterDuration converts between uniffi duration and Go duration.
type FfiConverterDuration struct{}

var FfiConverterDurationINSTANCE = FfiConverterDuration{}

func (c FfiConverterDuration) Lift(rb RustBufferI) time.Duration {
	return LiftFromRustBuffer[time.Duration](c, rb)
}

func (c FfiConverterDuration) Read(reader io.Reader) time.Duration {
	sec := readUint64(reader)
	nsec := readUint32(reader)
	return time.Duration(sec*1_000_000_000 + uint64(nsec))
}

func (c FfiConverterDuration) Lower(value time.Duration) C.RustBuffer {
	return LowerIntoRustBuffer[time.Duration](c, value)
}

func (c FfiConverterDuration) LowerExternal(value time.Duration) ExternalCRustBuffer {
	return RustBufferFromC(c.Lower(value))
}

func (c FfiConverterDuration) Write(writer io.Writer, value time.Duration) {
	if value.Nanoseconds() < 0 {
		// Rust does not support negative durations:
		// https://www.reddit.com/r/rust/comments/ljl55u/why_rusts_duration_not_supporting_negative_values/
		// This panic is very bad, because it depends on user input, and in Go user input related
		// error are supposed to be returned as errors, and not cause panics. However, with the
		// current architecture, its not possible to return an error from here, so panic is used as
		// the only other option to signal an error.
		panic("negative duration is not allowed")
	}

	writeUint64(writer, uint64(value)/1_000_000_000)
	writeUint32(writer, uint32(uint64(value)%1_000_000_000))
}

type FfiDestroyerDuration struct{}

func (FfiDestroyerDuration) Destroy(_ time.Duration) {}

// Below is an implementation of synchronization requirements outlined in the link.
// https://github.com/mozilla/uniffi-rs/blob/0dc031132d9493ca812c3af6e7dd60ad2ea95bf0/uniffi_bindgen/src/bindings/kotlin/templates/ObjectRuntime.kt#L31

type FfiObject struct {
	handle        C.uint64_t
	callCounter   atomic.Int64
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t
	freeFunction  func(C.uint64_t, *C.RustCallStatus)
	destroyed     atomic.Bool
}

func newFfiObject(
	handle C.uint64_t,
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t,
	freeFunction func(C.uint64_t, *C.RustCallStatus),
) FfiObject {
	return FfiObject{
		handle:        handle,
		cloneFunction: cloneFunction,
		freeFunction:  freeFunction,
	}
}

func (ffiObject *FfiObject) incrementPointer(debugName string) C.uint64_t {
	for {
		counter := ffiObject.callCounter.Load()
		if counter <= -1 {
			panic(fmt.Errorf("%v object has already been destroyed", debugName))
		}
		if counter == math.MaxInt64 {
			panic(fmt.Errorf("%v object call counter would overflow", debugName))
		}
		if ffiObject.callCounter.CompareAndSwap(counter, counter+1) {
			break
		}
	}

	return rustCall(func(status *C.RustCallStatus) C.uint64_t {
		return ffiObject.cloneFunction(ffiObject.handle, status)
	})
}

func (ffiObject *FfiObject) decrementPointer() {
	if ffiObject.callCounter.Add(-1) == -1 {
		ffiObject.freeRustArcPtr()
	}
}

func (ffiObject *FfiObject) destroy() {
	if ffiObject.destroyed.CompareAndSwap(false, true) {
		if ffiObject.callCounter.Add(-1) == -1 {
			ffiObject.freeRustArcPtr()
		}
	}
}

func (ffiObject *FfiObject) freeRustArcPtr() {
	if ffiObject.handle == 0 {
		return
	}
	rustCall(func(status *C.RustCallStatus) int32 {
		ffiObject.freeFunction(ffiObject.handle, status)
		return 0
	})
}

// Bidirectional stream handler for stream-to-stream RPC calls
//
// Allows sending and receiving messages concurrently.
type BidiStreamHandlerInterface interface {
	// Close the request stream (no more messages will be sent)
	CloseSend() error
	// Close the request stream (async version)
	CloseSendAsync() error
	// Receive the next response message (blocking version)
	Recv() StreamMessage
	// Receive the next response message (async version)
	RecvAsync() StreamMessage
	// Send a request message to the stream (blocking version)
	Send(data []byte) error
	// Send a request message to the stream (async version)
	SendAsync(data []byte) error
}

// Bidirectional stream handler for stream-to-stream RPC calls
//
// Allows sending and receiving messages concurrently.
type BidiStreamHandler struct {
	ffiObject FfiObject
}

// Close the request stream (no more messages will be sent)
func (_self *BidiStreamHandler) CloseSend() error {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_bidistreamhandler_close_send(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Close the request stream (async version)
func (_self *BidiStreamHandler) CloseSendAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_bidistreamhandler_close_send_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Receive the next response message (blocking version)
func (_self *BidiStreamHandler) Recv() StreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStreamMessageINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_bidistreamhandler_recv(
				_pointer, _uniffiStatus),
		}
	}))
}

// Receive the next response message (async version)
func (_self *BidiStreamHandler) RecvAsync() StreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	res, _ := uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) StreamMessage {
			return FfiConverterStreamMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_bidistreamhandler_recv_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	return res
}

// Send a request message to the stream (blocking version)
func (_self *BidiStreamHandler) Send(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_bidistreamhandler_send(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send a request message to the stream (async version)
func (_self *BidiStreamHandler) SendAsync(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_bidistreamhandler_send_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *BidiStreamHandler) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterBidiStreamHandler struct{}

var FfiConverterBidiStreamHandlerINSTANCE = FfiConverterBidiStreamHandler{}

func (c FfiConverterBidiStreamHandler) Lift(handle C.uint64_t) *BidiStreamHandler {
	result := &BidiStreamHandler{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_bidistreamhandler(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_bidistreamhandler(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*BidiStreamHandler).Destroy)
	return result
}

func (c FfiConverterBidiStreamHandler) Read(reader io.Reader) *BidiStreamHandler {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterBidiStreamHandler) Lower(value *BidiStreamHandler) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*BidiStreamHandler")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterBidiStreamHandler) Write(writer io.Writer, value *BidiStreamHandler) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalBidiStreamHandler(handle uint64) *BidiStreamHandler {
	return FfiConverterBidiStreamHandlerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalBidiStreamHandler(value *BidiStreamHandler) uint64 {
	return uint64(FfiConverterBidiStreamHandlerINSTANCE.Lower(value))
}

type FfiDestroyerBidiStreamHandler struct{}

func (_ FfiDestroyerBidiStreamHandler) Destroy(value *BidiStreamHandler) {
	value.Destroy()
}

// Client-side channel for making RPC calls.
//
// Manages a single persistent SLIM session: either a `PointToPoint` session
// to a single remote server, or a `Multicast` (GROUP) session shared across
// multiple remote servers. The session is lazily initialised and recreated
// when dead.
//
// ## Constructor
//
// - `new_with_members_internal(app, members)` — Smart constructor:
// - **1 member**: creates a P2P channel.
// - **Many members**: creates a GROUP channel with a generated session name
// and auto-invites all members on the first multicast call.
type ChannelInterface interface {
	// Broadcast a request stream to all GROUP members and stream their
	// responses.
	//
	// Semantically equivalent to `call_multicast_stream_unary` at the
	// transport level; the difference (one vs many responses per member) is
	// determined by the server handler.
	CallMulticastStreamStream(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *MulticastBidiStreamHandler
	// Broadcast a request stream to all GROUP members and collect their
	// responses.
	//
	// Returns a handler that lets you send requests and receive responses
	// concurrently. Use `send` / `send_async` to push request messages,
	// `close_send` / `close_send_async` to signal end-of-requests, and
	// `recv` / `recv_async` to pull response items.
	CallMulticastStreamUnary(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *MulticastBidiStreamHandler
	// Broadcast one request to all GROUP members and collect their responses.
	//
	// Returns a reader from which each member's response (wrapped in
	// `MulticastStreamMessage`) can be pulled one at a time (blocking).
	CallMulticastUnary(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*MulticastResponseReader, error)
	// Broadcast one request to all GROUP members and collect their responses
	// (async).
	CallMulticastUnaryAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*MulticastResponseReader, error)
	// Broadcast one request to all GROUP members and stream their responses
	// (blocking).
	//
	// Semantically identical to `call_multicast_unary` at the transport level;
	// the difference is that each member may send multiple responses before its
	// EOS, which the server handler determines.
	CallMulticastUnaryStream(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*MulticastResponseReader, error)
	// Broadcast one request to all GROUP members and stream their responses
	// (async).
	CallMulticastUnaryStreamAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*MulticastResponseReader, error)
	CallStreamStream(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *BidiStreamHandler
	CallStreamUnary(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *RequestStreamWriter
	CallUnary(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) ([]byte, error)
	CallUnaryAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) ([]byte, error)
	CallUnaryStream(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*ResponseStreamReader, error)
	CallUnaryStreamAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*ResponseStreamReader, error)
	// Async FFI wrapper for [`Channel::close`].
	CloseAsync(timeout *time.Duration) error
	// Blocking FFI wrapper for [`Channel::close`].
	CloseBlocking(timeout *time.Duration) error
}

// Client-side channel for making RPC calls.
//
// Manages a single persistent SLIM session: either a `PointToPoint` session
// to a single remote server, or a `Multicast` (GROUP) session shared across
// multiple remote servers. The session is lazily initialised and recreated
// when dead.
//
// ## Constructor
//
// - `new_with_members_internal(app, members)` — Smart constructor:
// - **1 member**: creates a P2P channel.
// - **Many members**: creates a GROUP channel with a generated session name
// and auto-invites all members on the first multicast call.
type Channel struct {
	ffiObject FfiObject
}

func NewChannel(app *slim_bindings.App, remote *slim_bindings.Name) *Channel {
	return FfiConverterChannelINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_constructor_channel_new(func(value *slim_bindings.App) C.uint64_t { return C.uint64_t(slim_bindings.LowerToExternalApp(value)) }(app), func(value *slim_bindings.Name) C.uint64_t {
			return C.uint64_t(slim_bindings.LowerToExternalName(value))
		}(remote), _uniffiStatus)
	}))
}

func ChannelNewGroup(app *slim_bindings.App, members []*slim_bindings.Name) (*Channel, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_constructor_channel_new_group(func(value *slim_bindings.App) C.uint64_t { return C.uint64_t(slim_bindings.LowerToExternalApp(value)) }(app), FfiConverterSequenceNameINSTANCE.Lower(members), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Channel
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterChannelINSTANCE.Lift(_uniffiRV), nil
	}
}

func ChannelNewGroupWithConnection(app *slim_bindings.App, members []*slim_bindings.Name, connectionId *uint64) (*Channel, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_constructor_channel_new_group_with_connection(func(value *slim_bindings.App) C.uint64_t { return C.uint64_t(slim_bindings.LowerToExternalApp(value)) }(app), FfiConverterSequenceNameINSTANCE.Lower(members), FfiConverterOptionalUint64INSTANCE.Lower(connectionId), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Channel
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterChannelINSTANCE.Lift(_uniffiRV), nil
	}
}

func ChannelNewWithConnection(app *slim_bindings.App, remote *slim_bindings.Name, connectionId *uint64) *Channel {
	return FfiConverterChannelINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_constructor_channel_new_with_connection(func(value *slim_bindings.App) C.uint64_t { return C.uint64_t(slim_bindings.LowerToExternalApp(value)) }(app), func(value *slim_bindings.Name) C.uint64_t {
			return C.uint64_t(slim_bindings.LowerToExternalName(value))
		}(remote), FfiConverterOptionalUint64INSTANCE.Lower(connectionId), _uniffiStatus)
	}))
}

// Broadcast a request stream to all GROUP members and stream their
// responses.
//
// Semantically equivalent to `call_multicast_stream_unary` at the
// transport level; the difference (one vs many responses per member) is
// determined by the server handler.
func (_self *Channel) CallMulticastStreamStream(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *MulticastBidiStreamHandler {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMulticastBidiStreamHandlerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_method_channel_call_multicast_stream_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	}))
}

// Broadcast a request stream to all GROUP members and collect their
// responses.
//
// Returns a handler that lets you send requests and receive responses
// concurrently. Use `send` / `send_async` to push request messages,
// `close_send` / `close_send_async` to signal end-of-requests, and
// `recv` / `recv_async` to pull response items.
func (_self *Channel) CallMulticastStreamUnary(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *MulticastBidiStreamHandler {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMulticastBidiStreamHandlerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_method_channel_call_multicast_stream_unary(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	}))
}

// Broadcast one request to all GROUP members and collect their responses.
//
// Returns a reader from which each member's response (wrapped in
// `MulticastStreamMessage`) can be pulled one at a time (blocking).
func (_self *Channel) CallMulticastUnary(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*MulticastResponseReader, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_method_channel_call_multicast_unary(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MulticastResponseReader
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMulticastResponseReaderINSTANCE.Lift(_uniffiRV), nil
	}
}

// Broadcast one request to all GROUP members and collect their responses
// (async).
func (_self *Channel) CallMulticastUnaryAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*MulticastResponseReader, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_slim_rpc_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MulticastResponseReader {
			return FfiConverterMulticastResponseReaderINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_channel_call_multicast_unary_async(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Broadcast one request to all GROUP members and stream their responses
// (blocking).
//
// Semantically identical to `call_multicast_unary` at the transport level;
// the difference is that each member may send multiple responses before its
// EOS, which the server handler determines.
func (_self *Channel) CallMulticastUnaryStream(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*MulticastResponseReader, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_method_channel_call_multicast_unary_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MulticastResponseReader
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMulticastResponseReaderINSTANCE.Lift(_uniffiRV), nil
	}
}

// Broadcast one request to all GROUP members and stream their responses
// (async).
func (_self *Channel) CallMulticastUnaryStreamAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*MulticastResponseReader, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_slim_rpc_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MulticastResponseReader {
			return FfiConverterMulticastResponseReaderINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_channel_call_multicast_unary_stream_async(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *Channel) CallStreamStream(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *BidiStreamHandler {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBidiStreamHandlerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_method_channel_call_stream_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	}))
}

func (_self *Channel) CallStreamUnary(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *RequestStreamWriter {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterRequestStreamWriterINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_method_channel_call_stream_unary(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	}))
}

func (_self *Channel) CallUnary(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_channel_call_unary(
				_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []byte
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterBytesINSTANCE.Lift(_uniffiRV), nil
	}
}

func (_self *Channel) CallUnaryAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_channel_call_unary_async(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *Channel) CallUnaryStream(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*ResponseStreamReader, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_method_channel_call_unary_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *ResponseStreamReader
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterResponseStreamReaderINSTANCE.Lift(_uniffiRV), nil
	}
}

func (_self *Channel) CallUnaryStreamAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*ResponseStreamReader, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_slim_rpc_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *ResponseStreamReader {
			return FfiConverterResponseStreamReaderINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_channel_call_unary_stream_async(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Async FFI wrapper for [`Channel::close`].
func (_self *Channel) CloseAsync(timeout *time.Duration) error {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_channel_close_async(
			_pointer, FfiConverterOptionalDurationINSTANCE.Lower(timeout)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Blocking FFI wrapper for [`Channel::close`].
func (_self *Channel) CloseBlocking(timeout *time.Duration) error {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_channel_close_blocking(
			_pointer, FfiConverterOptionalDurationINSTANCE.Lower(timeout), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *Channel) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterChannel struct{}

var FfiConverterChannelINSTANCE = FfiConverterChannel{}

func (c FfiConverterChannel) Lift(handle C.uint64_t) *Channel {
	result := &Channel{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_channel(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_channel(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Channel).Destroy)
	return result
}

func (c FfiConverterChannel) Read(reader io.Reader) *Channel {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterChannel) Lower(value *Channel) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*Channel")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterChannel) Write(writer io.Writer, value *Channel) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalChannel(handle uint64) *Channel {
	return FfiConverterChannelINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalChannel(value *Channel) uint64 {
	return uint64(FfiConverterChannelINSTANCE.Lower(value))
}

type FfiDestroyerChannel struct{}

func (_ FfiDestroyerChannel) Destroy(value *Channel) {
	value.Destroy()
}

// Context passed to RPC handlers
//
// Contains all contextual information about an RPC call including:
// - Session information (source, destination, session ID)
// - Metadata (key-value pairs)
// - Deadline/timeout information
// - Message routing details
type ContextInterface interface {
	// Get the deadline for this RPC call
	Deadline() time.Time
	// Check if the deadline has been exceeded
	IsDeadlineExceeded() bool
	// Get the rpc session metadata
	Metadata() map[string]string
	// Get the remaining time until deadline
	//
	// Returns Duration::ZERO if the deadline has already passed
	RemainingTime() time.Duration
	// Get the session ID
	SessionId() string
}

// Context passed to RPC handlers
//
// Contains all contextual information about an RPC call including:
// - Session information (source, destination, session ID)
// - Metadata (key-value pairs)
// - Deadline/timeout information
// - Message routing details
type Context struct {
	ffiObject FfiObject
}

// Get the deadline for this RPC call
func (_self *Context) Deadline() time.Time {
	_pointer := _self.ffiObject.incrementPointer("*Context")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterTimestampINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_context_deadline(
				_pointer, _uniffiStatus),
		}
	}))
}

// Check if the deadline has been exceeded
func (_self *Context) IsDeadlineExceeded() bool {
	_pointer := _self.ffiObject.incrementPointer("*Context")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_slim_rpc_fn_method_context_is_deadline_exceeded(
			_pointer, _uniffiStatus)
	}))
}

// Get the rpc session metadata
func (_self *Context) Metadata() map[string]string {
	_pointer := _self.ffiObject.incrementPointer("*Context")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMapStringStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_context_metadata(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the remaining time until deadline
//
// Returns Duration::ZERO if the deadline has already passed
func (_self *Context) RemainingTime() time.Duration {
	_pointer := _self.ffiObject.incrementPointer("*Context")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterDurationINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_context_remaining_time(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the session ID
func (_self *Context) SessionId() string {
	_pointer := _self.ffiObject.incrementPointer("*Context")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_context_session_id(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *Context) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterContext struct{}

var FfiConverterContextINSTANCE = FfiConverterContext{}

func (c FfiConverterContext) Lift(handle C.uint64_t) *Context {
	result := &Context{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_context(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_context(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Context).Destroy)
	return result
}

func (c FfiConverterContext) Read(reader io.Reader) *Context {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterContext) Lower(value *Context) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*Context")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterContext) Write(writer io.Writer, value *Context) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalContext(handle uint64) *Context {
	return FfiConverterContextINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalContext(value *Context) uint64 {
	return uint64(FfiConverterContextINSTANCE.Lower(value))
}

type FfiDestroyerContext struct{}

func (_ FfiDestroyerContext) Destroy(value *Context) {
	value.Destroy()
}

// Bidirectional stream handler for multicast stream-to-unary and
// stream-to-stream RPC calls.
//
// Send request messages via [`send`](Self::send) / [`send_async`](Self::send_async),
// close the request stream via [`close_send`](Self::close_send), and receive
// responses via [`recv`](Self::recv) / [`recv_async`](Self::recv_async). Each
// response item carries the source member's identity.
type MulticastBidiStreamHandlerInterface interface {
	// Close the request stream — signals that no more messages will be sent
	// (blocking).
	CloseSend() error
	// Close the request stream (async).
	CloseSendAsync() error
	// Receive the next response item (blocking).
	Recv() MulticastStreamMessage
	// Receive the next response item (async).
	RecvAsync() MulticastStreamMessage
	// Send a request message to the stream (blocking).
	Send(data []byte) error
	// Send a request message to the stream (async).
	SendAsync(data []byte) error
}

// Bidirectional stream handler for multicast stream-to-unary and
// stream-to-stream RPC calls.
//
// Send request messages via [`send`](Self::send) / [`send_async`](Self::send_async),
// close the request stream via [`close_send`](Self::close_send), and receive
// responses via [`recv`](Self::recv) / [`recv_async`](Self::recv_async). Each
// response item carries the source member's identity.
type MulticastBidiStreamHandler struct {
	ffiObject FfiObject
}

// Close the request stream — signals that no more messages will be sent
// (blocking).
func (_self *MulticastBidiStreamHandler) CloseSend() error {
	_pointer := _self.ffiObject.incrementPointer("*MulticastBidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_multicastbidistreamhandler_close_send(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Close the request stream (async).
func (_self *MulticastBidiStreamHandler) CloseSendAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*MulticastBidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_multicastbidistreamhandler_close_send_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Receive the next response item (blocking).
func (_self *MulticastBidiStreamHandler) Recv() MulticastStreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*MulticastBidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMulticastStreamMessageINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_multicastbidistreamhandler_recv(
				_pointer, _uniffiStatus),
		}
	}))
}

// Receive the next response item (async).
func (_self *MulticastBidiStreamHandler) RecvAsync() MulticastStreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*MulticastBidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	res, _ := uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MulticastStreamMessage {
			return FfiConverterMulticastStreamMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_multicastbidistreamhandler_recv_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	return res
}

// Send a request message to the stream (blocking).
func (_self *MulticastBidiStreamHandler) Send(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MulticastBidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_multicastbidistreamhandler_send(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send a request message to the stream (async).
func (_self *MulticastBidiStreamHandler) SendAsync(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MulticastBidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_multicastbidistreamhandler_send_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *MulticastBidiStreamHandler) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMulticastBidiStreamHandler struct{}

var FfiConverterMulticastBidiStreamHandlerINSTANCE = FfiConverterMulticastBidiStreamHandler{}

func (c FfiConverterMulticastBidiStreamHandler) Lift(handle C.uint64_t) *MulticastBidiStreamHandler {
	result := &MulticastBidiStreamHandler{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_multicastbidistreamhandler(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_multicastbidistreamhandler(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MulticastBidiStreamHandler).Destroy)
	return result
}

func (c FfiConverterMulticastBidiStreamHandler) Read(reader io.Reader) *MulticastBidiStreamHandler {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMulticastBidiStreamHandler) Lower(value *MulticastBidiStreamHandler) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MulticastBidiStreamHandler")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMulticastBidiStreamHandler) Write(writer io.Writer, value *MulticastBidiStreamHandler) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMulticastBidiStreamHandler(handle uint64) *MulticastBidiStreamHandler {
	return FfiConverterMulticastBidiStreamHandlerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMulticastBidiStreamHandler(value *MulticastBidiStreamHandler) uint64 {
	return uint64(FfiConverterMulticastBidiStreamHandlerINSTANCE.Lower(value))
}

type FfiDestroyerMulticastBidiStreamHandler struct{}

func (_ FfiDestroyerMulticastBidiStreamHandler) Destroy(value *MulticastBidiStreamHandler) {
	value.Destroy()
}

// Response stream reader for multicast RPC calls.
//
// Allows pulling `RpcMulticastItem`s from a GROUP response stream one at a
// time. Each item carries the source member's identity alongside the payload.
type MulticastResponseReaderInterface interface {
	// Pull the next item from the multicast response stream (blocking).
	Next() MulticastStreamMessage
	// Pull the next item from the multicast response stream (async).
	NextAsync() MulticastStreamMessage
}

// Response stream reader for multicast RPC calls.
//
// Allows pulling `RpcMulticastItem`s from a GROUP response stream one at a
// time. Each item carries the source member's identity alongside the payload.
type MulticastResponseReader struct {
	ffiObject FfiObject
}

// Pull the next item from the multicast response stream (blocking).
func (_self *MulticastResponseReader) Next() MulticastStreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*MulticastResponseReader")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMulticastStreamMessageINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_multicastresponsereader_next(
				_pointer, _uniffiStatus),
		}
	}))
}

// Pull the next item from the multicast response stream (async).
func (_self *MulticastResponseReader) NextAsync() MulticastStreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*MulticastResponseReader")
	defer _self.ffiObject.decrementPointer()
	res, _ := uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MulticastStreamMessage {
			return FfiConverterMulticastStreamMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_multicastresponsereader_next_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	return res
}
func (object *MulticastResponseReader) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMulticastResponseReader struct{}

var FfiConverterMulticastResponseReaderINSTANCE = FfiConverterMulticastResponseReader{}

func (c FfiConverterMulticastResponseReader) Lift(handle C.uint64_t) *MulticastResponseReader {
	result := &MulticastResponseReader{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_multicastresponsereader(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_multicastresponsereader(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MulticastResponseReader).Destroy)
	return result
}

func (c FfiConverterMulticastResponseReader) Read(reader io.Reader) *MulticastResponseReader {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMulticastResponseReader) Lower(value *MulticastResponseReader) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MulticastResponseReader")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMulticastResponseReader) Write(writer io.Writer, value *MulticastResponseReader) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMulticastResponseReader(handle uint64) *MulticastResponseReader {
	return FfiConverterMulticastResponseReaderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMulticastResponseReader(value *MulticastResponseReader) uint64 {
	return uint64(FfiConverterMulticastResponseReaderINSTANCE.Lower(value))
}

type FfiDestroyerMulticastResponseReader struct{}

func (_ FfiDestroyerMulticastResponseReader) Destroy(value *MulticastResponseReader) {
	value.Destroy()
}

// Request stream reader
//
// Allows pulling messages from a client request stream.
// This wraps the underlying async stream and provides a blocking interface
// suitable for UniFFI callback traits.
type RequestStreamInterface interface {
	// Pull the next message from the stream (blocking version)
	//
	// Returns a StreamMessage indicating the result
	Next() StreamMessage
	// Pull the next message from the stream (async version)
	//
	// Returns a StreamMessage indicating the result
	NextAsync() StreamMessage
}

// Request stream reader
//
// Allows pulling messages from a client request stream.
// This wraps the underlying async stream and provides a blocking interface
// suitable for UniFFI callback traits.
type RequestStream struct {
	ffiObject FfiObject
}

// Pull the next message from the stream (blocking version)
//
// Returns a StreamMessage indicating the result
func (_self *RequestStream) Next() StreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*RequestStream")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStreamMessageINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_requeststream_next(
				_pointer, _uniffiStatus),
		}
	}))
}

// Pull the next message from the stream (async version)
//
// Returns a StreamMessage indicating the result
func (_self *RequestStream) NextAsync() StreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*RequestStream")
	defer _self.ffiObject.decrementPointer()
	res, _ := uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) StreamMessage {
			return FfiConverterStreamMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_requeststream_next_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	return res
}
func (object *RequestStream) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterRequestStream struct{}

var FfiConverterRequestStreamINSTANCE = FfiConverterRequestStream{}

func (c FfiConverterRequestStream) Lift(handle C.uint64_t) *RequestStream {
	result := &RequestStream{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_requeststream(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_requeststream(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*RequestStream).Destroy)
	return result
}

func (c FfiConverterRequestStream) Read(reader io.Reader) *RequestStream {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterRequestStream) Lower(value *RequestStream) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*RequestStream")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterRequestStream) Write(writer io.Writer, value *RequestStream) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalRequestStream(handle uint64) *RequestStream {
	return FfiConverterRequestStreamINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalRequestStream(value *RequestStream) uint64 {
	return uint64(FfiConverterRequestStreamINSTANCE.Lower(value))
}

type FfiDestroyerRequestStream struct{}

func (_ FfiDestroyerRequestStream) Destroy(value *RequestStream) {
	value.Destroy()
}

// Request stream writer for stream-to-unary RPC calls
//
// Allows sending multiple request messages and getting a final response.
type RequestStreamWriterInterface interface {
	// Finalize the stream and get the response (async version)
	//
	// **Deprecated**: Use [`finalize_stream_async`](Self::finalize_stream_async) instead.
	FinalizeAsync() ([]byte, error)
	// Finalize the stream and get the response (blocking version)
	FinalizeStream() ([]byte, error)
	// Finalize the stream and get the response (async version)
	FinalizeStreamAsync() ([]byte, error)
	// Send a request message to the stream (blocking version)
	Send(data []byte) error
	// Send a request message to the stream (async version)
	SendAsync(data []byte) error
}

// Request stream writer for stream-to-unary RPC calls
//
// Allows sending multiple request messages and getting a final response.
type RequestStreamWriter struct {
	ffiObject FfiObject
}

// Finalize the stream and get the response (async version)
//
// **Deprecated**: Use [`finalize_stream_async`](Self::finalize_stream_async) instead.
func (_self *RequestStreamWriter) FinalizeAsync() ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*RequestStreamWriter")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_requeststreamwriter_finalize_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Finalize the stream and get the response (blocking version)
func (_self *RequestStreamWriter) FinalizeStream() ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*RequestStreamWriter")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_requeststreamwriter_finalize_stream(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []byte
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterBytesINSTANCE.Lift(_uniffiRV), nil
	}
}

// Finalize the stream and get the response (async version)
func (_self *RequestStreamWriter) FinalizeStreamAsync() ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*RequestStreamWriter")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_requeststreamwriter_finalize_stream_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Send a request message to the stream (blocking version)
func (_self *RequestStreamWriter) Send(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*RequestStreamWriter")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_requeststreamwriter_send(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send a request message to the stream (async version)
func (_self *RequestStreamWriter) SendAsync(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*RequestStreamWriter")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_requeststreamwriter_send_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *RequestStreamWriter) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterRequestStreamWriter struct{}

var FfiConverterRequestStreamWriterINSTANCE = FfiConverterRequestStreamWriter{}

func (c FfiConverterRequestStreamWriter) Lift(handle C.uint64_t) *RequestStreamWriter {
	result := &RequestStreamWriter{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_requeststreamwriter(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_requeststreamwriter(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*RequestStreamWriter).Destroy)
	return result
}

func (c FfiConverterRequestStreamWriter) Read(reader io.Reader) *RequestStreamWriter {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterRequestStreamWriter) Lower(value *RequestStreamWriter) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*RequestStreamWriter")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterRequestStreamWriter) Write(writer io.Writer, value *RequestStreamWriter) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalRequestStreamWriter(handle uint64) *RequestStreamWriter {
	return FfiConverterRequestStreamWriterINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalRequestStreamWriter(value *RequestStreamWriter) uint64 {
	return uint64(FfiConverterRequestStreamWriterINSTANCE.Lower(value))
}

type FfiDestroyerRequestStreamWriter struct{}

func (_ FfiDestroyerRequestStreamWriter) Destroy(value *RequestStreamWriter) {
	value.Destroy()
}

// Response stream writer
//
// Allows pushing messages to a client response stream.
// This wraps an async channel sender and provides a blocking interface
// suitable for UniFFI callback traits.
type ResponseSinkInterface interface {
	// Close the response stream (blocking version)
	//
	// Signals that no more messages will be sent.
	// The stream will end gracefully.
	Close() error
	// Close the response stream (async version)
	//
	// Signals that no more messages will be sent.
	// The stream will end gracefully.
	CloseAsync() error
	// Check if the sink has been closed (blocking version)
	IsClosed() bool
	// Check if the sink has been closed (async version)
	IsClosedAsync() bool
	// Send a message to the response stream (blocking version)
	//
	// Returns an error if the stream has been closed or if sending fails.
	Send(data []byte) error
	// Send a message to the response stream (async version)
	//
	// Returns an error if the stream has been closed or if sending fails.
	SendAsync(data []byte) error
	// Send an error to the response stream and close it (blocking version)
	//
	// This terminates the stream with an error status.
	SendError(error *RpcError) error
	// Send an error to the response stream and close it (async version)
	//
	// This terminates the stream with an error status.
	SendErrorAsync(error *RpcError) error
}

// Response stream writer
//
// Allows pushing messages to a client response stream.
// This wraps an async channel sender and provides a blocking interface
// suitable for UniFFI callback traits.
type ResponseSink struct {
	ffiObject FfiObject
}

// Close the response stream (blocking version)
//
// Signals that no more messages will be sent.
// The stream will end gracefully.
func (_self *ResponseSink) Close() error {
	_pointer := _self.ffiObject.incrementPointer("*ResponseSink")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_responsesink_close(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Close the response stream (async version)
//
// Signals that no more messages will be sent.
// The stream will end gracefully.
func (_self *ResponseSink) CloseAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*ResponseSink")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_responsesink_close_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Check if the sink has been closed (blocking version)
func (_self *ResponseSink) IsClosed() bool {
	_pointer := _self.ffiObject.incrementPointer("*ResponseSink")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_slim_rpc_fn_method_responsesink_is_closed(
			_pointer, _uniffiStatus)
	}))
}

// Check if the sink has been closed (async version)
func (_self *ResponseSink) IsClosedAsync() bool {
	_pointer := _self.ffiObject.incrementPointer("*ResponseSink")
	defer _self.ffiObject.decrementPointer()
	res, _ := uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.int8_t {
			res := C.ffi_slim_rpc_rust_future_complete_i8(handle, status)
			return res
		},
		// liftFn
		func(ffi C.int8_t) bool {
			return FfiConverterBoolINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_responsesink_is_closed_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_i8(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_i8(handle)
		},
	)

	return res
}

// Send a message to the response stream (blocking version)
//
// Returns an error if the stream has been closed or if sending fails.
func (_self *ResponseSink) Send(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*ResponseSink")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_responsesink_send(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send a message to the response stream (async version)
//
// Returns an error if the stream has been closed or if sending fails.
func (_self *ResponseSink) SendAsync(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*ResponseSink")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_responsesink_send_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Send an error to the response stream and close it (blocking version)
//
// This terminates the stream with an error status.
func (_self *ResponseSink) SendError(error *RpcError) error {
	_pointer := _self.ffiObject.incrementPointer("*ResponseSink")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_responsesink_send_error(
			_pointer, FfiConverterRpcErrorINSTANCE.Lower(error), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send an error to the response stream and close it (async version)
//
// This terminates the stream with an error status.
func (_self *ResponseSink) SendErrorAsync(error *RpcError) error {
	_pointer := _self.ffiObject.incrementPointer("*ResponseSink")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_responsesink_send_error_async(
			_pointer, FfiConverterRpcErrorINSTANCE.Lower(error)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *ResponseSink) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterResponseSink struct{}

var FfiConverterResponseSinkINSTANCE = FfiConverterResponseSink{}

func (c FfiConverterResponseSink) Lift(handle C.uint64_t) *ResponseSink {
	result := &ResponseSink{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_responsesink(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_responsesink(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*ResponseSink).Destroy)
	return result
}

func (c FfiConverterResponseSink) Read(reader io.Reader) *ResponseSink {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterResponseSink) Lower(value *ResponseSink) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*ResponseSink")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterResponseSink) Write(writer io.Writer, value *ResponseSink) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalResponseSink(handle uint64) *ResponseSink {
	return FfiConverterResponseSinkINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalResponseSink(value *ResponseSink) uint64 {
	return uint64(FfiConverterResponseSinkINSTANCE.Lower(value))
}

type FfiDestroyerResponseSink struct{}

func (_ FfiDestroyerResponseSink) Destroy(value *ResponseSink) {
	value.Destroy()
}

// Response stream reader for unary-to-stream RPC calls
//
// Allows pulling messages from a server response stream one at a time.
type ResponseStreamReaderInterface interface {
	// Pull the next message from the response stream (blocking version)
	//
	// Returns a StreamMessage indicating the result
	Next() StreamMessage
	// Pull the next message from the response stream (async version)
	//
	// Returns a StreamMessage indicating the result
	NextAsync() StreamMessage
}

// Response stream reader for unary-to-stream RPC calls
//
// Allows pulling messages from a server response stream one at a time.
type ResponseStreamReader struct {
	ffiObject FfiObject
}

// Pull the next message from the response stream (blocking version)
//
// Returns a StreamMessage indicating the result
func (_self *ResponseStreamReader) Next() StreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*ResponseStreamReader")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStreamMessageINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_rpc_fn_method_responsestreamreader_next(
				_pointer, _uniffiStatus),
		}
	}))
}

// Pull the next message from the response stream (async version)
//
// Returns a StreamMessage indicating the result
func (_self *ResponseStreamReader) NextAsync() StreamMessage {
	_pointer := _self.ffiObject.incrementPointer("*ResponseStreamReader")
	defer _self.ffiObject.decrementPointer()
	res, _ := uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) StreamMessage {
			return FfiConverterStreamMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_responsestreamreader_next_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	return res
}
func (object *ResponseStreamReader) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterResponseStreamReader struct{}

var FfiConverterResponseStreamReaderINSTANCE = FfiConverterResponseStreamReader{}

func (c FfiConverterResponseStreamReader) Lift(handle C.uint64_t) *ResponseStreamReader {
	result := &ResponseStreamReader{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_responsestreamreader(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_responsestreamreader(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*ResponseStreamReader).Destroy)
	return result
}

func (c FfiConverterResponseStreamReader) Read(reader io.Reader) *ResponseStreamReader {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterResponseStreamReader) Lower(value *ResponseStreamReader) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*ResponseStreamReader")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterResponseStreamReader) Write(writer io.Writer, value *ResponseStreamReader) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalResponseStreamReader(handle uint64) *ResponseStreamReader {
	return FfiConverterResponseStreamReaderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalResponseStreamReader(value *ResponseStreamReader) uint64 {
	return uint64(FfiConverterResponseStreamReaderINSTANCE.Lower(value))
}

type FfiDestroyerResponseStreamReader struct{}

func (_ FfiDestroyerResponseStreamReader) Destroy(value *ResponseStreamReader) {
	value.Destroy()
}

// RPC Server
//
// Handles incoming RPC requests by creating sessions and dispatching
// to registered service handlers.
//
// # Example
//
// ```ignore
// # use slim_bindings::{Server, Context, RpcError, Decoder, Encoder, App, Name};
// # use std::sync::Arc;
// # fn main() -> Result<(), Box<dyn std::error::Error>> {
// # use slim_bindings::{IdentityProviderConfig, IdentityVerifierConfig};
// # let app_name = Arc::new(Name::new("test".to_string(), "app".to_string(), "v1".to_string()));
// # let provider = IdentityProviderConfig::SharedSecret { id: "test".to_string(), data: "secret".to_string() };
// # let verifier = IdentityVerifierConfig::SharedSecret { id: "test".to_string(), data: "secret".to_string() };
// # let app = App::new(app_name, provider, verifier)?;
// # let core_app = app.inner();
// # let notification_rx = app.notification_receiver();
// # #[derive(Default)]
// # struct Request {}
// # impl Decoder for Request {
// #     fn decode(_buf: impl Into<Vec<u8>>) -> Result<Self, RpcError> { Ok(Request::default()) }
// # }
// # #[derive(Default)]
// # struct Response {}
// # impl Encoder for Response {
// #     fn encode(self) -> Result<Vec<u8>, RpcError> { Ok(vec![]) }
// # }
// let base_name = Name::new("org".to_string(), "namespace".to_string(), "service".to_string());
// let server = Server::new_with_shared_rx_and_connection(core_app, base_name.as_slim_name(), None, notification_rx, None);
//
// // Register handlers
// server.register_unary_unary_internal(
// "MyService",
// "MyMethod",
// |request: Request, _ctx: Context| async move {
// Ok(Response::default())
// }
// );
// # Ok(())
// # }
// ```
type ServerInterface interface {
	RegisterStreamStream(serviceName string, methodName string, handler StreamStreamHandler)
	RegisterStreamUnary(serviceName string, methodName string, handler StreamUnaryHandler)
	RegisterUnaryStream(serviceName string, methodName string, handler UnaryStreamHandler)
	RegisterUnaryUnary(serviceName string, methodName string, handler UnaryUnaryHandler)
	// Async FFI wrapper for [`Server::serve`].
	ServeAsync() error
	// Blocking FFI wrapper for [`Server::serve`].
	ServeBlocking() error
	// Async FFI wrapper for [`Server::shutdown`].
	ShutdownAsync()
	// Blocking FFI wrapper for [`Server::shutdown`].
	ShutdownBlocking()
}

// RPC Server
//
// Handles incoming RPC requests by creating sessions and dispatching
// to registered service handlers.
//
// # Example
//
// ```ignore
// # use slim_bindings::{Server, Context, RpcError, Decoder, Encoder, App, Name};
// # use std::sync::Arc;
// # fn main() -> Result<(), Box<dyn std::error::Error>> {
// # use slim_bindings::{IdentityProviderConfig, IdentityVerifierConfig};
// # let app_name = Arc::new(Name::new("test".to_string(), "app".to_string(), "v1".to_string()));
// # let provider = IdentityProviderConfig::SharedSecret { id: "test".to_string(), data: "secret".to_string() };
// # let verifier = IdentityVerifierConfig::SharedSecret { id: "test".to_string(), data: "secret".to_string() };
// # let app = App::new(app_name, provider, verifier)?;
// # let core_app = app.inner();
// # let notification_rx = app.notification_receiver();
// # #[derive(Default)]
// # struct Request {}
// # impl Decoder for Request {
// #     fn decode(_buf: impl Into<Vec<u8>>) -> Result<Self, RpcError> { Ok(Request::default()) }
// # }
// # #[derive(Default)]
// # struct Response {}
// # impl Encoder for Response {
// #     fn encode(self) -> Result<Vec<u8>, RpcError> { Ok(vec![]) }
// # }
// let base_name = Name::new("org".to_string(), "namespace".to_string(), "service".to_string());
// let server = Server::new_with_shared_rx_and_connection(core_app, base_name.as_slim_name(), None, notification_rx, None);
//
// // Register handlers
// server.register_unary_unary_internal(
// "MyService",
// "MyMethod",
// |request: Request, _ctx: Context| async move {
// Ok(Response::default())
// }
// );
// # Ok(())
// # }
// ```
type Server struct {
	ffiObject FfiObject
}

// Create a new RPC server
//
// This is the primary constructor for creating an RPC server instance
// that can handle incoming RPC requests over SLIM.
//
// # Arguments
// * `app` - The SLIM application instance that provides the underlying
// network transport and session management
// * `base_name` - The base name for this service (e.g., org.namespace.service).
// This name is used to construct subscription names for RPC methods.
//
// # Returns
// A new RPC server instance wrapped in an Arc for shared ownership
func NewServer(app *slim_bindings.App, baseName *slim_bindings.Name) *Server {
	return FfiConverterServerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_constructor_server_new(func(value *slim_bindings.App) C.uint64_t { return C.uint64_t(slim_bindings.LowerToExternalApp(value)) }(app), func(value *slim_bindings.Name) C.uint64_t {
			return C.uint64_t(slim_bindings.LowerToExternalName(value))
		}(baseName), _uniffiStatus)
	}))
}

// Create a new RPC server with optional connection ID
//
// The connection ID is used to set up routing before serving RPC requests,
// enabling multi-hop RPC calls through specific connections.
//
// # Arguments
// * `app` - The SLIM application instance that provides the underlying
// network transport and session management
// * `base_name` - The base name for this service (e.g., org.namespace.service).
// This name is used to construct subscription names for RPC methods.
// * `connection_id` - Optional connection ID for routing setup
//
// # Returns
// A new RPC server instance wrapped in an Arc for shared ownership
func ServerNewWithConnection(app *slim_bindings.App, baseName *slim_bindings.Name, connectionId *uint64) *Server {
	return FfiConverterServerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_rpc_fn_constructor_server_new_with_connection(func(value *slim_bindings.App) C.uint64_t { return C.uint64_t(slim_bindings.LowerToExternalApp(value)) }(app), func(value *slim_bindings.Name) C.uint64_t {
			return C.uint64_t(slim_bindings.LowerToExternalName(value))
		}(baseName), FfiConverterOptionalUint64INSTANCE.Lower(connectionId), _uniffiStatus)
	}))
}

func (_self *Server) RegisterStreamStream(serviceName string, methodName string, handler StreamStreamHandler) {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_server_register_stream_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterStreamStreamHandlerINSTANCE.Lower(handler), _uniffiStatus)
		return false
	})
}

func (_self *Server) RegisterStreamUnary(serviceName string, methodName string, handler StreamUnaryHandler) {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_server_register_stream_unary(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterStreamUnaryHandlerINSTANCE.Lower(handler), _uniffiStatus)
		return false
	})
}

func (_self *Server) RegisterUnaryStream(serviceName string, methodName string, handler UnaryStreamHandler) {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_server_register_unary_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterUnaryStreamHandlerINSTANCE.Lower(handler), _uniffiStatus)
		return false
	})
}

func (_self *Server) RegisterUnaryUnary(serviceName string, methodName string, handler UnaryUnaryHandler) {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_server_register_unary_unary(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterUnaryUnaryHandlerINSTANCE.Lower(handler), _uniffiStatus)
		return false
	})
}

// Async FFI wrapper for [`Server::serve`].
func (_self *Server) ServeAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_server_serve_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Blocking FFI wrapper for [`Server::serve`].
func (_self *Server) ServeBlocking() error {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_server_serve_blocking(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Async FFI wrapper for [`Server::shutdown`].
func (_self *Server) ShutdownAsync() {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_server_shutdown_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

}

// Blocking FFI wrapper for [`Server::shutdown`].
func (_self *Server) ShutdownBlocking() {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_rpc_fn_method_server_shutdown_blocking(
			_pointer, _uniffiStatus)
		return false
	})
}
func (object *Server) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterServer struct{}

var FfiConverterServerINSTANCE = FfiConverterServer{}

func (c FfiConverterServer) Lift(handle C.uint64_t) *Server {
	result := &Server{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_slim_rpc_fn_clone_server(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_slim_rpc_fn_free_server(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Server).Destroy)
	return result
}

func (c FfiConverterServer) Read(reader io.Reader) *Server {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterServer) Lower(value *Server) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*Server")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterServer) Write(writer io.Writer, value *Server) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalServer(handle uint64) *Server {
	return FfiConverterServerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalServer(value *Server) uint64 {
	return uint64(FfiConverterServerINSTANCE.Lower(value))
}

type FfiDestroyerServer struct{}

func (_ FfiDestroyerServer) Destroy(value *Server) {
	value.Destroy()
}

// Stream-to-Stream RPC handler trait
//
// Implement this trait to handle stream-to-stream RPC calls.
// The handler receives multiple requests via the stream and sends multiple responses via the sink.
type StreamStreamHandler interface {
	// Handle a stream-to-stream RPC call
	//
	// # Arguments
	// * `stream` - Request stream to pull messages from
	// * `context` - RPC context with metadata and session information
	// * `sink` - Response sink to send streaming responses
	//
	// # Returns
	// Ok(()) if handling succeeded, or an error
	//
	// # Note
	// You must call `sink.close()` or `sink.send_error()` when done.
	Handle(stream *RequestStream, context *Context, sink *ResponseSink) error
}

// Stream-to-Stream RPC handler trait
//
// Implement this trait to handle stream-to-stream RPC calls.
// The handler receives multiple requests via the stream and sends multiple responses via the sink.
type StreamStreamHandlerImpl struct {
	ffiObject FfiObject
}

// Handle a stream-to-stream RPC call
//
// # Arguments
// * `stream` - Request stream to pull messages from
// * `context` - RPC context with metadata and session information
// * `sink` - Response sink to send streaming responses
//
// # Returns
// Ok(()) if handling succeeded, or an error
//
// # Note
// You must call `sink.close()` or `sink.send_error()` when done.
func (_self *StreamStreamHandlerImpl) Handle(stream *RequestStream, context *Context, sink *ResponseSink) error {
	_pointer := _self.ffiObject.incrementPointer("StreamStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_streamstreamhandler_handle(
			_pointer, FfiConverterRequestStreamINSTANCE.Lower(stream), FfiConverterContextINSTANCE.Lower(context), FfiConverterResponseSinkINSTANCE.Lower(sink)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *StreamStreamHandlerImpl) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterStreamStreamHandler struct {
	handleMap *concurrentHandleMap[StreamStreamHandler]
}

var FfiConverterStreamStreamHandlerINSTANCE = FfiConverterStreamStreamHandler{
	handleMap: newConcurrentHandleMap[StreamStreamHandler](),
}

func (c FfiConverterStreamStreamHandler) Lift(handle C.uint64_t) StreamStreamHandler {
	if uint64(handle)&1 == 0 {
		// Rust-generated handle (even), construct a new object wrapping the handle
		result := &StreamStreamHandlerImpl{
			newFfiObject(
				handle,
				func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
					return C.uniffi_slim_rpc_fn_clone_streamstreamhandler(handle, status)
				},
				func(handle C.uint64_t, status *C.RustCallStatus) {
					C.uniffi_slim_rpc_fn_free_streamstreamhandler(handle, status)
				},
			),
		}
		runtime.SetFinalizer(result, (*StreamStreamHandlerImpl).Destroy)
		return result
	} else {
		// Go-generated handle (odd), retrieve from the handle map
		val, ok := c.handleMap.tryGet(uint64(handle))
		if !ok {
			panic(fmt.Errorf("no callback in handle map: %d", handle))
		}
		c.handleMap.remove(uint64(handle))
		return val
	}
}

func (c FfiConverterStreamStreamHandler) Read(reader io.Reader) StreamStreamHandler {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterStreamStreamHandler) Lower(value StreamStreamHandler) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	if val, ok := value.(*StreamStreamHandlerImpl); ok {
		// Rust-backed object, clone the handle
		handle := val.ffiObject.incrementPointer("StreamStreamHandler")
		defer val.ffiObject.decrementPointer()
		return handle
	} else {
		// Go-backed object, insert into handle map
		return C.uint64_t(c.handleMap.insert(value))
	}
}

func (c FfiConverterStreamStreamHandler) Write(writer io.Writer, value StreamStreamHandler) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalStreamStreamHandler(handle uint64) StreamStreamHandler {
	return FfiConverterStreamStreamHandlerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalStreamStreamHandler(value StreamStreamHandler) uint64 {
	return uint64(FfiConverterStreamStreamHandlerINSTANCE.Lower(value))
}

type FfiDestroyerStreamStreamHandler struct{}

func (_ FfiDestroyerStreamStreamHandler) Destroy(value StreamStreamHandler) {
	if val, ok := value.(*StreamStreamHandlerImpl); ok {
		val.Destroy()
	}
}

type uniffiCallbackResult C.int8_t

const (
	uniffiIdxCallbackFree               uniffiCallbackResult = 0
	uniffiCallbackResultSuccess         uniffiCallbackResult = 0
	uniffiCallbackResultError           uniffiCallbackResult = 1
	uniffiCallbackUnexpectedResultError uniffiCallbackResult = 2
	uniffiCallbackCancelled             uniffiCallbackResult = 3
)

type concurrentHandleMap[T any] struct {
	handles       map[uint64]T
	currentHandle uint64
	lock          sync.RWMutex
}

func newConcurrentHandleMap[T any]() *concurrentHandleMap[T] {
	return &concurrentHandleMap[T]{
		handles:       map[uint64]T{},
		currentHandle: 1,
	}
}

func (cm *concurrentHandleMap[T]) insert(obj T) uint64 {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	handle := cm.currentHandle
	cm.currentHandle = cm.currentHandle + 2
	cm.handles[handle] = obj
	return handle
}

func (cm *concurrentHandleMap[T]) remove(handle uint64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	delete(cm.handles, handle)
}

func (cm *concurrentHandleMap[T]) tryGet(handle uint64) (T, bool) {
	cm.lock.RLock()
	defer cm.lock.RUnlock()

	val, ok := cm.handles[handle]
	return val, ok
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerMethod0
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerMethod0(uniffiHandle C.uint64_t, stream C.uint64_t, context C.uint64_t, sink C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterStreamStreamHandlerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.slim_rpc_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.Handle(
				FfiConverterRequestStreamINSTANCE.Lift(stream),
				FfiConverterContextINSTANCE.Lift(context),
				FfiConverterResponseSinkINSTANCE.Lift(sink),
			)

		if err != nil {
			var actualError *RpcError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterRpcErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

var UniffiVTableCallbackInterfaceStreamStreamHandlerINSTANCE = C.UniffiVTableCallbackInterfaceStreamStreamHandler{
	uniffiFree:  (C.UniffiCallbackInterfaceFree)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerFree),
	uniffiClone: (C.UniffiCallbackInterfaceClone)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerClone),
	handle:      (C.UniffiCallbackInterfaceStreamStreamHandlerMethod0)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerMethod0),
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerFree
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerFree(handle C.uint64_t) {
	FfiConverterStreamStreamHandlerINSTANCE.handleMap.remove(uint64(handle))
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerClone
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamStreamHandlerClone(handle C.uint64_t) C.uint64_t {
	val, ok := FfiConverterStreamStreamHandlerINSTANCE.handleMap.tryGet(uint64(handle))
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}
	return C.uint64_t(FfiConverterStreamStreamHandlerINSTANCE.handleMap.insert(val))
}

func (c FfiConverterStreamStreamHandler) register() {
	C.uniffi_slim_rpc_fn_init_callback_vtable_streamstreamhandler(&UniffiVTableCallbackInterfaceStreamStreamHandlerINSTANCE)
}

// Stream-to-Unary RPC handler trait
//
// Implement this trait to handle stream-to-unary RPC calls.
// The handler receives multiple requests via the stream and returns a single response.
type StreamUnaryHandler interface {
	// Handle a stream-to-unary RPC call
	//
	// # Arguments
	// * `stream` - Request stream to pull messages from
	// * `context` - RPC context with metadata and session information
	//
	// # Returns
	// The response message bytes or an error
	Handle(stream *RequestStream, context *Context) ([]byte, error)
}

// Stream-to-Unary RPC handler trait
//
// Implement this trait to handle stream-to-unary RPC calls.
// The handler receives multiple requests via the stream and returns a single response.
type StreamUnaryHandlerImpl struct {
	ffiObject FfiObject
}

// Handle a stream-to-unary RPC call
//
// # Arguments
// * `stream` - Request stream to pull messages from
// * `context` - RPC context with metadata and session information
//
// # Returns
// The response message bytes or an error
func (_self *StreamUnaryHandlerImpl) Handle(stream *RequestStream, context *Context) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("StreamUnaryHandler")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_streamunaryhandler_handle(
			_pointer, FfiConverterRequestStreamINSTANCE.Lower(stream), FfiConverterContextINSTANCE.Lower(context)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *StreamUnaryHandlerImpl) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterStreamUnaryHandler struct {
	handleMap *concurrentHandleMap[StreamUnaryHandler]
}

var FfiConverterStreamUnaryHandlerINSTANCE = FfiConverterStreamUnaryHandler{
	handleMap: newConcurrentHandleMap[StreamUnaryHandler](),
}

func (c FfiConverterStreamUnaryHandler) Lift(handle C.uint64_t) StreamUnaryHandler {
	if uint64(handle)&1 == 0 {
		// Rust-generated handle (even), construct a new object wrapping the handle
		result := &StreamUnaryHandlerImpl{
			newFfiObject(
				handle,
				func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
					return C.uniffi_slim_rpc_fn_clone_streamunaryhandler(handle, status)
				},
				func(handle C.uint64_t, status *C.RustCallStatus) {
					C.uniffi_slim_rpc_fn_free_streamunaryhandler(handle, status)
				},
			),
		}
		runtime.SetFinalizer(result, (*StreamUnaryHandlerImpl).Destroy)
		return result
	} else {
		// Go-generated handle (odd), retrieve from the handle map
		val, ok := c.handleMap.tryGet(uint64(handle))
		if !ok {
			panic(fmt.Errorf("no callback in handle map: %d", handle))
		}
		c.handleMap.remove(uint64(handle))
		return val
	}
}

func (c FfiConverterStreamUnaryHandler) Read(reader io.Reader) StreamUnaryHandler {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterStreamUnaryHandler) Lower(value StreamUnaryHandler) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	if val, ok := value.(*StreamUnaryHandlerImpl); ok {
		// Rust-backed object, clone the handle
		handle := val.ffiObject.incrementPointer("StreamUnaryHandler")
		defer val.ffiObject.decrementPointer()
		return handle
	} else {
		// Go-backed object, insert into handle map
		return C.uint64_t(c.handleMap.insert(value))
	}
}

func (c FfiConverterStreamUnaryHandler) Write(writer io.Writer, value StreamUnaryHandler) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalStreamUnaryHandler(handle uint64) StreamUnaryHandler {
	return FfiConverterStreamUnaryHandlerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalStreamUnaryHandler(value StreamUnaryHandler) uint64 {
	return uint64(FfiConverterStreamUnaryHandlerINSTANCE.Lower(value))
}

type FfiDestroyerStreamUnaryHandler struct{}

func (_ FfiDestroyerStreamUnaryHandler) Destroy(value StreamUnaryHandler) {
	if val, ok := value.(*StreamUnaryHandlerImpl); ok {
		val.Destroy()
	}
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerMethod0
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerMethod0(uniffiHandle C.uint64_t, stream C.uint64_t, context C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterStreamUnaryHandlerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.slim_rpc_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.Handle(
				FfiConverterRequestStreamINSTANCE.Lift(stream),
				FfiConverterContextINSTANCE.Lift(context),
			)

		if err != nil {
			var actualError *RpcError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterRpcErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterBytesINSTANCE.Lower(res)
	}()
}

var UniffiVTableCallbackInterfaceStreamUnaryHandlerINSTANCE = C.UniffiVTableCallbackInterfaceStreamUnaryHandler{
	uniffiFree:  (C.UniffiCallbackInterfaceFree)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerFree),
	uniffiClone: (C.UniffiCallbackInterfaceClone)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerClone),
	handle:      (C.UniffiCallbackInterfaceStreamUnaryHandlerMethod0)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerMethod0),
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerFree
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerFree(handle C.uint64_t) {
	FfiConverterStreamUnaryHandlerINSTANCE.handleMap.remove(uint64(handle))
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerClone
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceStreamUnaryHandlerClone(handle C.uint64_t) C.uint64_t {
	val, ok := FfiConverterStreamUnaryHandlerINSTANCE.handleMap.tryGet(uint64(handle))
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}
	return C.uint64_t(FfiConverterStreamUnaryHandlerINSTANCE.handleMap.insert(val))
}

func (c FfiConverterStreamUnaryHandler) register() {
	C.uniffi_slim_rpc_fn_init_callback_vtable_streamunaryhandler(&UniffiVTableCallbackInterfaceStreamUnaryHandlerINSTANCE)
}

// Unary-to-Stream RPC handler trait
//
// Implement this trait to handle unary-to-stream RPC calls.
// The handler receives a single request and sends multiple responses via the sink.
type UnaryStreamHandler interface {
	// Handle a unary-to-stream RPC call
	//
	// # Arguments
	// * `request` - The request message bytes
	// * `context` - RPC context with metadata and session information
	// * `sink` - Response sink to send streaming responses
	//
	// # Returns
	// Ok(()) if handling succeeded, or an error
	//
	// # Note
	// You must call `sink.close()` or `sink.send_error()` when done.
	Handle(request []byte, context *Context, sink *ResponseSink) error
}

// Unary-to-Stream RPC handler trait
//
// Implement this trait to handle unary-to-stream RPC calls.
// The handler receives a single request and sends multiple responses via the sink.
type UnaryStreamHandlerImpl struct {
	ffiObject FfiObject
}

// Handle a unary-to-stream RPC call
//
// # Arguments
// * `request` - The request message bytes
// * `context` - RPC context with metadata and session information
// * `sink` - Response sink to send streaming responses
//
// # Returns
// Ok(()) if handling succeeded, or an error
//
// # Note
// You must call `sink.close()` or `sink.send_error()` when done.
func (_self *UnaryStreamHandlerImpl) Handle(request []byte, context *Context, sink *ResponseSink) error {
	_pointer := _self.ffiObject.incrementPointer("UnaryStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_rpc_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_rpc_fn_method_unarystreamhandler_handle(
			_pointer, FfiConverterBytesINSTANCE.Lower(request), FfiConverterContextINSTANCE.Lower(context), FfiConverterResponseSinkINSTANCE.Lower(sink)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *UnaryStreamHandlerImpl) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterUnaryStreamHandler struct {
	handleMap *concurrentHandleMap[UnaryStreamHandler]
}

var FfiConverterUnaryStreamHandlerINSTANCE = FfiConverterUnaryStreamHandler{
	handleMap: newConcurrentHandleMap[UnaryStreamHandler](),
}

func (c FfiConverterUnaryStreamHandler) Lift(handle C.uint64_t) UnaryStreamHandler {
	if uint64(handle)&1 == 0 {
		// Rust-generated handle (even), construct a new object wrapping the handle
		result := &UnaryStreamHandlerImpl{
			newFfiObject(
				handle,
				func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
					return C.uniffi_slim_rpc_fn_clone_unarystreamhandler(handle, status)
				},
				func(handle C.uint64_t, status *C.RustCallStatus) {
					C.uniffi_slim_rpc_fn_free_unarystreamhandler(handle, status)
				},
			),
		}
		runtime.SetFinalizer(result, (*UnaryStreamHandlerImpl).Destroy)
		return result
	} else {
		// Go-generated handle (odd), retrieve from the handle map
		val, ok := c.handleMap.tryGet(uint64(handle))
		if !ok {
			panic(fmt.Errorf("no callback in handle map: %d", handle))
		}
		c.handleMap.remove(uint64(handle))
		return val
	}
}

func (c FfiConverterUnaryStreamHandler) Read(reader io.Reader) UnaryStreamHandler {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterUnaryStreamHandler) Lower(value UnaryStreamHandler) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	if val, ok := value.(*UnaryStreamHandlerImpl); ok {
		// Rust-backed object, clone the handle
		handle := val.ffiObject.incrementPointer("UnaryStreamHandler")
		defer val.ffiObject.decrementPointer()
		return handle
	} else {
		// Go-backed object, insert into handle map
		return C.uint64_t(c.handleMap.insert(value))
	}
}

func (c FfiConverterUnaryStreamHandler) Write(writer io.Writer, value UnaryStreamHandler) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalUnaryStreamHandler(handle uint64) UnaryStreamHandler {
	return FfiConverterUnaryStreamHandlerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalUnaryStreamHandler(value UnaryStreamHandler) uint64 {
	return uint64(FfiConverterUnaryStreamHandlerINSTANCE.Lower(value))
}

type FfiDestroyerUnaryStreamHandler struct{}

func (_ FfiDestroyerUnaryStreamHandler) Destroy(value UnaryStreamHandler) {
	if val, ok := value.(*UnaryStreamHandlerImpl); ok {
		val.Destroy()
	}
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerMethod0
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerMethod0(uniffiHandle C.uint64_t, request C.RustBuffer, context C.uint64_t, sink C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterUnaryStreamHandlerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.slim_rpc_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.Handle(
				FfiConverterBytesINSTANCE.Lift(GoRustBuffer{
					inner: request,
				}),
				FfiConverterContextINSTANCE.Lift(context),
				FfiConverterResponseSinkINSTANCE.Lift(sink),
			)

		if err != nil {
			var actualError *RpcError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterRpcErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

var UniffiVTableCallbackInterfaceUnaryStreamHandlerINSTANCE = C.UniffiVTableCallbackInterfaceUnaryStreamHandler{
	uniffiFree:  (C.UniffiCallbackInterfaceFree)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerFree),
	uniffiClone: (C.UniffiCallbackInterfaceClone)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerClone),
	handle:      (C.UniffiCallbackInterfaceUnaryStreamHandlerMethod0)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerMethod0),
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerFree
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerFree(handle C.uint64_t) {
	FfiConverterUnaryStreamHandlerINSTANCE.handleMap.remove(uint64(handle))
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerClone
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryStreamHandlerClone(handle C.uint64_t) C.uint64_t {
	val, ok := FfiConverterUnaryStreamHandlerINSTANCE.handleMap.tryGet(uint64(handle))
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}
	return C.uint64_t(FfiConverterUnaryStreamHandlerINSTANCE.handleMap.insert(val))
}

func (c FfiConverterUnaryStreamHandler) register() {
	C.uniffi_slim_rpc_fn_init_callback_vtable_unarystreamhandler(&UniffiVTableCallbackInterfaceUnaryStreamHandlerINSTANCE)
}

// Unary-to-Unary RPC handler trait
//
// Implement this trait to handle unary-to-unary RPC calls.
// The handler receives a single request and returns a single response.
type UnaryUnaryHandler interface {
	// Handle a unary-to-unary RPC call
	//
	// # Arguments
	// * `request` - The request message bytes
	// * `context` - RPC context with metadata and session information
	//
	// # Returns
	// The response message bytes or an error
	Handle(request []byte, context *Context) ([]byte, error)
}

// Unary-to-Unary RPC handler trait
//
// Implement this trait to handle unary-to-unary RPC calls.
// The handler receives a single request and returns a single response.
type UnaryUnaryHandlerImpl struct {
	ffiObject FfiObject
}

// Handle a unary-to-unary RPC call
//
// # Arguments
// * `request` - The request message bytes
// * `context` - RPC context with metadata and session information
//
// # Returns
// The response message bytes or an error
func (_self *UnaryUnaryHandlerImpl) Handle(request []byte, context *Context) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("UnaryUnaryHandler")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_rpc_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_rpc_fn_method_unaryunaryhandler_handle(
			_pointer, FfiConverterBytesINSTANCE.Lower(request), FfiConverterContextINSTANCE.Lower(context)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_rpc_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_rpc_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *UnaryUnaryHandlerImpl) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterUnaryUnaryHandler struct {
	handleMap *concurrentHandleMap[UnaryUnaryHandler]
}

var FfiConverterUnaryUnaryHandlerINSTANCE = FfiConverterUnaryUnaryHandler{
	handleMap: newConcurrentHandleMap[UnaryUnaryHandler](),
}

func (c FfiConverterUnaryUnaryHandler) Lift(handle C.uint64_t) UnaryUnaryHandler {
	if uint64(handle)&1 == 0 {
		// Rust-generated handle (even), construct a new object wrapping the handle
		result := &UnaryUnaryHandlerImpl{
			newFfiObject(
				handle,
				func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
					return C.uniffi_slim_rpc_fn_clone_unaryunaryhandler(handle, status)
				},
				func(handle C.uint64_t, status *C.RustCallStatus) {
					C.uniffi_slim_rpc_fn_free_unaryunaryhandler(handle, status)
				},
			),
		}
		runtime.SetFinalizer(result, (*UnaryUnaryHandlerImpl).Destroy)
		return result
	} else {
		// Go-generated handle (odd), retrieve from the handle map
		val, ok := c.handleMap.tryGet(uint64(handle))
		if !ok {
			panic(fmt.Errorf("no callback in handle map: %d", handle))
		}
		c.handleMap.remove(uint64(handle))
		return val
	}
}

func (c FfiConverterUnaryUnaryHandler) Read(reader io.Reader) UnaryUnaryHandler {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterUnaryUnaryHandler) Lower(value UnaryUnaryHandler) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	if val, ok := value.(*UnaryUnaryHandlerImpl); ok {
		// Rust-backed object, clone the handle
		handle := val.ffiObject.incrementPointer("UnaryUnaryHandler")
		defer val.ffiObject.decrementPointer()
		return handle
	} else {
		// Go-backed object, insert into handle map
		return C.uint64_t(c.handleMap.insert(value))
	}
}

func (c FfiConverterUnaryUnaryHandler) Write(writer io.Writer, value UnaryUnaryHandler) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalUnaryUnaryHandler(handle uint64) UnaryUnaryHandler {
	return FfiConverterUnaryUnaryHandlerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalUnaryUnaryHandler(value UnaryUnaryHandler) uint64 {
	return uint64(FfiConverterUnaryUnaryHandlerINSTANCE.Lower(value))
}

type FfiDestroyerUnaryUnaryHandler struct{}

func (_ FfiDestroyerUnaryUnaryHandler) Destroy(value UnaryUnaryHandler) {
	if val, ok := value.(*UnaryUnaryHandlerImpl); ok {
		val.Destroy()
	}
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerMethod0
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerMethod0(uniffiHandle C.uint64_t, request C.RustBuffer, context C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterUnaryUnaryHandlerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.slim_rpc_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.Handle(
				FfiConverterBytesINSTANCE.Lift(GoRustBuffer{
					inner: request,
				}),
				FfiConverterContextINSTANCE.Lift(context),
			)

		if err != nil {
			var actualError *RpcError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterRpcErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterBytesINSTANCE.Lower(res)
	}()
}

var UniffiVTableCallbackInterfaceUnaryUnaryHandlerINSTANCE = C.UniffiVTableCallbackInterfaceUnaryUnaryHandler{
	uniffiFree:  (C.UniffiCallbackInterfaceFree)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerFree),
	uniffiClone: (C.UniffiCallbackInterfaceClone)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerClone),
	handle:      (C.UniffiCallbackInterfaceUnaryUnaryHandlerMethod0)(C.slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerMethod0),
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerFree
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerFree(handle C.uint64_t) {
	FfiConverterUnaryUnaryHandlerINSTANCE.handleMap.remove(uint64(handle))
}

//export slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerClone
func slim_rpc_handler_traits_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerClone(handle C.uint64_t) C.uint64_t {
	val, ok := FfiConverterUnaryUnaryHandlerINSTANCE.handleMap.tryGet(uint64(handle))
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}
	return C.uint64_t(FfiConverterUnaryUnaryHandlerINSTANCE.handleMap.insert(val))
}

func (c FfiConverterUnaryUnaryHandler) register() {
	C.uniffi_slim_rpc_fn_init_callback_vtable_unaryunaryhandler(&UniffiVTableCallbackInterfaceUnaryUnaryHandlerINSTANCE)
}

// Per-message context for a multicast RPC response — identifies which group
// member sent the response.
type RpcMessageContext struct {
	// The SLIM name of the group member that sent this response.
	Source *slim_bindings.Name
}

func (r *RpcMessageContext) Destroy() {
	slim_bindings.FfiDestroyerName{}.Destroy(r.Source)
}

type FfiConverterRpcMessageContext struct{}

var FfiConverterRpcMessageContextINSTANCE = FfiConverterRpcMessageContext{}

func (c FfiConverterRpcMessageContext) Lift(rb RustBufferI) RpcMessageContext {
	return LiftFromRustBuffer[RpcMessageContext](c, rb)
}

func (c FfiConverterRpcMessageContext) Read(reader io.Reader) RpcMessageContext {
	return RpcMessageContext{
		slim_bindings.FfiConverterNameINSTANCE.Read(reader),
	}
}

func (c FfiConverterRpcMessageContext) Lower(value RpcMessageContext) C.RustBuffer {
	return LowerIntoRustBuffer[RpcMessageContext](c, value)
}

func (c FfiConverterRpcMessageContext) LowerExternal(value RpcMessageContext) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RpcMessageContext](c, value))
}

func (c FfiConverterRpcMessageContext) Write(writer io.Writer, value RpcMessageContext) {
	slim_bindings.FfiConverterNameINSTANCE.Write(writer, value.Source)
}

type FfiDestroyerRpcMessageContext struct{}

func (_ FfiDestroyerRpcMessageContext) Destroy(value RpcMessageContext) {
	value.Destroy()
}

// A single item in a multicast response stream, pairing the response payload
// with the identity of the member that produced it.
type RpcMulticastItem struct {
	// Context identifying the source member.
	Context RpcMessageContext
	// The encoded response payload (raw bytes).
	Message []byte
}

func (r *RpcMulticastItem) Destroy() {
	FfiDestroyerRpcMessageContext{}.Destroy(r.Context)
	FfiDestroyerBytes{}.Destroy(r.Message)
}

type FfiConverterRpcMulticastItem struct{}

var FfiConverterRpcMulticastItemINSTANCE = FfiConverterRpcMulticastItem{}

func (c FfiConverterRpcMulticastItem) Lift(rb RustBufferI) RpcMulticastItem {
	return LiftFromRustBuffer[RpcMulticastItem](c, rb)
}

func (c FfiConverterRpcMulticastItem) Read(reader io.Reader) RpcMulticastItem {
	return RpcMulticastItem{
		FfiConverterRpcMessageContextINSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterRpcMulticastItem) Lower(value RpcMulticastItem) C.RustBuffer {
	return LowerIntoRustBuffer[RpcMulticastItem](c, value)
}

func (c FfiConverterRpcMulticastItem) LowerExternal(value RpcMulticastItem) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RpcMulticastItem](c, value))
}

func (c FfiConverterRpcMulticastItem) Write(writer io.Writer, value RpcMulticastItem) {
	FfiConverterRpcMessageContextINSTANCE.Write(writer, value.Context)
	FfiConverterBytesINSTANCE.Write(writer, value.Message)
}

type FfiDestroyerRpcMulticastItem struct{}

func (_ FfiDestroyerRpcMulticastItem) Destroy(value RpcMulticastItem) {
	value.Destroy()
}

// Message from a multicast response stream.
type MulticastStreamMessage interface {
	Destroy()
}

// Successfully received response item with source context.
type MulticastStreamMessageData struct {
	Item RpcMulticastItem
}

func (e MulticastStreamMessageData) Destroy() {
	FfiDestroyerRpcMulticastItem{}.Destroy(e.Item)
}

// Error from one member — other members may still be active.
type MulticastStreamMessageError struct {
	Error *RpcError
}

func (e MulticastStreamMessageError) Destroy() {
	FfiDestroyerRpcError{}.Destroy(e.Error)
}

// All members have finished — the stream has ended.
type MulticastStreamMessageEnd struct {
}

func (e MulticastStreamMessageEnd) Destroy() {
}

type FfiConverterMulticastStreamMessage struct{}

var FfiConverterMulticastStreamMessageINSTANCE = FfiConverterMulticastStreamMessage{}

func (c FfiConverterMulticastStreamMessage) Lift(rb RustBufferI) MulticastStreamMessage {
	return LiftFromRustBuffer[MulticastStreamMessage](c, rb)
}

func (c FfiConverterMulticastStreamMessage) Lower(value MulticastStreamMessage) C.RustBuffer {
	return LowerIntoRustBuffer[MulticastStreamMessage](c, value)
}

func (c FfiConverterMulticastStreamMessage) LowerExternal(value MulticastStreamMessage) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MulticastStreamMessage](c, value))
}
func (FfiConverterMulticastStreamMessage) Read(reader io.Reader) MulticastStreamMessage {
	id := readInt32(reader)
	switch id {
	case 1:
		return MulticastStreamMessageData{
			FfiConverterRpcMulticastItemINSTANCE.Read(reader),
		}
	case 2:
		return MulticastStreamMessageError{
			FfiConverterRpcErrorINSTANCE.Read(reader),
		}
	case 3:
		return MulticastStreamMessageEnd{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterMulticastStreamMessage.Read()", id))
	}
}

func (FfiConverterMulticastStreamMessage) Write(writer io.Writer, value MulticastStreamMessage) {
	switch variant_value := value.(type) {
	case MulticastStreamMessageData:
		writeInt32(writer, 1)
		FfiConverterRpcMulticastItemINSTANCE.Write(writer, variant_value.Item)
	case MulticastStreamMessageError:
		writeInt32(writer, 2)
		FfiConverterRpcErrorINSTANCE.Write(writer, variant_value.Error)
	case MulticastStreamMessageEnd:
		writeInt32(writer, 3)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterMulticastStreamMessage.Write", value))
	}
}

type FfiDestroyerMulticastStreamMessage struct{}

func (_ FfiDestroyerMulticastStreamMessage) Destroy(value MulticastStreamMessage) {
	value.Destroy()
}

// gRPC status codes
type RpcCode uint16

const (
	// Success
	RpcCodeOk RpcCode = 0
	// The operation was cancelled
	RpcCodeCancelled RpcCode = 1
	// Unknown error
	RpcCodeUnknown RpcCode = 2
	// Client specified an invalid argument
	RpcCodeInvalidArgument RpcCode = 3
	// Deadline exceeded before operation could complete
	RpcCodeDeadlineExceeded RpcCode = 4
	// Some requested entity was not found
	RpcCodeNotFound RpcCode = 5
	// Some entity that we attempted to create already exists
	RpcCodeAlreadyExists RpcCode = 6
	// The caller does not have permission to execute the specified operation
	RpcCodePermissionDenied RpcCode = 7
	// Some resource has been exhausted
	RpcCodeResourceExhausted RpcCode = 8
	// The system is not in a state required for the operation's execution
	RpcCodeFailedPrecondition RpcCode = 9
	// The operation was aborted
	RpcCodeAborted RpcCode = 10
	// Operation was attempted past the valid range
	RpcCodeOutOfRange RpcCode = 11
	// Operation is not implemented or not supported
	RpcCodeUnimplemented RpcCode = 12
	// Internal errors
	RpcCodeInternal RpcCode = 13
	// The service is currently unavailable
	RpcCodeUnavailable RpcCode = 14
	// Unrecoverable data loss or corruption
	RpcCodeDataLoss RpcCode = 15
	// The request does not have valid authentication credentials
	RpcCodeUnauthenticated RpcCode = 16
)

type FfiConverterRpcCode struct{}

var FfiConverterRpcCodeINSTANCE = FfiConverterRpcCode{}

func (c FfiConverterRpcCode) Lift(rb RustBufferI) RpcCode {
	return LiftFromRustBuffer[RpcCode](c, rb)
}

func (c FfiConverterRpcCode) Lower(value RpcCode) C.RustBuffer {
	return LowerIntoRustBuffer[RpcCode](c, value)
}

func (c FfiConverterRpcCode) LowerExternal(value RpcCode) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RpcCode](c, value))
}
func (FfiConverterRpcCode) Read(reader io.Reader) RpcCode {
	id := readInt32(reader)
	return RpcCode(id)
}

func (FfiConverterRpcCode) Write(writer io.Writer, value RpcCode) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerRpcCode struct{}

func (_ FfiDestroyerRpcCode) Destroy(value RpcCode) {
}

// UniFFI-compatible RPC error
//
// This represents RPC errors with gRPC-compatible status codes.
type RpcError struct {
	err error
}

// Convenience method to turn *RpcError into error
// Avoiding treating nil pointer as non nil error interface
func (err *RpcError) AsError() error {
	if err == nil {
		return nil
	} else {
		return err
	}
}

func (err RpcError) Error() string {
	return fmt.Sprintf("RpcError: %s", err.err.Error())
}

func (err RpcError) Unwrap() error {
	return err.err
}

// Err* are used for checking error type with `errors.Is`
var ErrRpcErrorRpc = fmt.Errorf("RpcErrorRpc")
var ErrRpcErrorMulticastRpc = fmt.Errorf("RpcErrorMulticastRpc")
var ErrRpcErrorMulticastSessionClosed = fmt.Errorf("RpcErrorMulticastSessionClosed")

// Variant structs
type RpcErrorRpc struct {
	Code    RpcCode
	Message string
	Details *[]byte
}

func NewRpcErrorRpc(
	code RpcCode,
	message string,
	details *[]byte,
) *RpcError {
	return &RpcError{err: &RpcErrorRpc{
		Code:    code,
		Message: message,
		Details: details}}
}

func (e RpcErrorRpc) destroy() {
	FfiDestroyerRpcCode{}.Destroy(e.Code)
	FfiDestroyerString{}.Destroy(e.Message)
	FfiDestroyerOptionalBytes{}.Destroy(e.Details)
}

func (err RpcErrorRpc) Error() string {
	return fmt.Sprint("Rpc",
		": ",

		"Code=",
		err.Code,
		", ",
		"Message=",
		err.Message,
		", ",
		"Details=",
		err.Details,
	)
}

func (self RpcErrorRpc) Is(target error) bool {
	return target == ErrRpcErrorRpc
}

type RpcErrorMulticastRpc struct {
	Origin  string
	Code    RpcCode
	Message string
	Details *[]byte
}

func NewRpcErrorMulticastRpc(
	origin string,
	code RpcCode,
	message string,
	details *[]byte,
) *RpcError {
	return &RpcError{err: &RpcErrorMulticastRpc{
		Origin:  origin,
		Code:    code,
		Message: message,
		Details: details}}
}

func (e RpcErrorMulticastRpc) destroy() {
	FfiDestroyerString{}.Destroy(e.Origin)
	FfiDestroyerRpcCode{}.Destroy(e.Code)
	FfiDestroyerString{}.Destroy(e.Message)
	FfiDestroyerOptionalBytes{}.Destroy(e.Details)
}

func (err RpcErrorMulticastRpc) Error() string {
	return fmt.Sprint("MulticastRpc",
		": ",

		"Origin=",
		err.Origin,
		", ",
		"Code=",
		err.Code,
		", ",
		"Message=",
		err.Message,
		", ",
		"Details=",
		err.Details,
	)
}

func (self RpcErrorMulticastRpc) Is(target error) bool {
	return target == ErrRpcErrorMulticastRpc
}

type RpcErrorMulticastSessionClosed struct {
	Completed uint64
	Total     uint64
	Received  []string
	Missing   []string
}

func NewRpcErrorMulticastSessionClosed(
	completed uint64,
	total uint64,
	received []string,
	missing []string,
) *RpcError {
	return &RpcError{err: &RpcErrorMulticastSessionClosed{
		Completed: completed,
		Total:     total,
		Received:  received,
		Missing:   missing}}
}

func (e RpcErrorMulticastSessionClosed) destroy() {
	FfiDestroyerUint64{}.Destroy(e.Completed)
	FfiDestroyerUint64{}.Destroy(e.Total)
	FfiDestroyerSequenceString{}.Destroy(e.Received)
	FfiDestroyerSequenceString{}.Destroy(e.Missing)
}

func (err RpcErrorMulticastSessionClosed) Error() string {
	return fmt.Sprint("MulticastSessionClosed",
		": ",

		"Completed=",
		err.Completed,
		", ",
		"Total=",
		err.Total,
		", ",
		"Received=",
		err.Received,
		", ",
		"Missing=",
		err.Missing,
	)
}

func (self RpcErrorMulticastSessionClosed) Is(target error) bool {
	return target == ErrRpcErrorMulticastSessionClosed
}

type FfiConverterRpcError struct{}

var FfiConverterRpcErrorINSTANCE = FfiConverterRpcError{}

func (c FfiConverterRpcError) Lift(eb RustBufferI) *RpcError {
	return LiftFromRustBuffer[*RpcError](c, eb)
}

func (c FfiConverterRpcError) Lower(value *RpcError) C.RustBuffer {
	return LowerIntoRustBuffer[*RpcError](c, value)
}

func (c FfiConverterRpcError) LowerExternal(value *RpcError) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*RpcError](c, value))
}

func (c FfiConverterRpcError) Read(reader io.Reader) *RpcError {
	errorID := readUint32(reader)

	switch errorID {
	case 1:
		return &RpcError{&RpcErrorRpc{
			Code:    FfiConverterRpcCodeINSTANCE.Read(reader),
			Message: FfiConverterStringINSTANCE.Read(reader),
			Details: FfiConverterOptionalBytesINSTANCE.Read(reader),
		}}
	case 2:
		return &RpcError{&RpcErrorMulticastRpc{
			Origin:  FfiConverterStringINSTANCE.Read(reader),
			Code:    FfiConverterRpcCodeINSTANCE.Read(reader),
			Message: FfiConverterStringINSTANCE.Read(reader),
			Details: FfiConverterOptionalBytesINSTANCE.Read(reader),
		}}
	case 3:
		return &RpcError{&RpcErrorMulticastSessionClosed{
			Completed: FfiConverterUint64INSTANCE.Read(reader),
			Total:     FfiConverterUint64INSTANCE.Read(reader),
			Received:  FfiConverterSequenceStringINSTANCE.Read(reader),
			Missing:   FfiConverterSequenceStringINSTANCE.Read(reader),
		}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterRpcError.Read()", errorID))
	}
}

func (c FfiConverterRpcError) Write(writer io.Writer, value *RpcError) {
	switch variantValue := value.err.(type) {
	case *RpcErrorRpc:
		writeInt32(writer, 1)
		FfiConverterRpcCodeINSTANCE.Write(writer, variantValue.Code)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
		FfiConverterOptionalBytesINSTANCE.Write(writer, variantValue.Details)
	case *RpcErrorMulticastRpc:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Origin)
		FfiConverterRpcCodeINSTANCE.Write(writer, variantValue.Code)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
		FfiConverterOptionalBytesINSTANCE.Write(writer, variantValue.Details)
	case *RpcErrorMulticastSessionClosed:
		writeInt32(writer, 3)
		FfiConverterUint64INSTANCE.Write(writer, variantValue.Completed)
		FfiConverterUint64INSTANCE.Write(writer, variantValue.Total)
		FfiConverterSequenceStringINSTANCE.Write(writer, variantValue.Received)
		FfiConverterSequenceStringINSTANCE.Write(writer, variantValue.Missing)
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiConverterRpcError.Write", value))
	}
}

type FfiDestroyerRpcError struct{}

func (_ FfiDestroyerRpcError) Destroy(value *RpcError) {
	switch variantValue := value.err.(type) {
	case RpcErrorRpc:
		variantValue.destroy()
	case RpcErrorMulticastRpc:
		variantValue.destroy()
	case RpcErrorMulticastSessionClosed:
		variantValue.destroy()
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiDestroyerRpcError.Destroy", value))
	}
}

// Message from a stream
type StreamMessage interface {
	Destroy()
}

// Successfully received data
type StreamMessageData struct {
	Field0 []byte
}

func (e StreamMessageData) Destroy() {
	FfiDestroyerBytes{}.Destroy(e.Field0)
}

// Stream error occurred
type StreamMessageError struct {
	Field0 *RpcError
}

func (e StreamMessageError) Destroy() {
	FfiDestroyerRpcError{}.Destroy(e.Field0)
}

// Stream has ended
type StreamMessageEnd struct {
}

func (e StreamMessageEnd) Destroy() {
}

type FfiConverterStreamMessage struct{}

var FfiConverterStreamMessageINSTANCE = FfiConverterStreamMessage{}

func (c FfiConverterStreamMessage) Lift(rb RustBufferI) StreamMessage {
	return LiftFromRustBuffer[StreamMessage](c, rb)
}

func (c FfiConverterStreamMessage) Lower(value StreamMessage) C.RustBuffer {
	return LowerIntoRustBuffer[StreamMessage](c, value)
}

func (c FfiConverterStreamMessage) LowerExternal(value StreamMessage) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[StreamMessage](c, value))
}
func (FfiConverterStreamMessage) Read(reader io.Reader) StreamMessage {
	id := readInt32(reader)
	switch id {
	case 1:
		return StreamMessageData{
			FfiConverterBytesINSTANCE.Read(reader),
		}
	case 2:
		return StreamMessageError{
			FfiConverterRpcErrorINSTANCE.Read(reader),
		}
	case 3:
		return StreamMessageEnd{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterStreamMessage.Read()", id))
	}
}

func (FfiConverterStreamMessage) Write(writer io.Writer, value StreamMessage) {
	switch variant_value := value.(type) {
	case StreamMessageData:
		writeInt32(writer, 1)
		FfiConverterBytesINSTANCE.Write(writer, variant_value.Field0)
	case StreamMessageError:
		writeInt32(writer, 2)
		FfiConverterRpcErrorINSTANCE.Write(writer, variant_value.Field0)
	case StreamMessageEnd:
		writeInt32(writer, 3)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterStreamMessage.Write", value))
	}
}

type FfiDestroyerStreamMessage struct{}

func (_ FfiDestroyerStreamMessage) Destroy(value StreamMessage) {
	value.Destroy()
}

type FfiConverterOptionalUint64 struct{}

var FfiConverterOptionalUint64INSTANCE = FfiConverterOptionalUint64{}

func (c FfiConverterOptionalUint64) Lift(rb RustBufferI) *uint64 {
	return LiftFromRustBuffer[*uint64](c, rb)
}

func (_ FfiConverterOptionalUint64) Read(reader io.Reader) *uint64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint64) Lower(value *uint64) C.RustBuffer {
	return LowerIntoRustBuffer[*uint64](c, value)
}

func (c FfiConverterOptionalUint64) LowerExternal(value *uint64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint64](c, value))
}

func (_ FfiConverterOptionalUint64) Write(writer io.Writer, value *uint64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint64 struct{}

func (_ FfiDestroyerOptionalUint64) Destroy(value *uint64) {
	if value != nil {
		FfiDestroyerUint64{}.Destroy(*value)
	}
}

type FfiConverterOptionalBytes struct{}

var FfiConverterOptionalBytesINSTANCE = FfiConverterOptionalBytes{}

func (c FfiConverterOptionalBytes) Lift(rb RustBufferI) *[]byte {
	return LiftFromRustBuffer[*[]byte](c, rb)
}

func (_ FfiConverterOptionalBytes) Read(reader io.Reader) *[]byte {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBytesINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBytes) Lower(value *[]byte) C.RustBuffer {
	return LowerIntoRustBuffer[*[]byte](c, value)
}

func (c FfiConverterOptionalBytes) LowerExternal(value *[]byte) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]byte](c, value))
}

func (_ FfiConverterOptionalBytes) Write(writer io.Writer, value *[]byte) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBytesINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBytes struct{}

func (_ FfiDestroyerOptionalBytes) Destroy(value *[]byte) {
	if value != nil {
		FfiDestroyerBytes{}.Destroy(*value)
	}
}

type FfiConverterOptionalDuration struct{}

var FfiConverterOptionalDurationINSTANCE = FfiConverterOptionalDuration{}

func (c FfiConverterOptionalDuration) Lift(rb RustBufferI) *time.Duration {
	return LiftFromRustBuffer[*time.Duration](c, rb)
}

func (_ FfiConverterOptionalDuration) Read(reader io.Reader) *time.Duration {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterDurationINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalDuration) Lower(value *time.Duration) C.RustBuffer {
	return LowerIntoRustBuffer[*time.Duration](c, value)
}

func (c FfiConverterOptionalDuration) LowerExternal(value *time.Duration) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*time.Duration](c, value))
}

func (_ FfiConverterOptionalDuration) Write(writer io.Writer, value *time.Duration) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterDurationINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalDuration struct{}

func (_ FfiDestroyerOptionalDuration) Destroy(value *time.Duration) {
	if value != nil {
		FfiDestroyerDuration{}.Destroy(*value)
	}
}

type FfiConverterOptionalMapStringString struct{}

var FfiConverterOptionalMapStringStringINSTANCE = FfiConverterOptionalMapStringString{}

func (c FfiConverterOptionalMapStringString) Lift(rb RustBufferI) *map[string]string {
	return LiftFromRustBuffer[*map[string]string](c, rb)
}

func (_ FfiConverterOptionalMapStringString) Read(reader io.Reader) *map[string]string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMapStringStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMapStringString) Lower(value *map[string]string) C.RustBuffer {
	return LowerIntoRustBuffer[*map[string]string](c, value)
}

func (c FfiConverterOptionalMapStringString) LowerExternal(value *map[string]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*map[string]string](c, value))
}

func (_ FfiConverterOptionalMapStringString) Write(writer io.Writer, value *map[string]string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMapStringStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMapStringString struct{}

func (_ FfiDestroyerOptionalMapStringString) Destroy(value *map[string]string) {
	if value != nil {
		FfiDestroyerMapStringString{}.Destroy(*value)
	}
}

type FfiConverterSequenceString struct{}

var FfiConverterSequenceStringINSTANCE = FfiConverterSequenceString{}

func (c FfiConverterSequenceString) Lift(rb RustBufferI) []string {
	return LiftFromRustBuffer[[]string](c, rb)
}

func (c FfiConverterSequenceString) Read(reader io.Reader) []string {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]string, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterStringINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceString) Lower(value []string) C.RustBuffer {
	return LowerIntoRustBuffer[[]string](c, value)
}

func (c FfiConverterSequenceString) LowerExternal(value []string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]string](c, value))
}

func (c FfiConverterSequenceString) Write(writer io.Writer, value []string) {
	if len(value) > math.MaxInt32 {
		panic("[]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterStringINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceString struct{}

func (FfiDestroyerSequenceString) Destroy(sequence []string) {
	for _, value := range sequence {
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterSequenceName struct{}

var FfiConverterSequenceNameINSTANCE = FfiConverterSequenceName{}

func (c FfiConverterSequenceName) Lift(rb RustBufferI) []*slim_bindings.Name {
	return LiftFromRustBuffer[[]*slim_bindings.Name](c, rb)
}

func (c FfiConverterSequenceName) Read(reader io.Reader) []*slim_bindings.Name {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]*slim_bindings.Name, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, slim_bindings.FfiConverterNameINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceName) Lower(value []*slim_bindings.Name) C.RustBuffer {
	return LowerIntoRustBuffer[[]*slim_bindings.Name](c, value)
}

func (c FfiConverterSequenceName) LowerExternal(value []*slim_bindings.Name) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]*slim_bindings.Name](c, value))
}

func (c FfiConverterSequenceName) Write(writer io.Writer, value []*slim_bindings.Name) {
	if len(value) > math.MaxInt32 {
		panic("[]*slim_bindings.Name is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		slim_bindings.FfiConverterNameINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceName struct{}

func (FfiDestroyerSequenceName) Destroy(sequence []*slim_bindings.Name) {
	for _, value := range sequence {
		slim_bindings.FfiDestroyerName{}.Destroy(value)
	}
}

type FfiConverterMapStringString struct{}

var FfiConverterMapStringStringINSTANCE = FfiConverterMapStringString{}

func (c FfiConverterMapStringString) Lift(rb RustBufferI) map[string]string {
	return LiftFromRustBuffer[map[string]string](c, rb)
}

func (_ FfiConverterMapStringString) Read(reader io.Reader) map[string]string {
	result := make(map[string]string)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterStringINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringString) Lower(value map[string]string) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]string](c, value)
}

func (c FfiConverterMapStringString) LowerExternal(value map[string]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]string](c, value))
}

func (_ FfiConverterMapStringString) Write(writer io.Writer, mapValue map[string]string) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterStringINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringString struct{}

func (_ FfiDestroyerMapStringString) Destroy(mapValue map[string]string) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerString{}.Destroy(value)
	}
}

const (
	uniffiRustFuturePollReady      int8 = 0
	uniffiRustFuturePollMaybeReady int8 = 1
)

type rustFuturePollFunc func(C.uint64_t, C.UniffiRustFutureContinuationCallback, C.uint64_t)
type rustFutureCompleteFunc[T any] func(C.uint64_t, *C.RustCallStatus) T
type rustFutureFreeFunc func(C.uint64_t)

//export slim_rpc_uniffiFutureContinuationCallback
func slim_rpc_uniffiFutureContinuationCallback(data C.uint64_t, pollResult C.int8_t) {
	h := cgo.Handle(uintptr(data))
	waiter := h.Value().(chan int8)
	waiter <- int8(pollResult)
}

func uniffiRustCallAsync[E any, T any, F any](
	errConverter BufReader[E],
	completeFunc rustFutureCompleteFunc[F],
	liftFunc func(F) T,
	rustFuture C.uint64_t,
	pollFunc rustFuturePollFunc,
	freeFunc rustFutureFreeFunc,
) (T, E) {
	defer freeFunc(rustFuture)

	pollResult := int8(-1)
	waiter := make(chan int8, 1)

	chanHandle := cgo.NewHandle(waiter)
	defer chanHandle.Delete()

	for pollResult != uniffiRustFuturePollReady {
		pollFunc(
			rustFuture,
			(C.UniffiRustFutureContinuationCallback)(C.slim_rpc_uniffiFutureContinuationCallback),
			C.uint64_t(chanHandle),
		)
		pollResult = <-waiter
	}

	var goValue T
	ffiValue, err := rustCallWithError(errConverter, func(status *C.RustCallStatus) F {
		return completeFunc(rustFuture, status)
	})
	if value := reflect.ValueOf(err); value.IsValid() && !value.IsZero() {
		return goValue, err
	}
	return liftFunc(ffiValue), err
}

//export slim_rpc_uniffiFreeGorutine
func slim_rpc_uniffiFreeGorutine(data C.uint64_t) {
	handle := cgo.Handle(uintptr(data))
	defer handle.Delete()

	guard := handle.Value().(chan struct{})
	guard <- struct{}{}
}
