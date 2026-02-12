package slim_bindings

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo linux,amd64 LDFLAGS: -L${SRCDIR} -L${SRCDIR}/../../../../../.cgo-cache/slim-bindings -lslim_bindings_x86_64_linux_gnu -lm
#cgo linux,arm64 LDFLAGS: -L${SRCDIR} -L${SRCDIR}/../../../../../.cgo-cache/slim-bindings -lslim_bindings_aarch64_linux_gnu -lm
#cgo darwin,amd64 LDFLAGS: -L${SRCDIR} -L${SRCDIR}/../../../../../.cgo-cache/slim-bindings -lslim_bindings_x86_64_darwin -Wl,-undefined,dynamic_lookup
#cgo darwin,arm64 LDFLAGS: -L${SRCDIR} -L${SRCDIR}/../../../../../.cgo-cache/slim-bindings -lslim_bindings_aarch64_darwin -Wl,-undefined,dynamic_lookup
#cgo windows,amd64 LDFLAGS: -L${SRCDIR} -L${SRCDIR}/../../../../../.cgo-cache/slim-bindings -lslim_bindings_x86_64_windows_gnu -lws2_32 -lbcrypt -ladvapi32 -luserenv -lntdll -lgcc_eh -lgcc -lkernel32 -lole32
#include <slim_bindings.h>
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"runtime/cgo"
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

func RustBufferFromExternal(b RustBufferI) GoRustBuffer {
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
		C.ffi_slim_bindings_rustbuffer_free(cb.inner, status)
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
		return C.ffi_slim_bindings_rustbuffer_from_bytes(foreign, status)
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

func rustCallWithError[E any, U any](converter BufReader[*E], callback func(*C.RustCallStatus) U) (U, *E) {
	var status C.RustCallStatus
	returnValue := callback(&status)
	err := checkCallStatus(converter, status)
	return returnValue, err
}

func checkCallStatus[E any](converter BufReader[*E], status C.RustCallStatus) *E {
	switch status.code {
	case 0:
		return nil
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
	bindingsContractVersion := 26
	// Get the scaffolding contract version by calling the into the dylib
	scaffoldingContractVersion := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.ffi_slim_bindings_uniffi_contract_version()
	})
	if bindingsContractVersion != int(scaffoldingContractVersion) {
		// If this happens try cleaning and rebuilding your project
		panic("slim_bindings: UniFFI contract version mismatch")
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_create_service()
		})
		if checksum != 50798 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_create_service: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_create_service_with_config()
		})
		if checksum != 6614 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_create_service_with_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_get_build_info()
		})
		if checksum != 20767 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_get_build_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_get_global_service()
		})
		if checksum != 63486 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_get_global_service: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_get_services()
		})
		if checksum != 58132 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_get_services: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_get_version()
		})
		if checksum != 28442 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_get_version: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_initialize_from_config()
		})
		if checksum != 7375 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_initialize_from_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_initialize_with_configs()
		})
		if checksum != 4551 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_initialize_with_configs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_initialize_with_defaults()
		})
		if checksum != 58956 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_initialize_with_defaults: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_is_initialized()
		})
		if checksum != 4144 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_is_initialized: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_dataplane_config()
		})
		if checksum != 6114 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_dataplane_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_insecure_client_config()
		})
		if checksum != 42525 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_insecure_client_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_insecure_server_config()
		})
		if checksum != 40258 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_insecure_server_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_runtime_config()
		})
		if checksum != 61090 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_runtime_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_runtime_config_with()
		})
		if checksum != 39801 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_runtime_config_with: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_server_config()
		})
		if checksum != 36482 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_server_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_service_config()
		})
		if checksum != 58037 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_service_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_service_config_with()
		})
		if checksum != 9565 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_service_config_with: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_service_configuration()
		})
		if checksum != 51471 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_service_configuration: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_tracing_config()
		})
		if checksum != 62274 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_tracing_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_new_tracing_config_with()
		})
		if checksum != 52432 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_new_tracing_config_with: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_func_shutdown_blocking()
		})
		if checksum != 6435 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_func_shutdown_blocking: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_create_session()
		})
		if checksum != 43342 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_create_session: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_create_session_and_wait()
		})
		if checksum != 26130 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_create_session_and_wait: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_create_session_and_wait_async()
		})
		if checksum != 11981 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_create_session_and_wait_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_create_session_async()
		})
		if checksum != 12561 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_create_session_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_delete_session()
		})
		if checksum != 35432 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_delete_session: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_delete_session_and_wait()
		})
		if checksum != 49247 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_delete_session_and_wait: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_delete_session_and_wait_async()
		})
		if checksum != 21135 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_delete_session_and_wait_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_delete_session_async()
		})
		if checksum != 57531 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_delete_session_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_id()
		})
		if checksum != 25966 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_listen_for_session()
		})
		if checksum != 8567 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_listen_for_session: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_listen_for_session_async()
		})
		if checksum != 25092 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_listen_for_session_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_name()
		})
		if checksum != 60302 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_remove_route()
		})
		if checksum != 38502 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_remove_route: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_remove_route_async()
		})
		if checksum != 6042 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_remove_route_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_set_route()
		})
		if checksum != 60890 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_set_route: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_set_route_async()
		})
		if checksum != 32403 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_set_route_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_subscribe()
		})
		if checksum != 43519 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_subscribe: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_subscribe_async()
		})
		if checksum != 53158 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_subscribe_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_unsubscribe()
		})
		if checksum != 42801 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_unsubscribe: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_app_unsubscribe_async()
		})
		if checksum != 44105 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_app_unsubscribe_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_bidistreamhandler_close_send()
		})
		if checksum != 34987 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_bidistreamhandler_close_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_bidistreamhandler_close_send_async()
		})
		if checksum != 43985 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_bidistreamhandler_close_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_bidistreamhandler_recv()
		})
		if checksum != 35283 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_bidistreamhandler_recv: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_bidistreamhandler_recv_async()
		})
		if checksum != 62278 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_bidistreamhandler_recv_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_bidistreamhandler_send()
		})
		if checksum != 33730 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_bidistreamhandler_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_bidistreamhandler_send_async()
		})
		if checksum != 54011 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_bidistreamhandler_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_channel_call_stream_stream()
		})
		if checksum != 2107 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_channel_call_stream_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_channel_call_stream_unary()
		})
		if checksum != 40967 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_channel_call_stream_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_channel_call_unary()
		})
		if checksum != 65159 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_channel_call_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_channel_call_unary_async()
		})
		if checksum != 43673 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_channel_call_unary_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_channel_call_unary_stream()
		})
		if checksum != 13465 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_channel_call_unary_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_channel_call_unary_stream_async()
		})
		if checksum != 61723 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_channel_call_unary_stream_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_completionhandle_wait()
		})
		if checksum != 24983 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_completionhandle_wait: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_completionhandle_wait_async()
		})
		if checksum != 35325 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_completionhandle_wait_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_completionhandle_wait_for()
		})
		if checksum != 61981 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_completionhandle_wait_for: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_completionhandle_wait_for_async()
		})
		if checksum != 7758 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_completionhandle_wait_for_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_context_deadline()
		})
		if checksum != 6991 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_context_deadline: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_context_is_deadline_exceeded()
		})
		if checksum != 2375 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_context_is_deadline_exceeded: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_context_metadata()
		})
		if checksum != 36093 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_context_metadata: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_context_remaining_time()
		})
		if checksum != 408 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_context_remaining_time: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_context_session_id()
		})
		if checksum != 4038 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_context_session_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_name_components()
		})
		if checksum != 49977 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_name_components: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_name_id()
		})
		if checksum != 28732 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_name_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_requeststream_next()
		})
		if checksum != 31564 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_requeststream_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_requeststream_next_async()
		})
		if checksum != 16598 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_requeststream_next_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_requeststreamwriter_finalize()
		})
		if checksum != 64754 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_requeststreamwriter_finalize: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_requeststreamwriter_finalize_async()
		})
		if checksum != 16474 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_requeststreamwriter_finalize_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_requeststreamwriter_send()
		})
		if checksum != 2655 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_requeststreamwriter_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_requeststreamwriter_send_async()
		})
		if checksum != 12842 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_requeststreamwriter_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsesink_close()
		})
		if checksum != 41933 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsesink_close: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsesink_close_async()
		})
		if checksum != 30800 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsesink_close_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsesink_is_closed()
		})
		if checksum != 2540 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsesink_is_closed: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsesink_is_closed_async()
		})
		if checksum != 57028 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsesink_is_closed_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsesink_send()
		})
		if checksum != 4243 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsesink_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsesink_send_async()
		})
		if checksum != 18881 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsesink_send_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsesink_send_error()
		})
		if checksum != 4313 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsesink_send_error: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsesink_send_error_async()
		})
		if checksum != 29805 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsesink_send_error_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsestreamreader_next()
		})
		if checksum != 10992 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsestreamreader_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_responsestreamreader_next_async()
		})
		if checksum != 58848 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_responsestreamreader_next_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_server_register_stream_stream()
		})
		if checksum != 15236 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_server_register_stream_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_server_register_stream_unary()
		})
		if checksum != 333 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_server_register_stream_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_server_register_unary_stream()
		})
		if checksum != 50458 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_server_register_unary_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_server_register_unary_unary()
		})
		if checksum != 16842 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_server_register_unary_unary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_server_serve()
		})
		if checksum != 52691 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_server_serve: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_server_serve_async()
		})
		if checksum != 28652 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_server_serve_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_server_shutdown()
		})
		if checksum != 39578 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_server_shutdown: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_server_shutdown_async()
		})
		if checksum != 36033 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_server_shutdown_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_config()
		})
		if checksum != 32098 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_connect()
		})
		if checksum != 51734 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_connect: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_connect_async()
		})
		if checksum != 25060 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_connect_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_create_app()
		})
		if checksum != 6710 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_create_app: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_create_app_async()
		})
		if checksum != 17578 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_create_app_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_create_app_with_direction()
		})
		if checksum != 32611 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_create_app_with_direction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_create_app_with_direction_async()
		})
		if checksum != 55495 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_create_app_with_direction_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_create_app_with_secret()
		})
		if checksum != 54746 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_create_app_with_secret: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_create_app_with_secret_async()
		})
		if checksum != 43226 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_create_app_with_secret_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_disconnect()
		})
		if checksum != 15579 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_disconnect: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_get_connection_id()
		})
		if checksum != 21647 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_get_connection_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_get_name()
		})
		if checksum != 14958 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_get_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_run()
		})
		if checksum != 39615 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_run: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_run_async()
		})
		if checksum != 12742 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_run_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_run_server()
		})
		if checksum != 29360 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_run_server: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_run_server_async()
		})
		if checksum != 24894 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_run_server_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_shutdown()
		})
		if checksum != 9865 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_shutdown: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_shutdown_async()
		})
		if checksum != 9544 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_shutdown_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_service_stop_server()
		})
		if checksum != 52012 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_service_stop_server: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_config()
		})
		if checksum != 40208 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_destination()
		})
		if checksum != 42059 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_destination: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_get_message()
		})
		if checksum != 53473 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_get_message: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_get_message_async()
		})
		if checksum != 56667 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_get_message_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_invite()
		})
		if checksum != 25093 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_invite: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_invite_and_wait()
		})
		if checksum != 29134 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_invite_and_wait: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_invite_and_wait_async()
		})
		if checksum != 27936 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_invite_and_wait_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_invite_async()
		})
		if checksum != 3867 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_invite_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_is_initiator()
		})
		if checksum != 55820 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_is_initiator: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_metadata()
		})
		if checksum != 27503 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_metadata: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_participants_list()
		})
		if checksum != 62568 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_participants_list: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_participants_list_async()
		})
		if checksum != 13982 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_participants_list_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish()
		})
		if checksum != 32701 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_and_wait()
		})
		if checksum != 58778 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_and_wait: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_and_wait_async()
		})
		if checksum != 4151 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_and_wait_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_async()
		})
		if checksum != 8206 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_to()
		})
		if checksum != 18923 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_to: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_to_and_wait()
		})
		if checksum != 53774 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_to_and_wait: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_to_and_wait_async()
		})
		if checksum != 62190 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_to_and_wait_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_to_async()
		})
		if checksum != 48126 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_to_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_with_params()
		})
		if checksum != 40703 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_with_params: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_publish_with_params_async()
		})
		if checksum != 16343 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_publish_with_params_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_remove()
		})
		if checksum != 19253 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_remove: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_remove_and_wait()
		})
		if checksum != 46797 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_remove_and_wait: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_remove_and_wait_async()
		})
		if checksum != 23062 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_remove_and_wait_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_remove_async()
		})
		if checksum != 702 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_remove_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_session_id()
		})
		if checksum != 54096 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_session_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_session_type()
		})
		if checksum != 62208 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_session_type: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_session_source()
		})
		if checksum != 18512 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_session_source: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_streamstreamhandler_handle()
		})
		if checksum != 34812 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_streamstreamhandler_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_streamunaryhandler_handle()
		})
		if checksum != 10750 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_streamunaryhandler_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_unarystreamhandler_handle()
		})
		if checksum != 24216 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_unarystreamhandler_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_method_unaryunaryhandler_handle()
		})
		if checksum != 57903 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_method_unaryunaryhandler_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_app_new()
		})
		if checksum != 29282 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_app_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_app_new_with_direction()
		})
		if checksum != 10680 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_app_new_with_direction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_app_new_with_secret()
		})
		if checksum != 34848 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_app_new_with_secret: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_channel_new()
		})
		if checksum != 43717 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_channel_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_channel_new_with_connection()
		})
		if checksum != 2629 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_channel_new_with_connection: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_name_new()
		})
		if checksum != 17614 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_name_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_name_new_with_id()
		})
		if checksum != 54828 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_name_new_with_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_server_new()
		})
		if checksum != 43254 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_server_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_server_new_with_connection()
		})
		if checksum != 37943 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_server_new_with_connection: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_service_new()
		})
		if checksum != 45367 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_service_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_slim_bindings_checksum_constructor_service_new_with_config()
		})
		if checksum != 16863 {
			// If this happens try cleaning and rebuilding your project
			panic("slim_bindings: uniffi_slim_bindings_checksum_constructor_service_new_with_config: UniFFI API checksum mismatch")
		}
	}
}

type FfiConverterUint32 struct{}

var FfiConverterUint32INSTANCE = FfiConverterUint32{}

func (FfiConverterUint32) Lower(value uint32) C.uint32_t {
	return C.uint32_t(value)
}

func (FfiConverterUint32) Write(writer io.Writer, value uint32) {
	writeUint32(writer, value)
}

func (FfiConverterUint32) Lift(value C.uint32_t) uint32 {
	return uint32(value)
}

func (FfiConverterUint32) Read(reader io.Reader) uint32 {
	return readUint32(reader)
}

type FfiDestroyerUint32 struct{}

func (FfiDestroyerUint32) Destroy(_ uint32) {}

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
	pointer       unsafe.Pointer
	callCounter   atomic.Int64
	cloneFunction func(unsafe.Pointer, *C.RustCallStatus) unsafe.Pointer
	freeFunction  func(unsafe.Pointer, *C.RustCallStatus)
	destroyed     atomic.Bool
}

func newFfiObject(
	pointer unsafe.Pointer,
	cloneFunction func(unsafe.Pointer, *C.RustCallStatus) unsafe.Pointer,
	freeFunction func(unsafe.Pointer, *C.RustCallStatus),
) FfiObject {
	return FfiObject{
		pointer:       pointer,
		cloneFunction: cloneFunction,
		freeFunction:  freeFunction,
	}
}

func (ffiObject *FfiObject) incrementPointer(debugName string) unsafe.Pointer {
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

	return rustCall(func(status *C.RustCallStatus) unsafe.Pointer {
		return ffiObject.cloneFunction(ffiObject.pointer, status)
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
	rustCall(func(status *C.RustCallStatus) int32 {
		ffiObject.freeFunction(ffiObject.pointer, status)
		return 0
	})
}

// Adapter that bridges the App API with language-bindings interface
//
// This adapter uses enum-based auth types (`AuthProvider`/`AuthVerifier`) instead of generics
// to be compatible with UniFFI, supporting multiple authentication mechanisms (SharedSecret,
// JWT, SPIRE, StaticToken). It provides both synchronous (blocking) and asynchronous methods
// for flexibility.
type AppInterface interface {
	// Create a new session (blocking version for FFI)
	//
	// Returns a SessionWithCompletion containing the session context and a completion handle.
	// Call `.wait()` on the completion handle to wait for session establishment.
	CreateSession(config SessionConfig, destination *Name) (SessionWithCompletion, error)
	// Create a new session and wait for completion (blocking version)
	//
	// This method creates a session and blocks until the session establishment completes.
	// Returns only the session context, as the completion has already been awaited.
	CreateSessionAndWait(config SessionConfig, destination *Name) (*Session, error)
	// Create a new session and wait for completion (async version)
	//
	// This method creates a session and waits until the session establishment completes.
	// Returns only the session context, as the completion has already been awaited.
	CreateSessionAndWaitAsync(config SessionConfig, destination *Name) (*Session, error)
	// Create a new session (async version)
	//
	// Returns a SessionWithCompletion containing the session context and a completion handle.
	// Await the completion handle to wait for session establishment.
	// For point-to-point sessions, this ensures the remote peer has acknowledged the session.
	// For multicast sessions, this ensures the initial setup is complete.
	CreateSessionAsync(config SessionConfig, destination *Name) (SessionWithCompletion, error)
	// Delete a session (blocking version for FFI)
	//
	// Returns a completion handle that can be awaited to ensure the deletion completes.
	DeleteSession(session *Session) (*CompletionHandle, error)
	// Delete a session and wait for completion (blocking version)
	//
	// This method deletes a session and blocks until the deletion completes.
	DeleteSessionAndWait(session *Session) error
	// Delete a session and wait for completion (async version)
	//
	// This method deletes a session and waits until the deletion completes.
	DeleteSessionAndWaitAsync(session *Session) error
	// Delete a session (async version)
	//
	// Returns a completion handle that can be awaited to ensure the deletion completes.
	DeleteSessionAsync(session *Session) (*CompletionHandle, error)
	// Get the app ID (derived from name)
	Id() uint64
	// Listen for incoming sessions (blocking version for FFI)
	ListenForSession(timeout *time.Duration) (*Session, error)
	// Listen for incoming sessions (async version)
	ListenForSessionAsync(timeout *time.Duration) (*Session, error)
	// Get the app name
	Name() *Name
	// Remove a route (blocking version for FFI)
	RemoveRoute(name *Name, connectionId uint64) error
	// Remove a route (async version)
	RemoveRouteAsync(name *Name, connectionId uint64) error
	// Set a route to a name for a specific connection (blocking version for FFI)
	SetRoute(name *Name, connectionId uint64) error
	// Set a route to a name for a specific connection (async version)
	SetRouteAsync(name *Name, connectionId uint64) error
	// Subscribe to a session name (blocking version for FFI)
	Subscribe(name *Name, connectionId *uint64) error
	// Subscribe to a name (async version)
	SubscribeAsync(name *Name, connectionId *uint64) error
	// Unsubscribe from a name (blocking version for FFI)
	Unsubscribe(name *Name, connectionId *uint64) error
	// Unsubscribe from a name (async version)
	UnsubscribeAsync(name *Name, connectionId *uint64) error
}

// Adapter that bridges the App API with language-bindings interface
//
// This adapter uses enum-based auth types (`AuthProvider`/`AuthVerifier`) instead of generics
// to be compatible with UniFFI, supporting multiple authentication mechanisms (SharedSecret,
// JWT, SPIRE, StaticToken). It provides both synchronous (blocking) and asynchronous methods
// for flexibility.
type App struct {
	ffiObject FfiObject
}

// Create a new App with identity provider and verifier configurations
//
// This is the main entry point for creating a SLIM application from language bindings.
//
// # Arguments
// * `base_name` - The base name for the app (without ID)
// * `identity_provider_config` - Configuration for proving identity to others
// * `identity_verifier_config` - Configuration for verifying identity of others
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created adapter
// * `Err(SlimError)` - If adapter creation fails
//
// # Supported Identity Types
// - SharedSecret: Symmetric key authentication
// - JWT: Dynamic JWT generation/verification with signing/decoding keys
// - StaticJWT: Static JWT loaded from file with auto-reload
func NewApp(baseName *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig) (*App, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_app_new(FfiConverterNameINSTANCE.Lower(baseName), FfiConverterIdentityProviderConfigINSTANCE.Lower(identityProviderConfig), FfiConverterIdentityVerifierConfigINSTANCE.Lower(identityVerifierConfig), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *App
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAppINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new App with traffic direction (blocking version)
//
// This is a convenience function for creating a SLIM application with configurable
// traffic direction (send-only, receive-only, bidirectional, or none).
//
// # Arguments
// * `name` - The base name for the app (without ID)
// * `identity_provider_config` - Configuration for proving identity to others
// * `identity_verifier_config` - Configuration for verifying identity of others
// * `direction` - Traffic direction for sessions (Send, Recv, Bidirectional, or None)
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created app
// * `Err(SlimError)` - If app creation fails
func AppNewWithDirection(name *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig, direction Direction) (*App, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_app_new_with_direction(FfiConverterNameINSTANCE.Lower(name), FfiConverterIdentityProviderConfigINSTANCE.Lower(identityProviderConfig), FfiConverterIdentityVerifierConfigINSTANCE.Lower(identityVerifierConfig), FfiConverterDirectionINSTANCE.Lower(direction), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *App
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAppINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new App with SharedSecret authentication (blocking version)
//
// This is a convenience function for creating a SLIM application using SharedSecret authentication.
//
// # Arguments
// * `name` - The base name for the app (without ID)
// * `secret` - The shared secret string for authentication
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created adapter
// * `Err(SlimError)` - If adapter creation fails
func AppNewWithSecret(name *Name, secret string) (*App, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_app_new_with_secret(FfiConverterNameINSTANCE.Lower(name), FfiConverterStringINSTANCE.Lower(secret), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *App
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAppINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new session (blocking version for FFI)
//
// Returns a SessionWithCompletion containing the session context and a completion handle.
// Call `.wait()` on the completion handle to wait for session establishment.
func (_self *App) CreateSession(config SessionConfig, destination *Name) (SessionWithCompletion, error) {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_app_create_session(
				_pointer, FfiConverterSessionConfigINSTANCE.Lower(config), FfiConverterNameINSTANCE.Lower(destination), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue SessionWithCompletion
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSessionWithCompletionINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new session and wait for completion (blocking version)
//
// This method creates a session and blocks until the session establishment completes.
// Returns only the session context, as the completion has already been awaited.
func (_self *App) CreateSessionAndWait(config SessionConfig, destination *Name) (*Session, error) {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_app_create_session_and_wait(
			_pointer, FfiConverterSessionConfigINSTANCE.Lower(config), FfiConverterNameINSTANCE.Lower(destination), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Session
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSessionINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new session and wait for completion (async version)
//
// This method creates a session and waits until the session establishment completes.
// Returns only the session context, as the completion has already been awaited.
func (_self *App) CreateSessionAndWaitAsync(config SessionConfig, destination *Name) (*Session, error) {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *Session {
			return FfiConverterSessionINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_app_create_session_and_wait_async(
			_pointer, FfiConverterSessionConfigINSTANCE.Lower(config), FfiConverterNameINSTANCE.Lower(destination)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Create a new session (async version)
//
// Returns a SessionWithCompletion containing the session context and a completion handle.
// Await the completion handle to wait for session establishment.
// For point-to-point sessions, this ensures the remote peer has acknowledged the session.
// For multicast sessions, this ensures the initial setup is complete.
func (_self *App) CreateSessionAsync(config SessionConfig, destination *Name) (SessionWithCompletion, error) {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionWithCompletion {
			return FfiConverterSessionWithCompletionINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_app_create_session_async(
			_pointer, FfiConverterSessionConfigINSTANCE.Lower(config), FfiConverterNameINSTANCE.Lower(destination)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Delete a session (blocking version for FFI)
//
// Returns a completion handle that can be awaited to ensure the deletion completes.
func (_self *App) DeleteSession(session *Session) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_app_delete_session(
			_pointer, FfiConverterSessionINSTANCE.Lower(session), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *CompletionHandle
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterCompletionHandleINSTANCE.Lift(_uniffiRV), nil
	}
}

// Delete a session and wait for completion (blocking version)
//
// This method deletes a session and blocks until the deletion completes.
func (_self *App) DeleteSessionAndWait(session *Session) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_app_delete_session_and_wait(
			_pointer, FfiConverterSessionINSTANCE.Lower(session), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Delete a session and wait for completion (async version)
//
// This method deletes a session and waits until the deletion completes.
func (_self *App) DeleteSessionAndWaitAsync(session *Session) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_app_delete_session_and_wait_async(
			_pointer, FfiConverterSessionINSTANCE.Lower(session)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Delete a session (async version)
//
// Returns a completion handle that can be awaited to ensure the deletion completes.
func (_self *App) DeleteSessionAsync(session *Session) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *CompletionHandle {
			return FfiConverterCompletionHandleINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_app_delete_session_async(
			_pointer, FfiConverterSessionINSTANCE.Lower(session)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the app ID (derived from name)
func (_self *App) Id() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_bindings_fn_method_app_id(
			_pointer, _uniffiStatus)
	}))
}

// Listen for incoming sessions (blocking version for FFI)
func (_self *App) ListenForSession(timeout *time.Duration) (*Session, error) {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_app_listen_for_session(
			_pointer, FfiConverterOptionalDurationINSTANCE.Lower(timeout), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Session
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSessionINSTANCE.Lift(_uniffiRV), nil
	}
}

// Listen for incoming sessions (async version)
func (_self *App) ListenForSessionAsync(timeout *time.Duration) (*Session, error) {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *Session {
			return FfiConverterSessionINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_app_listen_for_session_async(
			_pointer, FfiConverterOptionalDurationINSTANCE.Lower(timeout)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the app name
func (_self *App) Name() *Name {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterNameINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_app_name(
			_pointer, _uniffiStatus)
	}))
}

// Remove a route (blocking version for FFI)
func (_self *App) RemoveRoute(name *Name, connectionId uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_app_remove_route(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterUint64INSTANCE.Lower(connectionId), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Remove a route (async version)
func (_self *App) RemoveRouteAsync(name *Name, connectionId uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_app_remove_route_async(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterUint64INSTANCE.Lower(connectionId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Set a route to a name for a specific connection (blocking version for FFI)
func (_self *App) SetRoute(name *Name, connectionId uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_app_set_route(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterUint64INSTANCE.Lower(connectionId), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set a route to a name for a specific connection (async version)
func (_self *App) SetRouteAsync(name *Name, connectionId uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_app_set_route_async(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterUint64INSTANCE.Lower(connectionId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Subscribe to a session name (blocking version for FFI)
func (_self *App) Subscribe(name *Name, connectionId *uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_app_subscribe(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterOptionalUint64INSTANCE.Lower(connectionId), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Subscribe to a name (async version)
func (_self *App) SubscribeAsync(name *Name, connectionId *uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_app_subscribe_async(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterOptionalUint64INSTANCE.Lower(connectionId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Unsubscribe from a name (blocking version for FFI)
func (_self *App) Unsubscribe(name *Name, connectionId *uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_app_unsubscribe(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterOptionalUint64INSTANCE.Lower(connectionId), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Unsubscribe from a name (async version)
func (_self *App) UnsubscribeAsync(name *Name, connectionId *uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*App")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_app_unsubscribe_async(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterOptionalUint64INSTANCE.Lower(connectionId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *App) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterApp struct{}

var FfiConverterAppINSTANCE = FfiConverterApp{}

func (c FfiConverterApp) Lift(pointer unsafe.Pointer) *App {
	result := &App{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_app(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_app(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*App).Destroy)
	return result
}

func (c FfiConverterApp) Read(reader io.Reader) *App {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterApp) Lower(value *App) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*App")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterApp) Write(writer io.Writer, value *App) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerApp struct{}

func (_ FfiDestroyerApp) Destroy(value *App) {
	value.Destroy()
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
	_, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_bidistreamhandler_close_send(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Close the request stream (async version)
func (_self *BidiStreamHandler) CloseSendAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_bidistreamhandler_close_send_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
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
			inner: C.uniffi_slim_bindings_fn_method_bidistreamhandler_recv(
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
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) StreamMessage {
			return FfiConverterStreamMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_bidistreamhandler_recv_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
		},
	)

	return res
}

// Send a request message to the stream (blocking version)
func (_self *BidiStreamHandler) Send(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_bidistreamhandler_send(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send a request message to the stream (async version)
func (_self *BidiStreamHandler) SendAsync(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*BidiStreamHandler")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_bidistreamhandler_send_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
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

func (c FfiConverterBidiStreamHandler) Lift(pointer unsafe.Pointer) *BidiStreamHandler {
	result := &BidiStreamHandler{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_bidistreamhandler(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_bidistreamhandler(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*BidiStreamHandler).Destroy)
	return result
}

func (c FfiConverterBidiStreamHandler) Read(reader io.Reader) *BidiStreamHandler {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterBidiStreamHandler) Lower(value *BidiStreamHandler) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*BidiStreamHandler")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterBidiStreamHandler) Write(writer io.Writer, value *BidiStreamHandler) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerBidiStreamHandler struct{}

func (_ FfiDestroyerBidiStreamHandler) Destroy(value *BidiStreamHandler) {
	value.Destroy()
}

// Client-side channel for making RPC calls
//
// A Channel manages the connection to a remote service and provides methods
// for making RPC calls with different streaming patterns.
//
// Each RPC call creates a new session which is closed after the RPC completes.
type ChannelInterface interface {
	// Make a stream-to-stream RPC call (blocking version)
	//
	// # Arguments
	// * `service_name` - The service name
	// * `method_name` - The method name
	// * `timeout` - Optional timeout duration
	//
	// # Returns
	// A BidiStreamHandler for sending and receiving messages
	//
	// # Note
	// This returns a BidiStreamHandler that can be used to send request messages
	// and read response messages concurrently.
	CallStreamStream(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *BidiStreamHandler
	// Make a stream-to-unary RPC call (blocking version)
	//
	// # Arguments
	// * `service_name` - The service name
	// * `method_name` - The method name
	// * `timeout` - Optional timeout duration
	//
	// # Returns
	// A RequestStreamWriter for sending request messages and getting the final response
	//
	// # Note
	// This returns a RequestStreamWriter that can be used to send multiple request
	// messages and then finalize to get the single response.
	CallStreamUnary(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *RequestStreamWriter
	// Make a unary-to-unary RPC call (blocking version)
	//
	// # Arguments
	// * `service_name` - The service name (e.g., "MyService")
	// * `method_name` - The method name (e.g., "GetUser")
	// * `request` - The request message bytes
	// * `timeout` - Optional timeout duration
	//
	// # Returns
	// The response message bytes or an error
	CallUnary(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) ([]byte, error)
	// Make a unary-to-unary RPC call (async version)
	//
	// # Arguments
	// * `service_name` - The service name (e.g., "MyService")
	// * `method_name` - The method name (e.g., "GetUser")
	// * `request` - The request message bytes
	// * `timeout` - Optional timeout duration
	//
	// # Returns
	// The response message bytes or an error
	CallUnaryAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) ([]byte, error)
	// Make a unary-to-stream RPC call (blocking version)
	//
	// # Arguments
	// * `service_name` - The service name
	// * `method_name` - The method name
	// * `request` - The request message bytes
	// * `timeout` - Optional timeout duration
	//
	// # Returns
	// A stream reader for pulling response messages
	//
	// # Note
	// This returns a ResponseStreamReader that can be used to pull messages
	// one at a time from the response stream.
	CallUnaryStream(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*ResponseStreamReader, error)
	// Make a unary-to-stream RPC call (async version)
	//
	// # Arguments
	// * `service_name` - The service name
	// * `method_name` - The method name
	// * `request` - The request message bytes
	// * `timeout` - Optional timeout duration
	//
	// # Returns
	// A stream reader for pulling response messages
	//
	// # Note
	// This returns a ResponseStreamReader that can be used to pull messages
	// one at a time from the response stream.
	CallUnaryStreamAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*ResponseStreamReader, error)
}

// Client-side channel for making RPC calls
//
// A Channel manages the connection to a remote service and provides methods
// for making RPC calls with different streaming patterns.
//
// Each RPC call creates a new session which is closed after the RPC completes.
type Channel struct {
	ffiObject FfiObject
}

// Create a new RPC channel
//
// # Arguments
// * `app` - The SLIM application instance
// * `remote` - The remote service name to connect to
//
// # Returns
// A new channel instance
func NewChannel(app *App, remote *Name) *Channel {
	return FfiConverterChannelINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_channel_new(FfiConverterAppINSTANCE.Lower(app), FfiConverterNameINSTANCE.Lower(remote), _uniffiStatus)
	}))
}

// Create a new RPC channel with optional connection ID
//
// The connection ID is used to set up routing before making RPC calls,
// enabling multi-hop RPC calls through specific connections.
//
// # Arguments
// * `app` - The SLIM application instance
// * `remote` - The remote service name to connect to
// * `connection_id` - Optional connection ID for routing setup
//
// # Returns
// A new channel instance
func ChannelNewWithConnection(app *App, remote *Name, connectionId *uint64) *Channel {
	return FfiConverterChannelINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_channel_new_with_connection(FfiConverterAppINSTANCE.Lower(app), FfiConverterNameINSTANCE.Lower(remote), FfiConverterOptionalUint64INSTANCE.Lower(connectionId), _uniffiStatus)
	}))
}

// Make a stream-to-stream RPC call (blocking version)
//
// # Arguments
// * `service_name` - The service name
// * `method_name` - The method name
// * `timeout` - Optional timeout duration
//
// # Returns
// A BidiStreamHandler for sending and receiving messages
//
// # Note
// This returns a BidiStreamHandler that can be used to send request messages
// and read response messages concurrently.
func (_self *Channel) CallStreamStream(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *BidiStreamHandler {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBidiStreamHandlerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_channel_call_stream_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	}))
}

// Make a stream-to-unary RPC call (blocking version)
//
// # Arguments
// * `service_name` - The service name
// * `method_name` - The method name
// * `timeout` - Optional timeout duration
//
// # Returns
// A RequestStreamWriter for sending request messages and getting the final response
//
// # Note
// This returns a RequestStreamWriter that can be used to send multiple request
// messages and then finalize to get the single response.
func (_self *Channel) CallStreamUnary(serviceName string, methodName string, timeout *time.Duration, metadata *map[string]string) *RequestStreamWriter {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterRequestStreamWriterINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_channel_call_stream_unary(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	}))
}

// Make a unary-to-unary RPC call (blocking version)
//
// # Arguments
// * `service_name` - The service name (e.g., "MyService")
// * `method_name` - The method name (e.g., "GetUser")
// * `request` - The request message bytes
// * `timeout` - Optional timeout duration
//
// # Returns
// The response message bytes or an error
func (_self *Channel) CallUnary(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_channel_call_unary(
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

// Make a unary-to-unary RPC call (async version)
//
// # Arguments
// * `service_name` - The service name (e.g., "MyService")
// * `method_name` - The method name (e.g., "GetUser")
// * `request` - The request message bytes
// * `timeout` - Optional timeout duration
//
// # Returns
// The response message bytes or an error
func (_self *Channel) CallUnaryAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_channel_call_unary_async(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Make a unary-to-stream RPC call (blocking version)
//
// # Arguments
// * `service_name` - The service name
// * `method_name` - The method name
// * `request` - The request message bytes
// * `timeout` - Optional timeout duration
//
// # Returns
// A stream reader for pulling response messages
//
// # Note
// This returns a ResponseStreamReader that can be used to pull messages
// one at a time from the response stream.
func (_self *Channel) CallUnaryStream(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*ResponseStreamReader, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_channel_call_unary_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *ResponseStreamReader
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterResponseStreamReaderINSTANCE.Lift(_uniffiRV), nil
	}
}

// Make a unary-to-stream RPC call (async version)
//
// # Arguments
// * `service_name` - The service name
// * `method_name` - The method name
// * `request` - The request message bytes
// * `timeout` - Optional timeout duration
//
// # Returns
// A stream reader for pulling response messages
//
// # Note
// This returns a ResponseStreamReader that can be used to pull messages
// one at a time from the response stream.
func (_self *Channel) CallUnaryStreamAsync(serviceName string, methodName string, request []byte, timeout *time.Duration, metadata *map[string]string) (*ResponseStreamReader, error) {
	_pointer := _self.ffiObject.incrementPointer("*Channel")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *ResponseStreamReader {
			return FfiConverterResponseStreamReaderINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_channel_call_unary_stream_async(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterBytesINSTANCE.Lower(request), FfiConverterOptionalDurationINSTANCE.Lower(timeout), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *Channel) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterChannel struct{}

var FfiConverterChannelINSTANCE = FfiConverterChannel{}

func (c FfiConverterChannel) Lift(pointer unsafe.Pointer) *Channel {
	result := &Channel{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_channel(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_channel(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Channel).Destroy)
	return result
}

func (c FfiConverterChannel) Read(reader io.Reader) *Channel {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterChannel) Lower(value *Channel) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*Channel")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterChannel) Write(writer io.Writer, value *Channel) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerChannel struct{}

func (_ FfiDestroyerChannel) Destroy(value *Channel) {
	value.Destroy()
}

// FFI-compatible completion handle for async operations
//
// Represents a pending operation that can be awaited to ensure completion.
// Used for operations that need delivery confirmation or handshake acknowledgment.
//
// # Examples
//
// Basic usage:
// ```ignore
// let completion = session.publish(data, None, None)?;
// completion.wait()?; // Wait for delivery confirmation
// ```
type CompletionHandleInterface interface {
	// Wait for the operation to complete indefinitely (blocking version)
	//
	// This blocks the calling thread until the operation completes.
	// Use this from Go or other languages when you need to ensure
	// an operation has finished before proceeding.
	//
	// **Note:** This can only be called once per handle. Subsequent calls
	// will return an error.
	//
	// # Returns
	// * `Ok(())` - Operation completed successfully
	// * `Err(SlimError)` - Operation failed or handle already consumed
	Wait() error
	// Wait for the operation to complete indefinitely (async version)
	//
	// This is the async version that integrates with UniFFI's polling mechanism.
	// The operation will yield control while waiting.
	//
	// **Note:** This can only be called once per handle. Subsequent calls
	// will return an error.
	//
	// # Returns
	// * `Ok(())` - Operation completed successfully
	// * `Err(SlimError)` - Operation failed or handle already consumed
	WaitAsync() error
	// Wait for the operation to complete with a timeout (blocking version)
	//
	// This blocks the calling thread until the operation completes or the timeout expires.
	// Use this from Go or other languages when you need to ensure
	// an operation has finished before proceeding with a time limit.
	//
	// **Note:** This can only be called once per handle. Subsequent calls
	// will return an error.
	//
	// # Arguments
	// * `timeout` - Maximum time to wait for completion
	//
	// # Returns
	// * `Ok(())` - Operation completed successfully
	// * `Err(SlimError::Timeout)` - If the operation timed out
	// * `Err(SlimError)` - Operation failed or handle already consumed
	WaitFor(timeout time.Duration) error
	// Wait for the operation to complete with a timeout (async version)
	//
	// This is the async version that integrates with UniFFI's polling mechanism.
	// The operation will yield control while waiting until completion or timeout.
	//
	// **Note:** This can only be called once per handle. Subsequent calls
	// will return an error.
	//
	// # Arguments
	// * `timeout` - Maximum time to wait for completion
	//
	// # Returns
	// * `Ok(())` - Operation completed successfully
	// * `Err(SlimError::Timeout)` - If the operation timed out
	// * `Err(SlimError)` - Operation failed or handle already consumed
	WaitForAsync(timeout time.Duration) error
}

// FFI-compatible completion handle for async operations
//
// Represents a pending operation that can be awaited to ensure completion.
// Used for operations that need delivery confirmation or handshake acknowledgment.
//
// # Examples
//
// Basic usage:
// ```ignore
// let completion = session.publish(data, None, None)?;
// completion.wait()?; // Wait for delivery confirmation
// ```
type CompletionHandle struct {
	ffiObject FfiObject
}

// Wait for the operation to complete indefinitely (blocking version)
//
// This blocks the calling thread until the operation completes.
// Use this from Go or other languages when you need to ensure
// an operation has finished before proceeding.
//
// **Note:** This can only be called once per handle. Subsequent calls
// will return an error.
//
// # Returns
// * `Ok(())` - Operation completed successfully
// * `Err(SlimError)` - Operation failed or handle already consumed
func (_self *CompletionHandle) Wait() error {
	_pointer := _self.ffiObject.incrementPointer("*CompletionHandle")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_completionhandle_wait(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Wait for the operation to complete indefinitely (async version)
//
// This is the async version that integrates with UniFFI's polling mechanism.
// The operation will yield control while waiting.
//
// **Note:** This can only be called once per handle. Subsequent calls
// will return an error.
//
// # Returns
// * `Ok(())` - Operation completed successfully
// * `Err(SlimError)` - Operation failed or handle already consumed
func (_self *CompletionHandle) WaitAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*CompletionHandle")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_completionhandle_wait_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Wait for the operation to complete with a timeout (blocking version)
//
// This blocks the calling thread until the operation completes or the timeout expires.
// Use this from Go or other languages when you need to ensure
// an operation has finished before proceeding with a time limit.
//
// **Note:** This can only be called once per handle. Subsequent calls
// will return an error.
//
// # Arguments
// * `timeout` - Maximum time to wait for completion
//
// # Returns
// * `Ok(())` - Operation completed successfully
// * `Err(SlimError::Timeout)` - If the operation timed out
// * `Err(SlimError)` - Operation failed or handle already consumed
func (_self *CompletionHandle) WaitFor(timeout time.Duration) error {
	_pointer := _self.ffiObject.incrementPointer("*CompletionHandle")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_completionhandle_wait_for(
			_pointer, FfiConverterDurationINSTANCE.Lower(timeout), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Wait for the operation to complete with a timeout (async version)
//
// This is the async version that integrates with UniFFI's polling mechanism.
// The operation will yield control while waiting until completion or timeout.
//
// **Note:** This can only be called once per handle. Subsequent calls
// will return an error.
//
// # Arguments
// * `timeout` - Maximum time to wait for completion
//
// # Returns
// * `Ok(())` - Operation completed successfully
// * `Err(SlimError::Timeout)` - If the operation timed out
// * `Err(SlimError)` - Operation failed or handle already consumed
func (_self *CompletionHandle) WaitForAsync(timeout time.Duration) error {
	_pointer := _self.ffiObject.incrementPointer("*CompletionHandle")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_completionhandle_wait_for_async(
			_pointer, FfiConverterDurationINSTANCE.Lower(timeout)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *CompletionHandle) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterCompletionHandle struct{}

var FfiConverterCompletionHandleINSTANCE = FfiConverterCompletionHandle{}

func (c FfiConverterCompletionHandle) Lift(pointer unsafe.Pointer) *CompletionHandle {
	result := &CompletionHandle{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_completionhandle(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_completionhandle(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*CompletionHandle).Destroy)
	return result
}

func (c FfiConverterCompletionHandle) Read(reader io.Reader) *CompletionHandle {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterCompletionHandle) Lower(value *CompletionHandle) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*CompletionHandle")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterCompletionHandle) Write(writer io.Writer, value *CompletionHandle) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerCompletionHandle struct{}

func (_ FfiDestroyerCompletionHandle) Destroy(value *CompletionHandle) {
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
			inner: C.uniffi_slim_bindings_fn_method_context_deadline(
				_pointer, _uniffiStatus),
		}
	}))
}

// Check if the deadline has been exceeded
func (_self *Context) IsDeadlineExceeded() bool {
	_pointer := _self.ffiObject.incrementPointer("*Context")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_slim_bindings_fn_method_context_is_deadline_exceeded(
			_pointer, _uniffiStatus)
	}))
}

// Get the rpc session metadata
func (_self *Context) Metadata() map[string]string {
	_pointer := _self.ffiObject.incrementPointer("*Context")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMapStringStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_context_metadata(
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
			inner: C.uniffi_slim_bindings_fn_method_context_remaining_time(
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
			inner: C.uniffi_slim_bindings_fn_method_context_session_id(
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

func (c FfiConverterContext) Lift(pointer unsafe.Pointer) *Context {
	result := &Context{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_context(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_context(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Context).Destroy)
	return result
}

func (c FfiConverterContext) Read(reader io.Reader) *Context {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterContext) Lower(value *Context) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*Context")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterContext) Write(writer io.Writer, value *Context) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerContext struct{}

func (_ FfiDestroyerContext) Destroy(value *Context) {
	value.Destroy()
}

// Name type for SLIM (Secure Low-Latency Interactive Messaging)
type NameInterface interface {
	// Get the name components as a vector of strings
	Components() []string
	// Get the name ID
	Id() uint64
}

// Name type for SLIM (Secure Low-Latency Interactive Messaging)
type Name struct {
	ffiObject FfiObject
}

// Create a new Name from components without an ID
func NewName(component0 string, component1 string, component2 string) *Name {
	return FfiConverterNameINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_name_new(FfiConverterStringINSTANCE.Lower(component0), FfiConverterStringINSTANCE.Lower(component1), FfiConverterStringINSTANCE.Lower(component2), _uniffiStatus)
	}))
}

// Create a new Name from components with an ID
func NameNewWithId(component0 string, component1 string, component2 string, id uint64) *Name {
	return FfiConverterNameINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_name_new_with_id(FfiConverterStringINSTANCE.Lower(component0), FfiConverterStringINSTANCE.Lower(component1), FfiConverterStringINSTANCE.Lower(component2), FfiConverterUint64INSTANCE.Lower(id), _uniffiStatus)
	}))
}

// Get the name components as a vector of strings
func (_self *Name) Components() []string {
	_pointer := _self.ffiObject.incrementPointer("*Name")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_name_components(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the name ID
func (_self *Name) Id() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*Name")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_bindings_fn_method_name_id(
			_pointer, _uniffiStatus)
	}))
}

func (_self *Name) DebugString() string {
	_pointer := _self.ffiObject.incrementPointer("*Name")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_name_uniffi_trait_debug(
				_pointer, _uniffiStatus),
		}
	}))
}

func (_self *Name) String() string {
	_pointer := _self.ffiObject.incrementPointer("*Name")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_name_uniffi_trait_display(
				_pointer, _uniffiStatus),
		}
	}))
}

func (_self *Name) Eq(other *Name) bool {
	_pointer := _self.ffiObject.incrementPointer("*Name")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_slim_bindings_fn_method_name_uniffi_trait_eq_eq(
			_pointer, FfiConverterNameINSTANCE.Lower(other), _uniffiStatus)
	}))
}

func (_self *Name) Ne(other *Name) bool {
	_pointer := _self.ffiObject.incrementPointer("*Name")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_slim_bindings_fn_method_name_uniffi_trait_eq_ne(
			_pointer, FfiConverterNameINSTANCE.Lower(other), _uniffiStatus)
	}))
}

func (object *Name) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterName struct{}

var FfiConverterNameINSTANCE = FfiConverterName{}

func (c FfiConverterName) Lift(pointer unsafe.Pointer) *Name {
	result := &Name{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_name(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_name(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Name).Destroy)
	return result
}

func (c FfiConverterName) Read(reader io.Reader) *Name {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterName) Lower(value *Name) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*Name")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterName) Write(writer io.Writer, value *Name) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerName struct{}

func (_ FfiDestroyerName) Destroy(value *Name) {
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
			inner: C.uniffi_slim_bindings_fn_method_requeststream_next(
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
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) StreamMessage {
			return FfiConverterStreamMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_requeststream_next_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
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

func (c FfiConverterRequestStream) Lift(pointer unsafe.Pointer) *RequestStream {
	result := &RequestStream{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_requeststream(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_requeststream(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*RequestStream).Destroy)
	return result
}

func (c FfiConverterRequestStream) Read(reader io.Reader) *RequestStream {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterRequestStream) Lower(value *RequestStream) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*RequestStream")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterRequestStream) Write(writer io.Writer, value *RequestStream) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerRequestStream struct{}

func (_ FfiDestroyerRequestStream) Destroy(value *RequestStream) {
	value.Destroy()
}

// Request stream writer for stream-to-unary RPC calls
//
// Allows sending multiple request messages and getting a final response.
type RequestStreamWriterInterface interface {
	// Finalize the stream and get the response (blocking version)
	Finalize() ([]byte, error)
	// Finalize the stream and get the response (async version)
	FinalizeAsync() ([]byte, error)
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

// Finalize the stream and get the response (blocking version)
func (_self *RequestStreamWriter) Finalize() ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*RequestStreamWriter")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_requeststreamwriter_finalize(
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
func (_self *RequestStreamWriter) FinalizeAsync() ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*RequestStreamWriter")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_requeststreamwriter_finalize_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
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
	_, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_requeststreamwriter_send(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send a request message to the stream (async version)
func (_self *RequestStreamWriter) SendAsync(data []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*RequestStreamWriter")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_requeststreamwriter_send_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
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

func (c FfiConverterRequestStreamWriter) Lift(pointer unsafe.Pointer) *RequestStreamWriter {
	result := &RequestStreamWriter{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_requeststreamwriter(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_requeststreamwriter(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*RequestStreamWriter).Destroy)
	return result
}

func (c FfiConverterRequestStreamWriter) Read(reader io.Reader) *RequestStreamWriter {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterRequestStreamWriter) Lower(value *RequestStreamWriter) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*RequestStreamWriter")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterRequestStreamWriter) Write(writer io.Writer, value *RequestStreamWriter) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
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
	_, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_responsesink_close(
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
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_responsesink_close_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
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
		return C.uniffi_slim_bindings_fn_method_responsesink_is_closed(
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
			res := C.ffi_slim_bindings_rust_future_complete_i8(handle, status)
			return res
		},
		// liftFn
		func(ffi C.int8_t) bool {
			return FfiConverterBoolINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_responsesink_is_closed_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_i8(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_i8(handle)
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
	_, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_responsesink_send(
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
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_responsesink_send_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
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
	_, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_responsesink_send_error(
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
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_responsesink_send_error_async(
			_pointer, FfiConverterRpcErrorINSTANCE.Lower(error)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
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

func (c FfiConverterResponseSink) Lift(pointer unsafe.Pointer) *ResponseSink {
	result := &ResponseSink{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_responsesink(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_responsesink(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*ResponseSink).Destroy)
	return result
}

func (c FfiConverterResponseSink) Read(reader io.Reader) *ResponseSink {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterResponseSink) Lower(value *ResponseSink) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*ResponseSink")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterResponseSink) Write(writer io.Writer, value *ResponseSink) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
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
			inner: C.uniffi_slim_bindings_fn_method_responsestreamreader_next(
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
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) StreamMessage {
			return FfiConverterStreamMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_responsestreamreader_next_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
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

func (c FfiConverterResponseStreamReader) Lift(pointer unsafe.Pointer) *ResponseStreamReader {
	result := &ResponseStreamReader{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_responsestreamreader(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_responsestreamreader(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*ResponseStreamReader).Destroy)
	return result
}

func (c FfiConverterResponseStreamReader) Read(reader io.Reader) *ResponseStreamReader {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterResponseStreamReader) Lower(value *ResponseStreamReader) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*ResponseStreamReader")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterResponseStreamReader) Write(writer io.Writer, value *ResponseStreamReader) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
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
// ```no_run
// # use slim_bindings::{Server, Context, Status, Decoder, Encoder, App, Name};
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
// #     fn decode(_buf: impl Into<Vec<u8>>) -> Result<Self, Status> { Ok(Request::default()) }
// # }
// # #[derive(Default)]
// # struct Response {}
// # impl Encoder for Response {
// #     fn encode(self) -> Result<Vec<u8>, Status> { Ok(vec![]) }
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
	// Register a stream-to-stream RPC handler
	//
	// # Arguments
	// * `service_name` - The service name
	// * `method_name` - The method name
	// * `handler` - Implementation of the StreamStreamHandler trait
	RegisterStreamStream(serviceName string, methodName string, handler StreamStreamHandler)
	// Register a stream-to-unary RPC handler
	//
	// # Arguments
	// * `service_name` - The service name
	// * `method_name` - The method name
	// * `handler` - Implementation of the StreamUnaryHandler trait
	RegisterStreamUnary(serviceName string, methodName string, handler StreamUnaryHandler)
	// Register a unary-to-stream RPC handler
	//
	// # Arguments
	// * `service_name` - The service name
	// * `method_name` - The method name
	// * `handler` - Implementation of the UnaryStreamHandler trait
	RegisterUnaryStream(serviceName string, methodName string, handler UnaryStreamHandler)
	// Register a unary-to-unary RPC handler
	//
	// # Arguments
	// * `service_name` - The service name (e.g., "MyService")
	// * `method_name` - The method name (e.g., "GetUser")
	// * `handler` - Implementation of the UnaryUnaryHandler trait
	RegisterUnaryUnary(serviceName string, methodName string, handler UnaryUnaryHandler)
	// Start serving RPC requests (blocking version)
	//
	// This is a blocking method that runs until the server is shut down.
	// It listens for incoming RPC calls and dispatches them to registered handlers.
	Serve() error
	// Start serving RPC requests (async version)
	//
	// This is an async method that runs until the server is shut down.
	// It listens for incoming RPC calls and dispatches them to registered handlers.
	ServeAsync() error
	// Shutdown the server gracefully (blocking version)
	//
	// This signals the server to stop accepting new requests and wait for
	// in-flight requests to complete.
	Shutdown()
	// Shutdown the server gracefully (async version)
	//
	// This signals the server to stop accepting new requests and wait for
	// in-flight requests to complete.
	ShutdownAsync()
}

// RPC Server
//
// Handles incoming RPC requests by creating sessions and dispatching
// to registered service handlers.
//
// # Example
//
// ```no_run
// # use slim_bindings::{Server, Context, Status, Decoder, Encoder, App, Name};
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
// #     fn decode(_buf: impl Into<Vec<u8>>) -> Result<Self, Status> { Ok(Request::default()) }
// # }
// # #[derive(Default)]
// # struct Response {}
// # impl Encoder for Response {
// #     fn encode(self) -> Result<Vec<u8>, Status> { Ok(vec![]) }
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
func NewServer(app *App, baseName *Name) *Server {
	return FfiConverterServerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_server_new(FfiConverterAppINSTANCE.Lower(app), FfiConverterNameINSTANCE.Lower(baseName), _uniffiStatus)
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
func ServerNewWithConnection(app *App, baseName *Name, connectionId *uint64) *Server {
	return FfiConverterServerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_server_new_with_connection(FfiConverterAppINSTANCE.Lower(app), FfiConverterNameINSTANCE.Lower(baseName), FfiConverterOptionalUint64INSTANCE.Lower(connectionId), _uniffiStatus)
	}))
}

// Register a stream-to-stream RPC handler
//
// # Arguments
// * `service_name` - The service name
// * `method_name` - The method name
// * `handler` - Implementation of the StreamStreamHandler trait
func (_self *Server) RegisterStreamStream(serviceName string, methodName string, handler StreamStreamHandler) {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_server_register_stream_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterStreamStreamHandlerINSTANCE.Lower(handler), _uniffiStatus)
		return false
	})
}

// Register a stream-to-unary RPC handler
//
// # Arguments
// * `service_name` - The service name
// * `method_name` - The method name
// * `handler` - Implementation of the StreamUnaryHandler trait
func (_self *Server) RegisterStreamUnary(serviceName string, methodName string, handler StreamUnaryHandler) {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_server_register_stream_unary(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterStreamUnaryHandlerINSTANCE.Lower(handler), _uniffiStatus)
		return false
	})
}

// Register a unary-to-stream RPC handler
//
// # Arguments
// * `service_name` - The service name
// * `method_name` - The method name
// * `handler` - Implementation of the UnaryStreamHandler trait
func (_self *Server) RegisterUnaryStream(serviceName string, methodName string, handler UnaryStreamHandler) {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_server_register_unary_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterUnaryStreamHandlerINSTANCE.Lower(handler), _uniffiStatus)
		return false
	})
}

// Register a unary-to-unary RPC handler
//
// # Arguments
// * `service_name` - The service name (e.g., "MyService")
// * `method_name` - The method name (e.g., "GetUser")
// * `handler` - Implementation of the UnaryUnaryHandler trait
func (_self *Server) RegisterUnaryUnary(serviceName string, methodName string, handler UnaryUnaryHandler) {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_server_register_unary_unary(
			_pointer, FfiConverterStringINSTANCE.Lower(serviceName), FfiConverterStringINSTANCE.Lower(methodName), FfiConverterUnaryUnaryHandlerINSTANCE.Lower(handler), _uniffiStatus)
		return false
	})
}

// Start serving RPC requests (blocking version)
//
// This is a blocking method that runs until the server is shut down.
// It listens for incoming RPC calls and dispatches them to registered handlers.
func (_self *Server) Serve() error {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[RpcError](FfiConverterRpcError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_server_serve(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Start serving RPC requests (async version)
//
// This is an async method that runs until the server is shut down.
// It listens for incoming RPC calls and dispatches them to registered handlers.
func (_self *Server) ServeAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_server_serve_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Shutdown the server gracefully (blocking version)
//
// This signals the server to stop accepting new requests and wait for
// in-flight requests to complete.
func (_self *Server) Shutdown() {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_server_shutdown(
			_pointer, _uniffiStatus)
		return false
	})
}

// Shutdown the server gracefully (async version)
//
// This signals the server to stop accepting new requests and wait for
// in-flight requests to complete.
func (_self *Server) ShutdownAsync() {
	_pointer := _self.ffiObject.incrementPointer("*Server")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_server_shutdown_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

}
func (object *Server) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterServer struct{}

var FfiConverterServerINSTANCE = FfiConverterServer{}

func (c FfiConverterServer) Lift(pointer unsafe.Pointer) *Server {
	result := &Server{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_server(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_server(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Server).Destroy)
	return result
}

func (c FfiConverterServer) Read(reader io.Reader) *Server {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterServer) Lower(value *Server) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*Server")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterServer) Write(writer io.Writer, value *Server) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerServer struct{}

func (_ FfiDestroyerServer) Destroy(value *Server) {
	value.Destroy()
}

// Service wrapper for uniffi bindings
type ServiceInterface interface {
	// Get the service configuration
	Config() ServiceConfig
	// Connect to a remote endpoint as a client - blocking version
	Connect(config ClientConfig) (uint64, error)
	// Connect to a remote endpoint as a client
	ConnectAsync(config ClientConfig) (uint64, error)
	// Create a new App with authentication configuration (blocking version)
	//
	// This method initializes authentication providers/verifiers and creates a App
	// on this service instance. This is a blocking wrapper around create_app_async.
	//
	// # Arguments
	// * `base_name` - The base name for the app (without ID)
	// * `identity_provider_config` - Configuration for proving identity to others
	// * `identity_verifier_config` - Configuration for verifying identity of others
	//
	// # Returns
	// * `Ok(Arc<App>)` - Successfully created adapter
	// * `Err(SlimError)` - If adapter creation fails
	CreateApp(baseName *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig) (*App, error)
	// Create a new App with authentication configuration (async version)
	//
	// This method initializes authentication providers/verifiers and creates a App
	// on this service instance.
	//
	// # Arguments
	// * `base_name` - The base name for the app (without ID)
	// * `identity_provider_config` - Configuration for proving identity to others
	// * `identity_verifier_config` - Configuration for verifying identity of others
	//
	// # Returns
	// * `Ok(Arc<App>)` - Successfully created adapter
	// * `Err(SlimError)` - If adapter creation fails
	CreateAppAsync(baseName *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig) (*App, error)
	// Create a new App with authentication configuration and traffic direction (blocking version)
	//
	// This method initializes authentication providers/verifiers and creates an App
	// on this service instance. The direction parameter controls whether the app
	// can send messages, receive messages, both, or neither.
	//
	// # Arguments
	// * `base_name` - The base name for the app (without ID)
	// * `identity_provider_config` - Configuration for proving identity to others
	// * `identity_verifier_config` - Configuration for verifying identity of others
	// * `direction` - Traffic direction: Send, Recv, Bidirectional, or None
	//
	// # Returns
	// * `Ok(Arc<App>)` - Successfully created adapter
	// * `Err(SlimError)` - If adapter creation fails
	CreateAppWithDirection(baseName *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig, direction Direction) (*App, error)
	// Create a new App with authentication configuration and traffic direction (async version)
	//
	// This method initializes authentication providers/verifiers and creates an App
	// on this service instance. The direction parameter controls whether the app
	// can send messages, receive messages, both, or neither.
	//
	// # Arguments
	// * `base_name` - The base name for the app (without ID)
	// * `identity_provider_config` - Configuration for proving identity to others
	// * `identity_verifier_config` - Configuration for verifying identity of others
	// * `direction` - Traffic direction: Send, Recv, Bidirectional, or None
	//
	// # Returns
	// * `Ok(Arc<App>)` - Successfully created adapter
	// * `Err(SlimError)` - If adapter creation fails
	CreateAppWithDirectionAsync(name *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig, direction Direction) (*App, error)
	// Create a new App with SharedSecret authentication (helper function)
	//
	// This is a convenience function for creating a SLIM application using SharedSecret authentication
	// on this service instance.
	//
	// # Arguments
	// * `name` - The base name for the app (without ID)
	// * `secret` - The shared secret string for authentication
	//
	// # Returns
	// * `Ok(Arc<App>)` - Successfully created app
	// * `Err(SlimError)` - If app creation fails
	CreateAppWithSecret(name *Name, secret string) (*App, error)
	// Create a new App with SharedSecret authentication (async version)
	//
	// This is a convenience function for creating a SLIM application using SharedSecret authentication
	// on this service instance. This is the async version.
	//
	// # Arguments
	// * `name` - The base name for the app (without ID)
	// * `secret` - The shared secret string for authentication
	//
	// # Returns
	// * `Ok(Arc<App>)` - Successfully created app
	// * `Err(SlimError)` - If app creation fails
	CreateAppWithSecretAsync(name *Name, secret string) (*App, error)
	// Disconnect a client connection by connection ID - blocking version
	Disconnect(connId uint64) error
	// Get the connection ID for a given endpoint
	GetConnectionId(endpoint string) *uint64
	// Get the service identifier/name
	GetName() string
	// Run the service (starts all configured servers and clients) - blocking version
	Run() error
	// Run the service (starts all configured servers and clients)
	RunAsync() error
	// Start a server with the given configuration - blocking version
	RunServer(config ServerConfig) error
	// Start a server with the given configuration
	RunServerAsync(config ServerConfig) error
	// Shutdown the service gracefully - blocking version
	Shutdown() error
	// Shutdown the service gracefully
	ShutdownAsync() error
	// Stop a server by endpoint - blocking version
	StopServer(endpoint string) error
}

// Service wrapper for uniffi bindings
type Service struct {
	ffiObject FfiObject
}

// Create a new Service with the given name
func NewService(name string) *Service {
	return FfiConverterServiceINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_service_new(FfiConverterStringINSTANCE.Lower(name), _uniffiStatus)
	}))
}

// Create a new Service with configuration
func ServiceNewWithConfig(name string, config ServiceConfig) *Service {
	return FfiConverterServiceINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_constructor_service_new_with_config(FfiConverterStringINSTANCE.Lower(name), FfiConverterServiceConfigINSTANCE.Lower(config), _uniffiStatus)
	}))
}

// Get the service configuration
func (_self *Service) Config() ServiceConfig {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterServiceConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_service_config(
				_pointer, _uniffiStatus),
		}
	}))
}

// Connect to a remote endpoint as a client - blocking version
func (_self *Service) Connect(config ClientConfig) (uint64, error) {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_slim_bindings_fn_method_service_connect(
			_pointer, FfiConverterClientConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue uint64
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterUint64INSTANCE.Lift(_uniffiRV), nil
	}
}

// Connect to a remote endpoint as a client
func (_self *Service) ConnectAsync(config ClientConfig) (uint64, error) {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_slim_bindings_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) uint64 {
			return FfiConverterUint64INSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_service_connect_async(
			_pointer, FfiConverterClientConfigINSTANCE.Lower(config)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Create a new App with authentication configuration (blocking version)
//
// This method initializes authentication providers/verifiers and creates a App
// on this service instance. This is a blocking wrapper around create_app_async.
//
// # Arguments
// * `base_name` - The base name for the app (without ID)
// * `identity_provider_config` - Configuration for proving identity to others
// * `identity_verifier_config` - Configuration for verifying identity of others
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created adapter
// * `Err(SlimError)` - If adapter creation fails
func (_self *Service) CreateApp(baseName *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig) (*App, error) {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_service_create_app(
			_pointer, FfiConverterNameINSTANCE.Lower(baseName), FfiConverterIdentityProviderConfigINSTANCE.Lower(identityProviderConfig), FfiConverterIdentityVerifierConfigINSTANCE.Lower(identityVerifierConfig), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *App
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAppINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new App with authentication configuration (async version)
//
// This method initializes authentication providers/verifiers and creates a App
// on this service instance.
//
// # Arguments
// * `base_name` - The base name for the app (without ID)
// * `identity_provider_config` - Configuration for proving identity to others
// * `identity_verifier_config` - Configuration for verifying identity of others
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created adapter
// * `Err(SlimError)` - If adapter creation fails
func (_self *Service) CreateAppAsync(baseName *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig) (*App, error) {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *App {
			return FfiConverterAppINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_service_create_app_async(
			_pointer, FfiConverterNameINSTANCE.Lower(baseName), FfiConverterIdentityProviderConfigINSTANCE.Lower(identityProviderConfig), FfiConverterIdentityVerifierConfigINSTANCE.Lower(identityVerifierConfig)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Create a new App with authentication configuration and traffic direction (blocking version)
//
// This method initializes authentication providers/verifiers and creates an App
// on this service instance. The direction parameter controls whether the app
// can send messages, receive messages, both, or neither.
//
// # Arguments
// * `base_name` - The base name for the app (without ID)
// * `identity_provider_config` - Configuration for proving identity to others
// * `identity_verifier_config` - Configuration for verifying identity of others
// * `direction` - Traffic direction: Send, Recv, Bidirectional, or None
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created adapter
// * `Err(SlimError)` - If adapter creation fails
func (_self *Service) CreateAppWithDirection(baseName *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig, direction Direction) (*App, error) {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_service_create_app_with_direction(
			_pointer, FfiConverterNameINSTANCE.Lower(baseName), FfiConverterIdentityProviderConfigINSTANCE.Lower(identityProviderConfig), FfiConverterIdentityVerifierConfigINSTANCE.Lower(identityVerifierConfig), FfiConverterDirectionINSTANCE.Lower(direction), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *App
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAppINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new App with authentication configuration and traffic direction (async version)
//
// This method initializes authentication providers/verifiers and creates an App
// on this service instance. The direction parameter controls whether the app
// can send messages, receive messages, both, or neither.
//
// # Arguments
// * `base_name` - The base name for the app (without ID)
// * `identity_provider_config` - Configuration for proving identity to others
// * `identity_verifier_config` - Configuration for verifying identity of others
// * `direction` - Traffic direction: Send, Recv, Bidirectional, or None
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created adapter
// * `Err(SlimError)` - If adapter creation fails
func (_self *Service) CreateAppWithDirectionAsync(name *Name, identityProviderConfig IdentityProviderConfig, identityVerifierConfig IdentityVerifierConfig, direction Direction) (*App, error) {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *App {
			return FfiConverterAppINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_service_create_app_with_direction_async(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterIdentityProviderConfigINSTANCE.Lower(identityProviderConfig), FfiConverterIdentityVerifierConfigINSTANCE.Lower(identityVerifierConfig), FfiConverterDirectionINSTANCE.Lower(direction)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Create a new App with SharedSecret authentication (helper function)
//
// This is a convenience function for creating a SLIM application using SharedSecret authentication
// on this service instance.
//
// # Arguments
// * `name` - The base name for the app (without ID)
// * `secret` - The shared secret string for authentication
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created app
// * `Err(SlimError)` - If app creation fails
func (_self *Service) CreateAppWithSecret(name *Name, secret string) (*App, error) {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_service_create_app_with_secret(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterStringINSTANCE.Lower(secret), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *App
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAppINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new App with SharedSecret authentication (async version)
//
// This is a convenience function for creating a SLIM application using SharedSecret authentication
// on this service instance. This is the async version.
//
// # Arguments
// * `name` - The base name for the app (without ID)
// * `secret` - The shared secret string for authentication
//
// # Returns
// * `Ok(Arc<App>)` - Successfully created app
// * `Err(SlimError)` - If app creation fails
func (_self *Service) CreateAppWithSecretAsync(name *Name, secret string) (*App, error) {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *App {
			return FfiConverterAppINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_service_create_app_with_secret_async(
			_pointer, FfiConverterNameINSTANCE.Lower(name), FfiConverterStringINSTANCE.Lower(secret)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Disconnect a client connection by connection ID - blocking version
func (_self *Service) Disconnect(connId uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_service_disconnect(
			_pointer, FfiConverterUint64INSTANCE.Lower(connId), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Get the connection ID for a given endpoint
func (_self *Service) GetConnectionId(endpoint string) *uint64 {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_service_get_connection_id(
				_pointer, FfiConverterStringINSTANCE.Lower(endpoint), _uniffiStatus),
		}
	}))
}

// Get the service identifier/name
func (_self *Service) GetName() string {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_service_get_name(
				_pointer, _uniffiStatus),
		}
	}))
}

// Run the service (starts all configured servers and clients) - blocking version
func (_self *Service) Run() error {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_service_run(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Run the service (starts all configured servers and clients)
func (_self *Service) RunAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_service_run_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Start a server with the given configuration - blocking version
func (_self *Service) RunServer(config ServerConfig) error {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_service_run_server(
			_pointer, FfiConverterServerConfigINSTANCE.Lower(config), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Start a server with the given configuration
func (_self *Service) RunServerAsync(config ServerConfig) error {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_service_run_server_async(
			_pointer, FfiConverterServerConfigINSTANCE.Lower(config)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Shutdown the service gracefully - blocking version
func (_self *Service) Shutdown() error {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_service_shutdown(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Shutdown the service gracefully
func (_self *Service) ShutdownAsync() error {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_service_shutdown_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Stop a server by endpoint - blocking version
func (_self *Service) StopServer(endpoint string) error {
	_pointer := _self.ffiObject.incrementPointer("*Service")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_service_stop_server(
			_pointer, FfiConverterStringINSTANCE.Lower(endpoint), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *Service) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterService struct{}

var FfiConverterServiceINSTANCE = FfiConverterService{}

func (c FfiConverterService) Lift(pointer unsafe.Pointer) *Service {
	result := &Service{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_service(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_service(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Service).Destroy)
	return result
}

func (c FfiConverterService) Read(reader io.Reader) *Service {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterService) Lower(value *Service) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*Service")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterService) Write(writer io.Writer, value *Service) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerService struct{}

func (_ FfiDestroyerService) Destroy(value *Service) {
	value.Destroy()
}

// Session context for language bindings (UniFFI-compatible)
//
// Wraps the session context with proper async access patterns for message reception.
// Provides both synchronous (blocking) and asynchronous methods for FFI compatibility.
type SessionInterface interface {
	// Get the session configuration
	Config() (SessionConfig, error)
	// Get the destination name for this session
	Destination() (*Name, error)
	// Receive a message from the session (blocking version for FFI)
	//
	// # Arguments
	// * `timeout` - Optional timeout duration
	//
	// # Returns
	// * `Ok(ReceivedMessage)` - Message with context and payload bytes
	// * `Err(SlimError)` - If the receive fails or times out
	GetMessage(timeout *time.Duration) (ReceivedMessage, error)
	// Receive a message from the session (async version)
	GetMessageAsync(timeout *time.Duration) (ReceivedMessage, error)
	// Invite a participant to the session (blocking version)
	//
	// Returns a completion handle that can be awaited to ensure the invitation completes.
	Invite(participant *Name) (*CompletionHandle, error)
	// Invite a participant and wait for completion (blocking version)
	//
	// This method invites a participant and blocks until the invitation completes.
	InviteAndWait(participant *Name) error
	// Invite a participant and wait for completion (async version)
	//
	// This method invites a participant and waits until the invitation completes.
	InviteAndWaitAsync(participant *Name) error
	// Invite a participant to the session (async version)
	//
	// Returns a completion handle that can be awaited to ensure the invitation completes.
	InviteAsync(participant *Name) (*CompletionHandle, error)
	// Check if this session is the initiator
	IsInitiator() (bool, error)
	// Get the session metadata
	Metadata() (map[string]string, error)
	// Get list of participants in the session (blocking version for FFI)
	ParticipantsList() ([]*Name, error)
	// Get list of participants in the session
	ParticipantsListAsync() ([]*Name, error)
	// Publish a message to the session's destination (blocking version)
	//
	// Returns a completion handle that can be awaited to ensure the message was delivered.
	//
	// # Arguments
	// * `data` - The message payload bytes
	// * `payload_type` - Optional content type identifier
	// * `metadata` - Optional key-value metadata pairs
	//
	// # Returns
	// * `Ok(CompletionHandle)` - Handle to await delivery confirmation
	// * `Err(SlimError)` - If publishing fails
	//
	// # Example
	// ```ignore
	// let completion = session.publish(data, None, None)?;
	// completion.wait()?; // Blocks until message is delivered
	// ```
	Publish(data []byte, payloadType *string, metadata *map[string]string) (*CompletionHandle, error)
	// Publish a message and wait for completion (blocking version)
	//
	// This method publishes a message and blocks until the delivery completes.
	PublishAndWait(data []byte, payloadType *string, metadata *map[string]string) error
	// Publish a message and wait for completion (async version)
	//
	// This method publishes a message and waits until the delivery completes.
	PublishAndWaitAsync(data []byte, payloadType *string, metadata *map[string]string) error
	// Publish a message to the session's destination (async version)
	//
	// Returns a completion handle that can be awaited to ensure the message was delivered.
	PublishAsync(data []byte, payloadType *string, metadata *map[string]string) (*CompletionHandle, error)
	// Publish a reply message to the originator of a received message (blocking version for FFI)
	//
	// This method uses the routing information from a previously received message
	// to send a reply back to the sender. This is the preferred way to implement
	// request/reply patterns.
	//
	// Returns a completion handle that can be awaited to ensure the message was delivered.
	//
	// # Arguments
	// * `message_context` - Context from a message received via `get_message()`
	// * `data` - The reply payload bytes
	// * `payload_type` - Optional content type identifier
	// * `metadata` - Optional key-value metadata pairs
	//
	// # Returns
	// * `Ok(CompletionHandle)` - Handle to await delivery confirmation
	// * `Err(SlimError)` - If publishing fails
	PublishTo(messageContext MessageContext, data []byte, payloadType *string, metadata *map[string]string) (*CompletionHandle, error)
	// Publish a reply message and wait for completion (blocking version)
	//
	// This method publishes a reply to a received message and blocks until the delivery completes.
	PublishToAndWait(messageContext MessageContext, data []byte, payloadType *string, metadata *map[string]string) error
	// Publish a reply message and wait for completion (async version)
	//
	// This method publishes a reply to a received message and waits until the delivery completes.
	PublishToAndWaitAsync(messageContext MessageContext, data []byte, payloadType *string, metadata *map[string]string) error
	// Publish a reply message (async version)
	//
	// Returns a completion handle that can be awaited to ensure the message was delivered.
	PublishToAsync(messageContext MessageContext, data []byte, payloadType *string, metadata *map[string]string) (*CompletionHandle, error)
	// Low-level publish with full control over all parameters (blocking version for FFI)
	//
	// This is an advanced method that provides complete control over routing and delivery.
	// Most users should use `publish()` or `publish_to()` instead.
	//
	// # Arguments
	// * `destination` - Target name to send to
	// * `fanout` - Number of copies to send (for multicast)
	// * `data` - The message payload bytes
	// * `connection_out` - Optional specific connection ID to use
	// * `payload_type` - Optional content type identifier
	// * `metadata` - Optional key-value metadata pairs
	PublishWithParams(destination *Name, fanout uint32, data []byte, connectionOut *uint64, payloadType *string, metadata *map[string]string) error
	// Low-level publish with full control (async version)
	PublishWithParamsAsync(destination *Name, fanout uint32, data []byte, connectionOut *uint64, payloadType *string, metadata *map[string]string) error
	// Remove a participant from the session (blocking version)
	//
	// Returns a completion handle that can be awaited to ensure the removal completes.
	Remove(participant *Name) (*CompletionHandle, error)
	// Remove a participant and wait for completion (blocking version)
	//
	// This method removes a participant and blocks until the removal completes.
	RemoveAndWait(participant *Name) error
	// Remove a participant and wait for completion (async version)
	//
	// This method removes a participant and waits until the removal completes.
	RemoveAndWaitAsync(participant *Name) error
	// Remove a participant from the session (async version)
	//
	// Returns a completion handle that can be awaited to ensure the removal completes.
	RemoveAsync(participant *Name) (*CompletionHandle, error)
	// Get the session ID
	SessionId() (uint32, error)
	// Get the session type (PointToPoint or Group)
	SessionType() (SessionType, error)
	// Get the source name for this session
	Source() (*Name, error)
}

// Session context for language bindings (UniFFI-compatible)
//
// Wraps the session context with proper async access patterns for message reception.
// Provides both synchronous (blocking) and asynchronous methods for FFI compatibility.
type Session struct {
	ffiObject FfiObject
}

// Get the session configuration
func (_self *Session) Config() (SessionConfig, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_session_config(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue SessionConfig
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSessionConfigINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the destination name for this session
func (_self *Session) Destination() (*Name, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_session_destination(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Name
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterNameINSTANCE.Lift(_uniffiRV), nil
	}
}

// Receive a message from the session (blocking version for FFI)
//
// # Arguments
// * `timeout` - Optional timeout duration
//
// # Returns
// * `Ok(ReceivedMessage)` - Message with context and payload bytes
// * `Err(SlimError)` - If the receive fails or times out
func (_self *Session) GetMessage(timeout *time.Duration) (ReceivedMessage, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_session_get_message(
				_pointer, FfiConverterOptionalDurationINSTANCE.Lower(timeout), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue ReceivedMessage
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterReceivedMessageINSTANCE.Lift(_uniffiRV), nil
	}
}

// Receive a message from the session (async version)
func (_self *Session) GetMessageAsync(timeout *time.Duration) (ReceivedMessage, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) ReceivedMessage {
			return FfiConverterReceivedMessageINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_session_get_message_async(
			_pointer, FfiConverterOptionalDurationINSTANCE.Lower(timeout)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Invite a participant to the session (blocking version)
//
// Returns a completion handle that can be awaited to ensure the invitation completes.
func (_self *Session) Invite(participant *Name) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_session_invite(
			_pointer, FfiConverterNameINSTANCE.Lower(participant), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *CompletionHandle
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterCompletionHandleINSTANCE.Lift(_uniffiRV), nil
	}
}

// Invite a participant and wait for completion (blocking version)
//
// This method invites a participant and blocks until the invitation completes.
func (_self *Session) InviteAndWait(participant *Name) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_session_invite_and_wait(
			_pointer, FfiConverterNameINSTANCE.Lower(participant), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Invite a participant and wait for completion (async version)
//
// This method invites a participant and waits until the invitation completes.
func (_self *Session) InviteAndWaitAsync(participant *Name) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_session_invite_and_wait_async(
			_pointer, FfiConverterNameINSTANCE.Lower(participant)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Invite a participant to the session (async version)
//
// Returns a completion handle that can be awaited to ensure the invitation completes.
func (_self *Session) InviteAsync(participant *Name) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *CompletionHandle {
			return FfiConverterCompletionHandleINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_session_invite_async(
			_pointer, FfiConverterNameINSTANCE.Lower(participant)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check if this session is the initiator
func (_self *Session) IsInitiator() (bool, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_slim_bindings_fn_method_session_is_initiator(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue bool
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterBoolINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the session metadata
func (_self *Session) Metadata() (map[string]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_session_metadata(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue map[string]string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMapStringStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get list of participants in the session (blocking version for FFI)
func (_self *Session) ParticipantsList() ([]*Name, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_session_participants_list(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []*Name
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSequenceNameINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get list of participants in the session
func (_self *Session) ParticipantsListAsync() ([]*Name, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []*Name {
			return FfiConverterSequenceNameINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_session_participants_list_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Publish a message to the session's destination (blocking version)
//
// Returns a completion handle that can be awaited to ensure the message was delivered.
//
// # Arguments
// * `data` - The message payload bytes
// * `payload_type` - Optional content type identifier
// * `metadata` - Optional key-value metadata pairs
//
// # Returns
// * `Ok(CompletionHandle)` - Handle to await delivery confirmation
// * `Err(SlimError)` - If publishing fails
//
// # Example
// ```ignore
// let completion = session.publish(data, None, None)?;
// completion.wait()?; // Blocks until message is delivered
// ```
func (_self *Session) Publish(data []byte, payloadType *string, metadata *map[string]string) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_session_publish(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *CompletionHandle
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterCompletionHandleINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a message and wait for completion (blocking version)
//
// This method publishes a message and blocks until the delivery completes.
func (_self *Session) PublishAndWait(data []byte, payloadType *string, metadata *map[string]string) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_session_publish_and_wait(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Publish a message and wait for completion (async version)
//
// This method publishes a message and waits until the delivery completes.
func (_self *Session) PublishAndWaitAsync(data []byte, payloadType *string, metadata *map[string]string) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_session_publish_and_wait_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Publish a message to the session's destination (async version)
//
// Returns a completion handle that can be awaited to ensure the message was delivered.
func (_self *Session) PublishAsync(data []byte, payloadType *string, metadata *map[string]string) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *CompletionHandle {
			return FfiConverterCompletionHandleINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_session_publish_async(
			_pointer, FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Publish a reply message to the originator of a received message (blocking version for FFI)
//
// This method uses the routing information from a previously received message
// to send a reply back to the sender. This is the preferred way to implement
// request/reply patterns.
//
// Returns a completion handle that can be awaited to ensure the message was delivered.
//
// # Arguments
// * `message_context` - Context from a message received via `get_message()`
// * `data` - The reply payload bytes
// * `payload_type` - Optional content type identifier
// * `metadata` - Optional key-value metadata pairs
//
// # Returns
// * `Ok(CompletionHandle)` - Handle to await delivery confirmation
// * `Err(SlimError)` - If publishing fails
func (_self *Session) PublishTo(messageContext MessageContext, data []byte, payloadType *string, metadata *map[string]string) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_session_publish_to(
			_pointer, FfiConverterMessageContextINSTANCE.Lower(messageContext), FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *CompletionHandle
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterCompletionHandleINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a reply message and wait for completion (blocking version)
//
// This method publishes a reply to a received message and blocks until the delivery completes.
func (_self *Session) PublishToAndWait(messageContext MessageContext, data []byte, payloadType *string, metadata *map[string]string) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_session_publish_to_and_wait(
			_pointer, FfiConverterMessageContextINSTANCE.Lower(messageContext), FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Publish a reply message and wait for completion (async version)
//
// This method publishes a reply to a received message and waits until the delivery completes.
func (_self *Session) PublishToAndWaitAsync(messageContext MessageContext, data []byte, payloadType *string, metadata *map[string]string) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_session_publish_to_and_wait_async(
			_pointer, FfiConverterMessageContextINSTANCE.Lower(messageContext), FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Publish a reply message (async version)
//
// Returns a completion handle that can be awaited to ensure the message was delivered.
func (_self *Session) PublishToAsync(messageContext MessageContext, data []byte, payloadType *string, metadata *map[string]string) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *CompletionHandle {
			return FfiConverterCompletionHandleINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_session_publish_to_async(
			_pointer, FfiConverterMessageContextINSTANCE.Lower(messageContext), FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Low-level publish with full control over all parameters (blocking version for FFI)
//
// This is an advanced method that provides complete control over routing and delivery.
// Most users should use `publish()` or `publish_to()` instead.
//
// # Arguments
// * `destination` - Target name to send to
// * `fanout` - Number of copies to send (for multicast)
// * `data` - The message payload bytes
// * `connection_out` - Optional specific connection ID to use
// * `payload_type` - Optional content type identifier
// * `metadata` - Optional key-value metadata pairs
func (_self *Session) PublishWithParams(destination *Name, fanout uint32, data []byte, connectionOut *uint64, payloadType *string, metadata *map[string]string) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_session_publish_with_params(
			_pointer, FfiConverterNameINSTANCE.Lower(destination), FfiConverterUint32INSTANCE.Lower(fanout), FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalUint64INSTANCE.Lower(connectionOut), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Low-level publish with full control (async version)
func (_self *Session) PublishWithParamsAsync(destination *Name, fanout uint32, data []byte, connectionOut *uint64, payloadType *string, metadata *map[string]string) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_session_publish_with_params_async(
			_pointer, FfiConverterNameINSTANCE.Lower(destination), FfiConverterUint32INSTANCE.Lower(fanout), FfiConverterBytesINSTANCE.Lower(data), FfiConverterOptionalUint64INSTANCE.Lower(connectionOut), FfiConverterOptionalStringINSTANCE.Lower(payloadType), FfiConverterOptionalMapStringStringINSTANCE.Lower(metadata)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Remove a participant from the session (blocking version)
//
// Returns a completion handle that can be awaited to ensure the removal completes.
func (_self *Session) Remove(participant *Name) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_session_remove(
			_pointer, FfiConverterNameINSTANCE.Lower(participant), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *CompletionHandle
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterCompletionHandleINSTANCE.Lift(_uniffiRV), nil
	}
}

// Remove a participant and wait for completion (blocking version)
//
// This method removes a participant and blocks until the removal completes.
func (_self *Session) RemoveAndWait(participant *Name) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_method_session_remove_and_wait(
			_pointer, FfiConverterNameINSTANCE.Lower(participant), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Remove a participant and wait for completion (async version)
//
// This method removes a participant and waits until the removal completes.
func (_self *Session) RemoveAndWaitAsync(participant *Name) error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_session_remove_and_wait_async(
			_pointer, FfiConverterNameINSTANCE.Lower(participant)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Remove a participant from the session (async version)
//
// Returns a completion handle that can be awaited to ensure the removal completes.
func (_self *Session) RemoveAsync(participant *Name) (*CompletionHandle, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[SlimError](
		FfiConverterSlimErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) unsafe.Pointer {
			res := C.ffi_slim_bindings_rust_future_complete_pointer(handle, status)
			return res
		},
		// liftFn
		func(ffi unsafe.Pointer) *CompletionHandle {
			return FfiConverterCompletionHandleINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_session_remove_async(
			_pointer, FfiConverterNameINSTANCE.Lower(participant)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_pointer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_pointer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the session ID
func (_self *Session) SessionId() (uint32, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.uniffi_slim_bindings_fn_method_session_session_id(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue uint32
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterUint32INSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the session type (PointToPoint or Group)
func (_self *Session) SessionType() (SessionType, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_method_session_session_type(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue SessionType
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSessionTypeINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the source name for this session
func (_self *Session) Source() (*Name, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_method_session_source(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Name
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterNameINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *Session) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterSession struct{}

var FfiConverterSessionINSTANCE = FfiConverterSession{}

func (c FfiConverterSession) Lift(pointer unsafe.Pointer) *Session {
	result := &Session{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_session(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_session(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Session).Destroy)
	return result
}

func (c FfiConverterSession) Read(reader io.Reader) *Session {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterSession) Lower(value *Session) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*Session")
	defer value.ffiObject.decrementPointer()
	return pointer

}

func (c FfiConverterSession) Write(writer io.Writer, value *Session) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerSession struct{}

func (_ FfiDestroyerSession) Destroy(value *Session) {
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
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_streamstreamhandler_handle(
			_pointer, FfiConverterRequestStreamINSTANCE.Lower(stream), FfiConverterContextINSTANCE.Lower(context), FfiConverterResponseSinkINSTANCE.Lower(sink)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
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

func (c FfiConverterStreamStreamHandler) Lift(pointer unsafe.Pointer) StreamStreamHandler {
	result := &StreamStreamHandlerImpl{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_streamstreamhandler(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_streamstreamhandler(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*StreamStreamHandlerImpl).Destroy)
	return result
}

func (c FfiConverterStreamStreamHandler) Read(reader io.Reader) StreamStreamHandler {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterStreamStreamHandler) Lower(value StreamStreamHandler) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := unsafe.Pointer(uintptr(c.handleMap.insert(value)))
	return pointer

}

func (c FfiConverterStreamStreamHandler) Write(writer io.Writer, value StreamStreamHandler) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerStreamStreamHandler struct{}

func (_ FfiDestroyerStreamStreamHandler) Destroy(value StreamStreamHandler) {
	if val, ok := value.(*StreamStreamHandlerImpl); ok {
		val.Destroy()
	} else {
		panic("Expected *StreamStreamHandlerImpl")
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
		handles: map[uint64]T{},
	}
}

func (cm *concurrentHandleMap[T]) insert(obj T) uint64 {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.currentHandle = cm.currentHandle + 1
	cm.handles[cm.currentHandle] = obj
	return cm.currentHandle
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

//export slim_bindings_cgo_dispatchCallbackInterfaceStreamStreamHandlerMethod0
func slim_bindings_cgo_dispatchCallbackInterfaceStreamStreamHandlerMethod0(uniffiHandle C.uint64_t, stream unsafe.Pointer, context unsafe.Pointer, sink unsafe.Pointer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutReturn *C.UniffiForeignFuture) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterStreamStreamHandlerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureStructVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutReturn = C.UniffiForeignFuture{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureFree(C.slim_bindings_uniffiFreeGorutine),
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
		asyncResult := &C.UniffiForeignFutureStructVoid{}
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
	handle: (C.UniffiCallbackInterfaceStreamStreamHandlerMethod0)(C.slim_bindings_cgo_dispatchCallbackInterfaceStreamStreamHandlerMethod0),

	uniffiFree: (C.UniffiCallbackInterfaceFree)(C.slim_bindings_cgo_dispatchCallbackInterfaceStreamStreamHandlerFree),
}

//export slim_bindings_cgo_dispatchCallbackInterfaceStreamStreamHandlerFree
func slim_bindings_cgo_dispatchCallbackInterfaceStreamStreamHandlerFree(handle C.uint64_t) {
	FfiConverterStreamStreamHandlerINSTANCE.handleMap.remove(uint64(handle))
}

func (c FfiConverterStreamStreamHandler) register() {
	C.uniffi_slim_bindings_fn_init_callback_vtable_streamstreamhandler(&UniffiVTableCallbackInterfaceStreamStreamHandlerINSTANCE)
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
	res, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_streamunaryhandler_handle(
			_pointer, FfiConverterRequestStreamINSTANCE.Lower(stream), FfiConverterContextINSTANCE.Lower(context)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
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

func (c FfiConverterStreamUnaryHandler) Lift(pointer unsafe.Pointer) StreamUnaryHandler {
	result := &StreamUnaryHandlerImpl{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_streamunaryhandler(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_streamunaryhandler(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*StreamUnaryHandlerImpl).Destroy)
	return result
}

func (c FfiConverterStreamUnaryHandler) Read(reader io.Reader) StreamUnaryHandler {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterStreamUnaryHandler) Lower(value StreamUnaryHandler) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := unsafe.Pointer(uintptr(c.handleMap.insert(value)))
	return pointer

}

func (c FfiConverterStreamUnaryHandler) Write(writer io.Writer, value StreamUnaryHandler) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerStreamUnaryHandler struct{}

func (_ FfiDestroyerStreamUnaryHandler) Destroy(value StreamUnaryHandler) {
	if val, ok := value.(*StreamUnaryHandlerImpl); ok {
		val.Destroy()
	} else {
		panic("Expected *StreamUnaryHandlerImpl")
	}
}

//export slim_bindings_cgo_dispatchCallbackInterfaceStreamUnaryHandlerMethod0
func slim_bindings_cgo_dispatchCallbackInterfaceStreamUnaryHandlerMethod0(uniffiHandle C.uint64_t, stream unsafe.Pointer, context unsafe.Pointer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutReturn *C.UniffiForeignFuture) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterStreamUnaryHandlerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureStructRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutReturn = C.UniffiForeignFuture{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureFree(C.slim_bindings_uniffiFreeGorutine),
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
		asyncResult := &C.UniffiForeignFutureStructRustBuffer{}
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
	handle: (C.UniffiCallbackInterfaceStreamUnaryHandlerMethod0)(C.slim_bindings_cgo_dispatchCallbackInterfaceStreamUnaryHandlerMethod0),

	uniffiFree: (C.UniffiCallbackInterfaceFree)(C.slim_bindings_cgo_dispatchCallbackInterfaceStreamUnaryHandlerFree),
}

//export slim_bindings_cgo_dispatchCallbackInterfaceStreamUnaryHandlerFree
func slim_bindings_cgo_dispatchCallbackInterfaceStreamUnaryHandlerFree(handle C.uint64_t) {
	FfiConverterStreamUnaryHandlerINSTANCE.handleMap.remove(uint64(handle))
}

func (c FfiConverterStreamUnaryHandler) register() {
	C.uniffi_slim_bindings_fn_init_callback_vtable_streamunaryhandler(&UniffiVTableCallbackInterfaceStreamUnaryHandlerINSTANCE)
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
	_, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_slim_bindings_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_slim_bindings_fn_method_unarystreamhandler_handle(
			_pointer, FfiConverterBytesINSTANCE.Lower(request), FfiConverterContextINSTANCE.Lower(context), FfiConverterResponseSinkINSTANCE.Lower(sink)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_void(handle)
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

func (c FfiConverterUnaryStreamHandler) Lift(pointer unsafe.Pointer) UnaryStreamHandler {
	result := &UnaryStreamHandlerImpl{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_unarystreamhandler(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_unarystreamhandler(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*UnaryStreamHandlerImpl).Destroy)
	return result
}

func (c FfiConverterUnaryStreamHandler) Read(reader io.Reader) UnaryStreamHandler {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterUnaryStreamHandler) Lower(value UnaryStreamHandler) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := unsafe.Pointer(uintptr(c.handleMap.insert(value)))
	return pointer

}

func (c FfiConverterUnaryStreamHandler) Write(writer io.Writer, value UnaryStreamHandler) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerUnaryStreamHandler struct{}

func (_ FfiDestroyerUnaryStreamHandler) Destroy(value UnaryStreamHandler) {
	if val, ok := value.(*UnaryStreamHandlerImpl); ok {
		val.Destroy()
	} else {
		panic("Expected *UnaryStreamHandlerImpl")
	}
}

//export slim_bindings_cgo_dispatchCallbackInterfaceUnaryStreamHandlerMethod0
func slim_bindings_cgo_dispatchCallbackInterfaceUnaryStreamHandlerMethod0(uniffiHandle C.uint64_t, request C.RustBuffer, context unsafe.Pointer, sink unsafe.Pointer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutReturn *C.UniffiForeignFuture) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterUnaryStreamHandlerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureStructVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutReturn = C.UniffiForeignFuture{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureFree(C.slim_bindings_uniffiFreeGorutine),
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
		asyncResult := &C.UniffiForeignFutureStructVoid{}
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
	handle: (C.UniffiCallbackInterfaceUnaryStreamHandlerMethod0)(C.slim_bindings_cgo_dispatchCallbackInterfaceUnaryStreamHandlerMethod0),

	uniffiFree: (C.UniffiCallbackInterfaceFree)(C.slim_bindings_cgo_dispatchCallbackInterfaceUnaryStreamHandlerFree),
}

//export slim_bindings_cgo_dispatchCallbackInterfaceUnaryStreamHandlerFree
func slim_bindings_cgo_dispatchCallbackInterfaceUnaryStreamHandlerFree(handle C.uint64_t) {
	FfiConverterUnaryStreamHandlerINSTANCE.handleMap.remove(uint64(handle))
}

func (c FfiConverterUnaryStreamHandler) register() {
	C.uniffi_slim_bindings_fn_init_callback_vtable_unarystreamhandler(&UniffiVTableCallbackInterfaceUnaryStreamHandlerINSTANCE)
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
	res, err := uniffiRustCallAsync[RpcError](
		FfiConverterRpcErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_slim_bindings_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []byte {
			return FfiConverterBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_slim_bindings_fn_method_unaryunaryhandler_handle(
			_pointer, FfiConverterBytesINSTANCE.Lower(request), FfiConverterContextINSTANCE.Lower(context)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_slim_bindings_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_slim_bindings_rust_future_free_rust_buffer(handle)
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

func (c FfiConverterUnaryUnaryHandler) Lift(pointer unsafe.Pointer) UnaryUnaryHandler {
	result := &UnaryUnaryHandlerImpl{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) unsafe.Pointer {
				return C.uniffi_slim_bindings_fn_clone_unaryunaryhandler(pointer, status)
			},
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_slim_bindings_fn_free_unaryunaryhandler(pointer, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*UnaryUnaryHandlerImpl).Destroy)
	return result
}

func (c FfiConverterUnaryUnaryHandler) Read(reader io.Reader) UnaryUnaryHandler {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterUnaryUnaryHandler) Lower(value UnaryUnaryHandler) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := unsafe.Pointer(uintptr(c.handleMap.insert(value)))
	return pointer

}

func (c FfiConverterUnaryUnaryHandler) Write(writer io.Writer, value UnaryUnaryHandler) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerUnaryUnaryHandler struct{}

func (_ FfiDestroyerUnaryUnaryHandler) Destroy(value UnaryUnaryHandler) {
	if val, ok := value.(*UnaryUnaryHandlerImpl); ok {
		val.Destroy()
	} else {
		panic("Expected *UnaryUnaryHandlerImpl")
	}
}

//export slim_bindings_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerMethod0
func slim_bindings_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerMethod0(uniffiHandle C.uint64_t, request C.RustBuffer, context unsafe.Pointer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutReturn *C.UniffiForeignFuture) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterUnaryUnaryHandlerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureStructRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutReturn = C.UniffiForeignFuture{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureFree(C.slim_bindings_uniffiFreeGorutine),
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
		asyncResult := &C.UniffiForeignFutureStructRustBuffer{}
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
	handle: (C.UniffiCallbackInterfaceUnaryUnaryHandlerMethod0)(C.slim_bindings_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerMethod0),

	uniffiFree: (C.UniffiCallbackInterfaceFree)(C.slim_bindings_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerFree),
}

//export slim_bindings_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerFree
func slim_bindings_cgo_dispatchCallbackInterfaceUnaryUnaryHandlerFree(handle C.uint64_t) {
	FfiConverterUnaryUnaryHandlerINSTANCE.handleMap.remove(uint64(handle))
}

func (c FfiConverterUnaryUnaryHandler) register() {
	C.uniffi_slim_bindings_fn_init_callback_vtable_unaryunaryhandler(&UniffiVTableCallbackInterfaceUnaryUnaryHandlerINSTANCE)
}

// Basic authentication configuration
type BasicAuth struct {
	Username string
	Password string
}

func (r *BasicAuth) Destroy() {
	FfiDestroyerString{}.Destroy(r.Username)
	FfiDestroyerString{}.Destroy(r.Password)
}

type FfiConverterBasicAuth struct{}

var FfiConverterBasicAuthINSTANCE = FfiConverterBasicAuth{}

func (c FfiConverterBasicAuth) Lift(rb RustBufferI) BasicAuth {
	return LiftFromRustBuffer[BasicAuth](c, rb)
}

func (c FfiConverterBasicAuth) Read(reader io.Reader) BasicAuth {
	return BasicAuth{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterBasicAuth) Lower(value BasicAuth) C.RustBuffer {
	return LowerIntoRustBuffer[BasicAuth](c, value)
}

func (c FfiConverterBasicAuth) Write(writer io.Writer, value BasicAuth) {
	FfiConverterStringINSTANCE.Write(writer, value.Username)
	FfiConverterStringINSTANCE.Write(writer, value.Password)
}

type FfiDestroyerBasicAuth struct{}

func (_ FfiDestroyerBasicAuth) Destroy(value BasicAuth) {
	value.Destroy()
}

// Build information for the SLIM bindings
type BuildInfo struct {
	// Semantic version (e.g., "0.7.0")
	Version string
	// Git commit hash (short)
	GitSha string
	// Build date in ISO 8601 UTC format
	BuildDate string
	// Build profile (debug/release)
	Profile string
}

func (r *BuildInfo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Version)
	FfiDestroyerString{}.Destroy(r.GitSha)
	FfiDestroyerString{}.Destroy(r.BuildDate)
	FfiDestroyerString{}.Destroy(r.Profile)
}

type FfiConverterBuildInfo struct{}

var FfiConverterBuildInfoINSTANCE = FfiConverterBuildInfo{}

func (c FfiConverterBuildInfo) Lift(rb RustBufferI) BuildInfo {
	return LiftFromRustBuffer[BuildInfo](c, rb)
}

func (c FfiConverterBuildInfo) Read(reader io.Reader) BuildInfo {
	return BuildInfo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterBuildInfo) Lower(value BuildInfo) C.RustBuffer {
	return LowerIntoRustBuffer[BuildInfo](c, value)
}

func (c FfiConverterBuildInfo) Write(writer io.Writer, value BuildInfo) {
	FfiConverterStringINSTANCE.Write(writer, value.Version)
	FfiConverterStringINSTANCE.Write(writer, value.GitSha)
	FfiConverterStringINSTANCE.Write(writer, value.BuildDate)
	FfiConverterStringINSTANCE.Write(writer, value.Profile)
}

type FfiDestroyerBuildInfo struct{}

func (_ FfiDestroyerBuildInfo) Destroy(value BuildInfo) {
	value.Destroy()
}

// Client configuration for connecting to a SLIM server
type ClientConfig struct {
	// The target endpoint the client will connect to
	Endpoint string
	// Origin (HTTP Host authority override) for the client
	Origin *string
	// Optional TLS SNI server name override
	ServerName *string
	// Compression type
	Compression *CompressionType
	// Rate limit string (e.g., "100/s" for 100 requests per second)
	RateLimit *string
	// TLS client configuration
	Tls TlsClientConfig
	// Keepalive parameters
	Keepalive *KeepaliveConfig
	// HTTP Proxy configuration
	Proxy ProxyConfig
	// Connection timeout
	ConnectTimeout time.Duration
	// Request timeout
	RequestTimeout time.Duration
	// Read buffer size in bytes
	BufferSize *uint64
	// Headers associated with gRPC requests
	Headers map[string]string
	// Authentication configuration for outgoing RPCs
	Auth ClientAuthenticationConfig
	// Backoff retry configuration
	Backoff BackoffConfig
	// Arbitrary user-provided metadata as JSON string
	Metadata *string
}

func (r *ClientConfig) Destroy() {
	FfiDestroyerString{}.Destroy(r.Endpoint)
	FfiDestroyerOptionalString{}.Destroy(r.Origin)
	FfiDestroyerOptionalString{}.Destroy(r.ServerName)
	FfiDestroyerOptionalCompressionType{}.Destroy(r.Compression)
	FfiDestroyerOptionalString{}.Destroy(r.RateLimit)
	FfiDestroyerTlsClientConfig{}.Destroy(r.Tls)
	FfiDestroyerOptionalKeepaliveConfig{}.Destroy(r.Keepalive)
	FfiDestroyerProxyConfig{}.Destroy(r.Proxy)
	FfiDestroyerDuration{}.Destroy(r.ConnectTimeout)
	FfiDestroyerDuration{}.Destroy(r.RequestTimeout)
	FfiDestroyerOptionalUint64{}.Destroy(r.BufferSize)
	FfiDestroyerMapStringString{}.Destroy(r.Headers)
	FfiDestroyerClientAuthenticationConfig{}.Destroy(r.Auth)
	FfiDestroyerBackoffConfig{}.Destroy(r.Backoff)
	FfiDestroyerOptionalString{}.Destroy(r.Metadata)
}

type FfiConverterClientConfig struct{}

var FfiConverterClientConfigINSTANCE = FfiConverterClientConfig{}

func (c FfiConverterClientConfig) Lift(rb RustBufferI) ClientConfig {
	return LiftFromRustBuffer[ClientConfig](c, rb)
}

func (c FfiConverterClientConfig) Read(reader io.Reader) ClientConfig {
	return ClientConfig{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalCompressionTypeINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterTlsClientConfigINSTANCE.Read(reader),
		FfiConverterOptionalKeepaliveConfigINSTANCE.Read(reader),
		FfiConverterProxyConfigINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
		FfiConverterClientAuthenticationConfigINSTANCE.Read(reader),
		FfiConverterBackoffConfigINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterClientConfig) Lower(value ClientConfig) C.RustBuffer {
	return LowerIntoRustBuffer[ClientConfig](c, value)
}

func (c FfiConverterClientConfig) Write(writer io.Writer, value ClientConfig) {
	FfiConverterStringINSTANCE.Write(writer, value.Endpoint)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Origin)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ServerName)
	FfiConverterOptionalCompressionTypeINSTANCE.Write(writer, value.Compression)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.RateLimit)
	FfiConverterTlsClientConfigINSTANCE.Write(writer, value.Tls)
	FfiConverterOptionalKeepaliveConfigINSTANCE.Write(writer, value.Keepalive)
	FfiConverterProxyConfigINSTANCE.Write(writer, value.Proxy)
	FfiConverterDurationINSTANCE.Write(writer, value.ConnectTimeout)
	FfiConverterDurationINSTANCE.Write(writer, value.RequestTimeout)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.BufferSize)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Headers)
	FfiConverterClientAuthenticationConfigINSTANCE.Write(writer, value.Auth)
	FfiConverterBackoffConfigINSTANCE.Write(writer, value.Backoff)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Metadata)
}

type FfiDestroyerClientConfig struct{}

func (_ FfiDestroyerClientConfig) Destroy(value ClientConfig) {
	value.Destroy()
}

// JWT authentication configuration for client-side signing
type ClientJwtAuth struct {
	// JWT key configuration (encoding key for signing)
	Key JwtKeyType
	// JWT audience claims to include
	Audience *[]string
	// JWT issuer to include
	Issuer *string
	// JWT subject to include
	Subject *string
	// Token validity duration (default: 3600 seconds)
	Duration time.Duration
}

func (r *ClientJwtAuth) Destroy() {
	FfiDestroyerJwtKeyType{}.Destroy(r.Key)
	FfiDestroyerOptionalSequenceString{}.Destroy(r.Audience)
	FfiDestroyerOptionalString{}.Destroy(r.Issuer)
	FfiDestroyerOptionalString{}.Destroy(r.Subject)
	FfiDestroyerDuration{}.Destroy(r.Duration)
}

type FfiConverterClientJwtAuth struct{}

var FfiConverterClientJwtAuthINSTANCE = FfiConverterClientJwtAuth{}

func (c FfiConverterClientJwtAuth) Lift(rb RustBufferI) ClientJwtAuth {
	return LiftFromRustBuffer[ClientJwtAuth](c, rb)
}

func (c FfiConverterClientJwtAuth) Read(reader io.Reader) ClientJwtAuth {
	return ClientJwtAuth{
		FfiConverterJwtKeyTypeINSTANCE.Read(reader),
		FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
	}
}

func (c FfiConverterClientJwtAuth) Lower(value ClientJwtAuth) C.RustBuffer {
	return LowerIntoRustBuffer[ClientJwtAuth](c, value)
}

func (c FfiConverterClientJwtAuth) Write(writer io.Writer, value ClientJwtAuth) {
	FfiConverterJwtKeyTypeINSTANCE.Write(writer, value.Key)
	FfiConverterOptionalSequenceStringINSTANCE.Write(writer, value.Audience)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Issuer)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Subject)
	FfiConverterDurationINSTANCE.Write(writer, value.Duration)
}

type FfiDestroyerClientJwtAuth struct{}

func (_ FfiDestroyerClientJwtAuth) Destroy(value ClientJwtAuth) {
	value.Destroy()
}

// DataPlane configuration wrapper for uniffi bindings
type DataplaneConfig struct {
	// DataPlane GRPC server settings
	Servers []ServerConfig
	// DataPlane client configs
	Clients []ClientConfig
}

func (r *DataplaneConfig) Destroy() {
	FfiDestroyerSequenceServerConfig{}.Destroy(r.Servers)
	FfiDestroyerSequenceClientConfig{}.Destroy(r.Clients)
}

type FfiConverterDataplaneConfig struct{}

var FfiConverterDataplaneConfigINSTANCE = FfiConverterDataplaneConfig{}

func (c FfiConverterDataplaneConfig) Lift(rb RustBufferI) DataplaneConfig {
	return LiftFromRustBuffer[DataplaneConfig](c, rb)
}

func (c FfiConverterDataplaneConfig) Read(reader io.Reader) DataplaneConfig {
	return DataplaneConfig{
		FfiConverterSequenceServerConfigINSTANCE.Read(reader),
		FfiConverterSequenceClientConfigINSTANCE.Read(reader),
	}
}

func (c FfiConverterDataplaneConfig) Lower(value DataplaneConfig) C.RustBuffer {
	return LowerIntoRustBuffer[DataplaneConfig](c, value)
}

func (c FfiConverterDataplaneConfig) Write(writer io.Writer, value DataplaneConfig) {
	FfiConverterSequenceServerConfigINSTANCE.Write(writer, value.Servers)
	FfiConverterSequenceClientConfigINSTANCE.Write(writer, value.Clients)
}

type FfiDestroyerDataplaneConfig struct{}

func (_ FfiDestroyerDataplaneConfig) Destroy(value DataplaneConfig) {
	value.Destroy()
}

// Exponential backoff configuration
type ExponentialBackoff struct {
	// Base delay
	Base time.Duration
	// Multiplication factor for each retry
	Factor uint64
	// Maximum delay
	MaxDelay time.Duration
	// Maximum number of retry attempts
	MaxAttempts uint64
	// Whether to add random jitter to delays
	Jitter bool
}

func (r *ExponentialBackoff) Destroy() {
	FfiDestroyerDuration{}.Destroy(r.Base)
	FfiDestroyerUint64{}.Destroy(r.Factor)
	FfiDestroyerDuration{}.Destroy(r.MaxDelay)
	FfiDestroyerUint64{}.Destroy(r.MaxAttempts)
	FfiDestroyerBool{}.Destroy(r.Jitter)
}

type FfiConverterExponentialBackoff struct{}

var FfiConverterExponentialBackoffINSTANCE = FfiConverterExponentialBackoff{}

func (c FfiConverterExponentialBackoff) Lift(rb RustBufferI) ExponentialBackoff {
	return LiftFromRustBuffer[ExponentialBackoff](c, rb)
}

func (c FfiConverterExponentialBackoff) Read(reader io.Reader) ExponentialBackoff {
	return ExponentialBackoff{
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterExponentialBackoff) Lower(value ExponentialBackoff) C.RustBuffer {
	return LowerIntoRustBuffer[ExponentialBackoff](c, value)
}

func (c FfiConverterExponentialBackoff) Write(writer io.Writer, value ExponentialBackoff) {
	FfiConverterDurationINSTANCE.Write(writer, value.Base)
	FfiConverterUint64INSTANCE.Write(writer, value.Factor)
	FfiConverterDurationINSTANCE.Write(writer, value.MaxDelay)
	FfiConverterUint64INSTANCE.Write(writer, value.MaxAttempts)
	FfiConverterBoolINSTANCE.Write(writer, value.Jitter)
}

type FfiDestroyerExponentialBackoff struct{}

func (_ FfiDestroyerExponentialBackoff) Destroy(value ExponentialBackoff) {
	value.Destroy()
}

// Fixed interval backoff configuration
type FixedIntervalBackoff struct {
	// Fixed interval between retries
	Interval time.Duration
	// Maximum number of retry attempts
	MaxAttempts uint64
}

func (r *FixedIntervalBackoff) Destroy() {
	FfiDestroyerDuration{}.Destroy(r.Interval)
	FfiDestroyerUint64{}.Destroy(r.MaxAttempts)
}

type FfiConverterFixedIntervalBackoff struct{}

var FfiConverterFixedIntervalBackoffINSTANCE = FfiConverterFixedIntervalBackoff{}

func (c FfiConverterFixedIntervalBackoff) Lift(rb RustBufferI) FixedIntervalBackoff {
	return LiftFromRustBuffer[FixedIntervalBackoff](c, rb)
}

func (c FfiConverterFixedIntervalBackoff) Read(reader io.Reader) FixedIntervalBackoff {
	return FixedIntervalBackoff{
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterFixedIntervalBackoff) Lower(value FixedIntervalBackoff) C.RustBuffer {
	return LowerIntoRustBuffer[FixedIntervalBackoff](c, value)
}

func (c FfiConverterFixedIntervalBackoff) Write(writer io.Writer, value FixedIntervalBackoff) {
	FfiConverterDurationINSTANCE.Write(writer, value.Interval)
	FfiConverterUint64INSTANCE.Write(writer, value.MaxAttempts)
}

type FfiDestroyerFixedIntervalBackoff struct{}

func (_ FfiDestroyerFixedIntervalBackoff) Destroy(value FixedIntervalBackoff) {
	value.Destroy()
}

// JWT authentication configuration for server-side verification
type JwtAuth struct {
	// JWT key configuration (decoding key for verification)
	Key JwtKeyType
	// JWT audience claims to verify
	Audience *[]string
	// JWT issuer to verify
	Issuer *string
	// JWT subject to verify
	Subject *string
	// Token validity duration (default: 3600 seconds)
	Duration time.Duration
}

func (r *JwtAuth) Destroy() {
	FfiDestroyerJwtKeyType{}.Destroy(r.Key)
	FfiDestroyerOptionalSequenceString{}.Destroy(r.Audience)
	FfiDestroyerOptionalString{}.Destroy(r.Issuer)
	FfiDestroyerOptionalString{}.Destroy(r.Subject)
	FfiDestroyerDuration{}.Destroy(r.Duration)
}

type FfiConverterJwtAuth struct{}

var FfiConverterJwtAuthINSTANCE = FfiConverterJwtAuth{}

func (c FfiConverterJwtAuth) Lift(rb RustBufferI) JwtAuth {
	return LiftFromRustBuffer[JwtAuth](c, rb)
}

func (c FfiConverterJwtAuth) Read(reader io.Reader) JwtAuth {
	return JwtAuth{
		FfiConverterJwtKeyTypeINSTANCE.Read(reader),
		FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
	}
}

func (c FfiConverterJwtAuth) Lower(value JwtAuth) C.RustBuffer {
	return LowerIntoRustBuffer[JwtAuth](c, value)
}

func (c FfiConverterJwtAuth) Write(writer io.Writer, value JwtAuth) {
	FfiConverterJwtKeyTypeINSTANCE.Write(writer, value.Key)
	FfiConverterOptionalSequenceStringINSTANCE.Write(writer, value.Audience)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Issuer)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Subject)
	FfiConverterDurationINSTANCE.Write(writer, value.Duration)
}

type FfiDestroyerJwtAuth struct{}

func (_ FfiDestroyerJwtAuth) Destroy(value JwtAuth) {
	value.Destroy()
}

// JWT key configuration
type JwtKeyConfig struct {
	// Algorithm used for signing/verifying the JWT
	Algorithm JwtAlgorithm
	// Key format - PEM, JWK or JWKS
	Format JwtKeyFormat
	// Encoded key or file path
	Key JwtKeyData
}

func (r *JwtKeyConfig) Destroy() {
	FfiDestroyerJwtAlgorithm{}.Destroy(r.Algorithm)
	FfiDestroyerJwtKeyFormat{}.Destroy(r.Format)
	FfiDestroyerJwtKeyData{}.Destroy(r.Key)
}

type FfiConverterJwtKeyConfig struct{}

var FfiConverterJwtKeyConfigINSTANCE = FfiConverterJwtKeyConfig{}

func (c FfiConverterJwtKeyConfig) Lift(rb RustBufferI) JwtKeyConfig {
	return LiftFromRustBuffer[JwtKeyConfig](c, rb)
}

func (c FfiConverterJwtKeyConfig) Read(reader io.Reader) JwtKeyConfig {
	return JwtKeyConfig{
		FfiConverterJwtAlgorithmINSTANCE.Read(reader),
		FfiConverterJwtKeyFormatINSTANCE.Read(reader),
		FfiConverterJwtKeyDataINSTANCE.Read(reader),
	}
}

func (c FfiConverterJwtKeyConfig) Lower(value JwtKeyConfig) C.RustBuffer {
	return LowerIntoRustBuffer[JwtKeyConfig](c, value)
}

func (c FfiConverterJwtKeyConfig) Write(writer io.Writer, value JwtKeyConfig) {
	FfiConverterJwtAlgorithmINSTANCE.Write(writer, value.Algorithm)
	FfiConverterJwtKeyFormatINSTANCE.Write(writer, value.Format)
	FfiConverterJwtKeyDataINSTANCE.Write(writer, value.Key)
}

type FfiDestroyerJwtKeyConfig struct{}

func (_ FfiDestroyerJwtKeyConfig) Destroy(value JwtKeyConfig) {
	value.Destroy()
}

// Keepalive configuration for the client
type KeepaliveConfig struct {
	// TCP keepalive duration
	TcpKeepalive time.Duration
	// HTTP2 keepalive duration
	Http2Keepalive time.Duration
	// Keepalive timeout
	Timeout time.Duration
	// Whether to permit keepalive without an active stream
	KeepAliveWhileIdle bool
}

func (r *KeepaliveConfig) Destroy() {
	FfiDestroyerDuration{}.Destroy(r.TcpKeepalive)
	FfiDestroyerDuration{}.Destroy(r.Http2Keepalive)
	FfiDestroyerDuration{}.Destroy(r.Timeout)
	FfiDestroyerBool{}.Destroy(r.KeepAliveWhileIdle)
}

type FfiConverterKeepaliveConfig struct{}

var FfiConverterKeepaliveConfigINSTANCE = FfiConverterKeepaliveConfig{}

func (c FfiConverterKeepaliveConfig) Lift(rb RustBufferI) KeepaliveConfig {
	return LiftFromRustBuffer[KeepaliveConfig](c, rb)
}

func (c FfiConverterKeepaliveConfig) Read(reader io.Reader) KeepaliveConfig {
	return KeepaliveConfig{
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterKeepaliveConfig) Lower(value KeepaliveConfig) C.RustBuffer {
	return LowerIntoRustBuffer[KeepaliveConfig](c, value)
}

func (c FfiConverterKeepaliveConfig) Write(writer io.Writer, value KeepaliveConfig) {
	FfiConverterDurationINSTANCE.Write(writer, value.TcpKeepalive)
	FfiConverterDurationINSTANCE.Write(writer, value.Http2Keepalive)
	FfiConverterDurationINSTANCE.Write(writer, value.Timeout)
	FfiConverterBoolINSTANCE.Write(writer, value.KeepAliveWhileIdle)
}

type FfiDestroyerKeepaliveConfig struct{}

func (_ FfiDestroyerKeepaliveConfig) Destroy(value KeepaliveConfig) {
	value.Destroy()
}

// Keepalive configuration for the server
type KeepaliveServerParameters struct {
	// Max connection idle time (time after which an idle connection is closed)
	MaxConnectionIdle time.Duration
	// Max connection age (maximum time a connection may exist before being closed)
	MaxConnectionAge time.Duration
	// Max connection age grace (additional time after max_connection_age before closing)
	MaxConnectionAgeGrace time.Duration
	// Keepalive ping frequency
	Time time.Duration
	// Keepalive ping timeout (time to wait for ack)
	Timeout time.Duration
}

func (r *KeepaliveServerParameters) Destroy() {
	FfiDestroyerDuration{}.Destroy(r.MaxConnectionIdle)
	FfiDestroyerDuration{}.Destroy(r.MaxConnectionAge)
	FfiDestroyerDuration{}.Destroy(r.MaxConnectionAgeGrace)
	FfiDestroyerDuration{}.Destroy(r.Time)
	FfiDestroyerDuration{}.Destroy(r.Timeout)
}

type FfiConverterKeepaliveServerParameters struct{}

var FfiConverterKeepaliveServerParametersINSTANCE = FfiConverterKeepaliveServerParameters{}

func (c FfiConverterKeepaliveServerParameters) Lift(rb RustBufferI) KeepaliveServerParameters {
	return LiftFromRustBuffer[KeepaliveServerParameters](c, rb)
}

func (c FfiConverterKeepaliveServerParameters) Read(reader io.Reader) KeepaliveServerParameters {
	return KeepaliveServerParameters{
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
	}
}

func (c FfiConverterKeepaliveServerParameters) Lower(value KeepaliveServerParameters) C.RustBuffer {
	return LowerIntoRustBuffer[KeepaliveServerParameters](c, value)
}

func (c FfiConverterKeepaliveServerParameters) Write(writer io.Writer, value KeepaliveServerParameters) {
	FfiConverterDurationINSTANCE.Write(writer, value.MaxConnectionIdle)
	FfiConverterDurationINSTANCE.Write(writer, value.MaxConnectionAge)
	FfiConverterDurationINSTANCE.Write(writer, value.MaxConnectionAgeGrace)
	FfiConverterDurationINSTANCE.Write(writer, value.Time)
	FfiConverterDurationINSTANCE.Write(writer, value.Timeout)
}

type FfiDestroyerKeepaliveServerParameters struct{}

func (_ FfiDestroyerKeepaliveServerParameters) Destroy(value KeepaliveServerParameters) {
	value.Destroy()
}

// Generic message context for language bindings (UniFFI-compatible)
//
// Provides routing and descriptive metadata needed for replying,
// auditing, and instrumentation across different language bindings.
// This type is exported to foreign languages via UniFFI.
type MessageContext struct {
	// Fully-qualified sender identity
	SourceName *Name
	// Fully-qualified destination identity (may be empty for broadcast/group scenarios)
	DestinationName **Name
	// Logical/semantic type (defaults to "msg" if unspecified)
	PayloadType string
	// Arbitrary key/value pairs supplied by the sender (e.g. tracing IDs)
	Metadata map[string]string
	// Numeric identifier of the inbound connection carrying the message
	InputConnection uint64
	// Identity contained in the message
	Identity string
}

func (r *MessageContext) Destroy() {
	FfiDestroyerName{}.Destroy(r.SourceName)
	FfiDestroyerOptionalName{}.Destroy(r.DestinationName)
	FfiDestroyerString{}.Destroy(r.PayloadType)
	FfiDestroyerMapStringString{}.Destroy(r.Metadata)
	FfiDestroyerUint64{}.Destroy(r.InputConnection)
	FfiDestroyerString{}.Destroy(r.Identity)
}

type FfiConverterMessageContext struct{}

var FfiConverterMessageContextINSTANCE = FfiConverterMessageContext{}

func (c FfiConverterMessageContext) Lift(rb RustBufferI) MessageContext {
	return LiftFromRustBuffer[MessageContext](c, rb)
}

func (c FfiConverterMessageContext) Read(reader io.Reader) MessageContext {
	return MessageContext{
		FfiConverterNameINSTANCE.Read(reader),
		FfiConverterOptionalNameINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMessageContext) Lower(value MessageContext) C.RustBuffer {
	return LowerIntoRustBuffer[MessageContext](c, value)
}

func (c FfiConverterMessageContext) Write(writer io.Writer, value MessageContext) {
	FfiConverterNameINSTANCE.Write(writer, value.SourceName)
	FfiConverterOptionalNameINSTANCE.Write(writer, value.DestinationName)
	FfiConverterStringINSTANCE.Write(writer, value.PayloadType)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Metadata)
	FfiConverterUint64INSTANCE.Write(writer, value.InputConnection)
	FfiConverterStringINSTANCE.Write(writer, value.Identity)
}

type FfiDestroyerMessageContext struct{}

func (_ FfiDestroyerMessageContext) Destroy(value MessageContext) {
	value.Destroy()
}

// HTTP Proxy configuration
type ProxyConfig struct {
	// The HTTP proxy URL (e.g., "http://proxy.example.com:8080")
	Url *string
	// TLS configuration for proxy connection
	Tls TlsClientConfig
	// Optional username for proxy authentication
	Username *string
	// Optional password for proxy authentication
	Password *string
	// Headers to send with proxy requests
	Headers map[string]string
}

func (r *ProxyConfig) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.Url)
	FfiDestroyerTlsClientConfig{}.Destroy(r.Tls)
	FfiDestroyerOptionalString{}.Destroy(r.Username)
	FfiDestroyerOptionalString{}.Destroy(r.Password)
	FfiDestroyerMapStringString{}.Destroy(r.Headers)
}

type FfiConverterProxyConfig struct{}

var FfiConverterProxyConfigINSTANCE = FfiConverterProxyConfig{}

func (c FfiConverterProxyConfig) Lift(rb RustBufferI) ProxyConfig {
	return LiftFromRustBuffer[ProxyConfig](c, rb)
}

func (c FfiConverterProxyConfig) Read(reader io.Reader) ProxyConfig {
	return ProxyConfig{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterTlsClientConfigINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterProxyConfig) Lower(value ProxyConfig) C.RustBuffer {
	return LowerIntoRustBuffer[ProxyConfig](c, value)
}

func (c FfiConverterProxyConfig) Write(writer io.Writer, value ProxyConfig) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Url)
	FfiConverterTlsClientConfigINSTANCE.Write(writer, value.Tls)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Username)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Password)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Headers)
}

type FfiDestroyerProxyConfig struct{}

func (_ FfiDestroyerProxyConfig) Destroy(value ProxyConfig) {
	value.Destroy()
}

// Received message containing context and payload
type ReceivedMessage struct {
	Context MessageContext
	Payload []byte
}

func (r *ReceivedMessage) Destroy() {
	FfiDestroyerMessageContext{}.Destroy(r.Context)
	FfiDestroyerBytes{}.Destroy(r.Payload)
}

type FfiConverterReceivedMessage struct{}

var FfiConverterReceivedMessageINSTANCE = FfiConverterReceivedMessage{}

func (c FfiConverterReceivedMessage) Lift(rb RustBufferI) ReceivedMessage {
	return LiftFromRustBuffer[ReceivedMessage](c, rb)
}

func (c FfiConverterReceivedMessage) Read(reader io.Reader) ReceivedMessage {
	return ReceivedMessage{
		FfiConverterMessageContextINSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterReceivedMessage) Lower(value ReceivedMessage) C.RustBuffer {
	return LowerIntoRustBuffer[ReceivedMessage](c, value)
}

func (c FfiConverterReceivedMessage) Write(writer io.Writer, value ReceivedMessage) {
	FfiConverterMessageContextINSTANCE.Write(writer, value.Context)
	FfiConverterBytesINSTANCE.Write(writer, value.Payload)
}

type FfiDestroyerReceivedMessage struct{}

func (_ FfiDestroyerReceivedMessage) Destroy(value ReceivedMessage) {
	value.Destroy()
}

// Runtime configuration for the SLIM bindings
//
// Controls the Tokio runtime behavior including thread count, naming, and shutdown timeout.
type RuntimeConfig struct {
	// Number of cores to use for the runtime (0 = use all available cores)
	NCores uint64
	// Thread name prefix for the runtime
	ThreadName string
	// Timeout duration for draining services during shutdown
	DrainTimeout time.Duration
}

func (r *RuntimeConfig) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.NCores)
	FfiDestroyerString{}.Destroy(r.ThreadName)
	FfiDestroyerDuration{}.Destroy(r.DrainTimeout)
}

type FfiConverterRuntimeConfig struct{}

var FfiConverterRuntimeConfigINSTANCE = FfiConverterRuntimeConfig{}

func (c FfiConverterRuntimeConfig) Lift(rb RustBufferI) RuntimeConfig {
	return LiftFromRustBuffer[RuntimeConfig](c, rb)
}

func (c FfiConverterRuntimeConfig) Read(reader io.Reader) RuntimeConfig {
	return RuntimeConfig{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
	}
}

func (c FfiConverterRuntimeConfig) Lower(value RuntimeConfig) C.RustBuffer {
	return LowerIntoRustBuffer[RuntimeConfig](c, value)
}

func (c FfiConverterRuntimeConfig) Write(writer io.Writer, value RuntimeConfig) {
	FfiConverterUint64INSTANCE.Write(writer, value.NCores)
	FfiConverterStringINSTANCE.Write(writer, value.ThreadName)
	FfiConverterDurationINSTANCE.Write(writer, value.DrainTimeout)
}

type FfiDestroyerRuntimeConfig struct{}

func (_ FfiDestroyerRuntimeConfig) Destroy(value RuntimeConfig) {
	value.Destroy()
}

// Server configuration for running a SLIM server
type ServerConfig struct {
	// Endpoint address to listen on (e.g., "0.0.0.0:50051" or "[::]:50051")
	Endpoint string
	// TLS server configuration
	Tls TlsServerConfig
	// Use HTTP/2 only (default: true)
	Http2Only bool
	// Maximum size (in MiB) of messages accepted by the server
	MaxFrameSize *uint32
	// Maximum number of concurrent streams per connection
	MaxConcurrentStreams *uint32
	// Maximum header list size in bytes
	MaxHeaderListSize *uint32
	// Read buffer size in bytes
	ReadBufferSize *uint64
	// Write buffer size in bytes
	WriteBufferSize *uint64
	// Keepalive parameters
	Keepalive KeepaliveServerParameters
	// Authentication configuration for incoming requests
	Auth ServerAuthenticationConfig
	// Arbitrary user-provided metadata as JSON string
	Metadata *string
}

func (r *ServerConfig) Destroy() {
	FfiDestroyerString{}.Destroy(r.Endpoint)
	FfiDestroyerTlsServerConfig{}.Destroy(r.Tls)
	FfiDestroyerBool{}.Destroy(r.Http2Only)
	FfiDestroyerOptionalUint32{}.Destroy(r.MaxFrameSize)
	FfiDestroyerOptionalUint32{}.Destroy(r.MaxConcurrentStreams)
	FfiDestroyerOptionalUint32{}.Destroy(r.MaxHeaderListSize)
	FfiDestroyerOptionalUint64{}.Destroy(r.ReadBufferSize)
	FfiDestroyerOptionalUint64{}.Destroy(r.WriteBufferSize)
	FfiDestroyerKeepaliveServerParameters{}.Destroy(r.Keepalive)
	FfiDestroyerServerAuthenticationConfig{}.Destroy(r.Auth)
	FfiDestroyerOptionalString{}.Destroy(r.Metadata)
}

type FfiConverterServerConfig struct{}

var FfiConverterServerConfigINSTANCE = FfiConverterServerConfig{}

func (c FfiConverterServerConfig) Lift(rb RustBufferI) ServerConfig {
	return LiftFromRustBuffer[ServerConfig](c, rb)
}

func (c FfiConverterServerConfig) Read(reader io.Reader) ServerConfig {
	return ServerConfig{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterTlsServerConfigINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterKeepaliveServerParametersINSTANCE.Read(reader),
		FfiConverterServerAuthenticationConfigINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterServerConfig) Lower(value ServerConfig) C.RustBuffer {
	return LowerIntoRustBuffer[ServerConfig](c, value)
}

func (c FfiConverterServerConfig) Write(writer io.Writer, value ServerConfig) {
	FfiConverterStringINSTANCE.Write(writer, value.Endpoint)
	FfiConverterTlsServerConfigINSTANCE.Write(writer, value.Tls)
	FfiConverterBoolINSTANCE.Write(writer, value.Http2Only)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.MaxFrameSize)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.MaxConcurrentStreams)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.MaxHeaderListSize)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.ReadBufferSize)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.WriteBufferSize)
	FfiConverterKeepaliveServerParametersINSTANCE.Write(writer, value.Keepalive)
	FfiConverterServerAuthenticationConfigINSTANCE.Write(writer, value.Auth)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Metadata)
}

type FfiDestroyerServerConfig struct{}

func (_ FfiDestroyerServerConfig) Destroy(value ServerConfig) {
	value.Destroy()
}

// Service configuration wrapper for uniffi bindings
type ServiceConfig struct {
	// Optional node ID for the service
	NodeId *string
	// Optional group name for the service
	GroupName *string
	// DataPlane configuration (servers and clients)
	Dataplane DataplaneConfig
}

func (r *ServiceConfig) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.NodeId)
	FfiDestroyerOptionalString{}.Destroy(r.GroupName)
	FfiDestroyerDataplaneConfig{}.Destroy(r.Dataplane)
}

type FfiConverterServiceConfig struct{}

var FfiConverterServiceConfigINSTANCE = FfiConverterServiceConfig{}

func (c FfiConverterServiceConfig) Lift(rb RustBufferI) ServiceConfig {
	return LiftFromRustBuffer[ServiceConfig](c, rb)
}

func (c FfiConverterServiceConfig) Read(reader io.Reader) ServiceConfig {
	return ServiceConfig{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterDataplaneConfigINSTANCE.Read(reader),
	}
}

func (c FfiConverterServiceConfig) Lower(value ServiceConfig) C.RustBuffer {
	return LowerIntoRustBuffer[ServiceConfig](c, value)
}

func (c FfiConverterServiceConfig) Write(writer io.Writer, value ServiceConfig) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.NodeId)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.GroupName)
	FfiConverterDataplaneConfigINSTANCE.Write(writer, value.Dataplane)
}

type FfiDestroyerServiceConfig struct{}

func (_ FfiDestroyerServiceConfig) Destroy(value ServiceConfig) {
	value.Destroy()
}

// Session configuration
type SessionConfig struct {
	// Session type (PointToPoint or Group)
	SessionType SessionType
	// Enable MLS encryption for this session
	EnableMls bool
	// Maximum number of retries for message transmission (None = use default)
	MaxRetries *uint32
	// Interval between retries in milliseconds (None = use default)
	Interval *time.Duration
	// Custom metadata key-value pairs for the session
	Metadata map[string]string
}

func (r *SessionConfig) Destroy() {
	FfiDestroyerSessionType{}.Destroy(r.SessionType)
	FfiDestroyerBool{}.Destroy(r.EnableMls)
	FfiDestroyerOptionalUint32{}.Destroy(r.MaxRetries)
	FfiDestroyerOptionalDuration{}.Destroy(r.Interval)
	FfiDestroyerMapStringString{}.Destroy(r.Metadata)
}

type FfiConverterSessionConfig struct{}

var FfiConverterSessionConfigINSTANCE = FfiConverterSessionConfig{}

func (c FfiConverterSessionConfig) Lift(rb RustBufferI) SessionConfig {
	return LiftFromRustBuffer[SessionConfig](c, rb)
}

func (c FfiConverterSessionConfig) Read(reader io.Reader) SessionConfig {
	return SessionConfig{
		FfiConverterSessionTypeINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalDurationINSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterSessionConfig) Lower(value SessionConfig) C.RustBuffer {
	return LowerIntoRustBuffer[SessionConfig](c, value)
}

func (c FfiConverterSessionConfig) Write(writer io.Writer, value SessionConfig) {
	FfiConverterSessionTypeINSTANCE.Write(writer, value.SessionType)
	FfiConverterBoolINSTANCE.Write(writer, value.EnableMls)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.MaxRetries)
	FfiConverterOptionalDurationINSTANCE.Write(writer, value.Interval)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Metadata)
}

type FfiDestroyerSessionConfig struct{}

func (_ FfiDestroyerSessionConfig) Destroy(value SessionConfig) {
	value.Destroy()
}

// Result of creating a session, containing the session context and a completion handle
//
// The completion handle should be awaited to ensure the session is fully established.
type SessionWithCompletion struct {
	// The session context for performing operations
	Session *Session
	// Completion handle to wait for session establishment
	Completion *CompletionHandle
}

func (r *SessionWithCompletion) Destroy() {
	FfiDestroyerSession{}.Destroy(r.Session)
	FfiDestroyerCompletionHandle{}.Destroy(r.Completion)
}

type FfiConverterSessionWithCompletion struct{}

var FfiConverterSessionWithCompletionINSTANCE = FfiConverterSessionWithCompletion{}

func (c FfiConverterSessionWithCompletion) Lift(rb RustBufferI) SessionWithCompletion {
	return LiftFromRustBuffer[SessionWithCompletion](c, rb)
}

func (c FfiConverterSessionWithCompletion) Read(reader io.Reader) SessionWithCompletion {
	return SessionWithCompletion{
		FfiConverterSessionINSTANCE.Read(reader),
		FfiConverterCompletionHandleINSTANCE.Read(reader),
	}
}

func (c FfiConverterSessionWithCompletion) Lower(value SessionWithCompletion) C.RustBuffer {
	return LowerIntoRustBuffer[SessionWithCompletion](c, value)
}

func (c FfiConverterSessionWithCompletion) Write(writer io.Writer, value SessionWithCompletion) {
	FfiConverterSessionINSTANCE.Write(writer, value.Session)
	FfiConverterCompletionHandleINSTANCE.Write(writer, value.Completion)
}

type FfiDestroyerSessionWithCompletion struct{}

func (_ FfiDestroyerSessionWithCompletion) Destroy(value SessionWithCompletion) {
	value.Destroy()
}

// SPIRE configuration for SPIFFE Workload API integration
type SpireConfig struct {
	// Path to the SPIFFE Workload API socket (None => use SPIFFE_ENDPOINT_SOCKET env var)
	SocketPath *string
	// Optional target SPIFFE ID when requesting JWT SVIDs
	TargetSpiffeId *string
	// Audiences to request/verify for JWT SVIDs
	JwtAudiences []string
	// Optional trust domains override for X.509 bundle retrieval
	TrustDomains []string
}

func (r *SpireConfig) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.SocketPath)
	FfiDestroyerOptionalString{}.Destroy(r.TargetSpiffeId)
	FfiDestroyerSequenceString{}.Destroy(r.JwtAudiences)
	FfiDestroyerSequenceString{}.Destroy(r.TrustDomains)
}

type FfiConverterSpireConfig struct{}

var FfiConverterSpireConfigINSTANCE = FfiConverterSpireConfig{}

func (c FfiConverterSpireConfig) Lift(rb RustBufferI) SpireConfig {
	return LiftFromRustBuffer[SpireConfig](c, rb)
}

func (c FfiConverterSpireConfig) Read(reader io.Reader) SpireConfig {
	return SpireConfig{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterSpireConfig) Lower(value SpireConfig) C.RustBuffer {
	return LowerIntoRustBuffer[SpireConfig](c, value)
}

func (c FfiConverterSpireConfig) Write(writer io.Writer, value SpireConfig) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.SocketPath)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TargetSpiffeId)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.JwtAudiences)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.TrustDomains)
}

type FfiDestroyerSpireConfig struct{}

func (_ FfiDestroyerSpireConfig) Destroy(value SpireConfig) {
	value.Destroy()
}

// Static JWT (Bearer token) authentication configuration
// The token is loaded from a file and automatically reloaded when changed
type StaticJwtAuth struct {
	// Path to file containing the JWT token
	TokenFile string
	// Duration for caching the token before re-reading from file (default: 3600 seconds)
	Duration time.Duration
}

func (r *StaticJwtAuth) Destroy() {
	FfiDestroyerString{}.Destroy(r.TokenFile)
	FfiDestroyerDuration{}.Destroy(r.Duration)
}

type FfiConverterStaticJwtAuth struct{}

var FfiConverterStaticJwtAuthINSTANCE = FfiConverterStaticJwtAuth{}

func (c FfiConverterStaticJwtAuth) Lift(rb RustBufferI) StaticJwtAuth {
	return LiftFromRustBuffer[StaticJwtAuth](c, rb)
}

func (c FfiConverterStaticJwtAuth) Read(reader io.Reader) StaticJwtAuth {
	return StaticJwtAuth{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterDurationINSTANCE.Read(reader),
	}
}

func (c FfiConverterStaticJwtAuth) Lower(value StaticJwtAuth) C.RustBuffer {
	return LowerIntoRustBuffer[StaticJwtAuth](c, value)
}

func (c FfiConverterStaticJwtAuth) Write(writer io.Writer, value StaticJwtAuth) {
	FfiConverterStringINSTANCE.Write(writer, value.TokenFile)
	FfiConverterDurationINSTANCE.Write(writer, value.Duration)
}

type FfiDestroyerStaticJwtAuth struct{}

func (_ FfiDestroyerStaticJwtAuth) Destroy(value StaticJwtAuth) {
	value.Destroy()
}

// TLS configuration for client connections
type TlsClientConfig struct {
	// Disable TLS entirely (plain text connection)
	Insecure bool
	// Skip server certificate verification (enables TLS but doesn't verify certs)
	// WARNING: Only use for testing - insecure in production!
	InsecureSkipVerify bool
	// Certificate and key source for client authentication
	Source TlsSource
	// CA certificate source for verifying server certificates
	CaSource CaSource
	// Include system CA certificates pool (default: true)
	IncludeSystemCaCertsPool bool
	// TLS version to use: "tls1.2" or "tls1.3" (default: "tls1.3")
	TlsVersion string
}

func (r *TlsClientConfig) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Insecure)
	FfiDestroyerBool{}.Destroy(r.InsecureSkipVerify)
	FfiDestroyerTlsSource{}.Destroy(r.Source)
	FfiDestroyerCaSource{}.Destroy(r.CaSource)
	FfiDestroyerBool{}.Destroy(r.IncludeSystemCaCertsPool)
	FfiDestroyerString{}.Destroy(r.TlsVersion)
}

type FfiConverterTlsClientConfig struct{}

var FfiConverterTlsClientConfigINSTANCE = FfiConverterTlsClientConfig{}

func (c FfiConverterTlsClientConfig) Lift(rb RustBufferI) TlsClientConfig {
	return LiftFromRustBuffer[TlsClientConfig](c, rb)
}

func (c FfiConverterTlsClientConfig) Read(reader io.Reader) TlsClientConfig {
	return TlsClientConfig{
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterTlsSourceINSTANCE.Read(reader),
		FfiConverterCaSourceINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTlsClientConfig) Lower(value TlsClientConfig) C.RustBuffer {
	return LowerIntoRustBuffer[TlsClientConfig](c, value)
}

func (c FfiConverterTlsClientConfig) Write(writer io.Writer, value TlsClientConfig) {
	FfiConverterBoolINSTANCE.Write(writer, value.Insecure)
	FfiConverterBoolINSTANCE.Write(writer, value.InsecureSkipVerify)
	FfiConverterTlsSourceINSTANCE.Write(writer, value.Source)
	FfiConverterCaSourceINSTANCE.Write(writer, value.CaSource)
	FfiConverterBoolINSTANCE.Write(writer, value.IncludeSystemCaCertsPool)
	FfiConverterStringINSTANCE.Write(writer, value.TlsVersion)
}

type FfiDestroyerTlsClientConfig struct{}

func (_ FfiDestroyerTlsClientConfig) Destroy(value TlsClientConfig) {
	value.Destroy()
}

// TLS configuration for server connections
type TlsServerConfig struct {
	// Disable TLS entirely (plain text connection)
	Insecure bool
	// Certificate and key source for server authentication
	Source TlsSource
	// CA certificate source for verifying client certificates
	ClientCa CaSource
	// Include system CA certificates pool (default: true)
	IncludeSystemCaCertsPool bool
	// TLS version to use: "tls1.2" or "tls1.3" (default: "tls1.3")
	TlsVersion string
	// Reload client CA file when modified
	ReloadClientCaFile bool
}

func (r *TlsServerConfig) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Insecure)
	FfiDestroyerTlsSource{}.Destroy(r.Source)
	FfiDestroyerCaSource{}.Destroy(r.ClientCa)
	FfiDestroyerBool{}.Destroy(r.IncludeSystemCaCertsPool)
	FfiDestroyerString{}.Destroy(r.TlsVersion)
	FfiDestroyerBool{}.Destroy(r.ReloadClientCaFile)
}

type FfiConverterTlsServerConfig struct{}

var FfiConverterTlsServerConfigINSTANCE = FfiConverterTlsServerConfig{}

func (c FfiConverterTlsServerConfig) Lift(rb RustBufferI) TlsServerConfig {
	return LiftFromRustBuffer[TlsServerConfig](c, rb)
}

func (c FfiConverterTlsServerConfig) Read(reader io.Reader) TlsServerConfig {
	return TlsServerConfig{
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterTlsSourceINSTANCE.Read(reader),
		FfiConverterCaSourceINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterTlsServerConfig) Lower(value TlsServerConfig) C.RustBuffer {
	return LowerIntoRustBuffer[TlsServerConfig](c, value)
}

func (c FfiConverterTlsServerConfig) Write(writer io.Writer, value TlsServerConfig) {
	FfiConverterBoolINSTANCE.Write(writer, value.Insecure)
	FfiConverterTlsSourceINSTANCE.Write(writer, value.Source)
	FfiConverterCaSourceINSTANCE.Write(writer, value.ClientCa)
	FfiConverterBoolINSTANCE.Write(writer, value.IncludeSystemCaCertsPool)
	FfiConverterStringINSTANCE.Write(writer, value.TlsVersion)
	FfiConverterBoolINSTANCE.Write(writer, value.ReloadClientCaFile)
}

type FfiDestroyerTlsServerConfig struct{}

func (_ FfiDestroyerTlsServerConfig) Destroy(value TlsServerConfig) {
	value.Destroy()
}

// Tracing/logging configuration for the SLIM bindings
//
// Controls logging behavior including log level, thread name/ID display, and filters.
type TracingConfig struct {
	// Log level (e.g., "debug", "info", "warn", "error")
	LogLevel string
	// Whether to display thread names in logs
	DisplayThreadNames bool
	// Whether to display thread IDs in logs
	DisplayThreadIds bool
	// List of tracing filter directives (e.g., ["slim=debug", "tokio=info"])
	Filters []string
}

func (r *TracingConfig) Destroy() {
	FfiDestroyerString{}.Destroy(r.LogLevel)
	FfiDestroyerBool{}.Destroy(r.DisplayThreadNames)
	FfiDestroyerBool{}.Destroy(r.DisplayThreadIds)
	FfiDestroyerSequenceString{}.Destroy(r.Filters)
}

type FfiConverterTracingConfig struct{}

var FfiConverterTracingConfigINSTANCE = FfiConverterTracingConfig{}

func (c FfiConverterTracingConfig) Lift(rb RustBufferI) TracingConfig {
	return LiftFromRustBuffer[TracingConfig](c, rb)
}

func (c FfiConverterTracingConfig) Read(reader io.Reader) TracingConfig {
	return TracingConfig{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTracingConfig) Lower(value TracingConfig) C.RustBuffer {
	return LowerIntoRustBuffer[TracingConfig](c, value)
}

func (c FfiConverterTracingConfig) Write(writer io.Writer, value TracingConfig) {
	FfiConverterStringINSTANCE.Write(writer, value.LogLevel)
	FfiConverterBoolINSTANCE.Write(writer, value.DisplayThreadNames)
	FfiConverterBoolINSTANCE.Write(writer, value.DisplayThreadIds)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Filters)
}

type FfiDestroyerTracingConfig struct{}

func (_ FfiDestroyerTracingConfig) Destroy(value TracingConfig) {
	value.Destroy()
}

// Backoff retry configuration
type BackoffConfig interface {
	Destroy()
}
type BackoffConfigExponential struct {
	Config ExponentialBackoff
}

func (e BackoffConfigExponential) Destroy() {
	FfiDestroyerExponentialBackoff{}.Destroy(e.Config)
}

type BackoffConfigFixedInterval struct {
	Config FixedIntervalBackoff
}

func (e BackoffConfigFixedInterval) Destroy() {
	FfiDestroyerFixedIntervalBackoff{}.Destroy(e.Config)
}

type FfiConverterBackoffConfig struct{}

var FfiConverterBackoffConfigINSTANCE = FfiConverterBackoffConfig{}

func (c FfiConverterBackoffConfig) Lift(rb RustBufferI) BackoffConfig {
	return LiftFromRustBuffer[BackoffConfig](c, rb)
}

func (c FfiConverterBackoffConfig) Lower(value BackoffConfig) C.RustBuffer {
	return LowerIntoRustBuffer[BackoffConfig](c, value)
}
func (FfiConverterBackoffConfig) Read(reader io.Reader) BackoffConfig {
	id := readInt32(reader)
	switch id {
	case 1:
		return BackoffConfigExponential{
			FfiConverterExponentialBackoffINSTANCE.Read(reader),
		}
	case 2:
		return BackoffConfigFixedInterval{
			FfiConverterFixedIntervalBackoffINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterBackoffConfig.Read()", id))
	}
}

func (FfiConverterBackoffConfig) Write(writer io.Writer, value BackoffConfig) {
	switch variant_value := value.(type) {
	case BackoffConfigExponential:
		writeInt32(writer, 1)
		FfiConverterExponentialBackoffINSTANCE.Write(writer, variant_value.Config)
	case BackoffConfigFixedInterval:
		writeInt32(writer, 2)
		FfiConverterFixedIntervalBackoffINSTANCE.Write(writer, variant_value.Config)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterBackoffConfig.Write", value))
	}
}

type FfiDestroyerBackoffConfig struct{}

func (_ FfiDestroyerBackoffConfig) Destroy(value BackoffConfig) {
	value.Destroy()
}

// CA certificate source configuration
type CaSource interface {
	Destroy()
}

// Load CA from file
type CaSourceFile struct {
	Path string
}

func (e CaSourceFile) Destroy() {
	FfiDestroyerString{}.Destroy(e.Path)
}

// Load CA from PEM string
type CaSourcePem struct {
	Data string
}

func (e CaSourcePem) Destroy() {
	FfiDestroyerString{}.Destroy(e.Data)
}

// Load CA from SPIRE Workload API
type CaSourceSpire struct {
	Config SpireConfig
}

func (e CaSourceSpire) Destroy() {
	FfiDestroyerSpireConfig{}.Destroy(e.Config)
}

// No CA configured
type CaSourceNone struct {
}

func (e CaSourceNone) Destroy() {
}

type FfiConverterCaSource struct{}

var FfiConverterCaSourceINSTANCE = FfiConverterCaSource{}

func (c FfiConverterCaSource) Lift(rb RustBufferI) CaSource {
	return LiftFromRustBuffer[CaSource](c, rb)
}

func (c FfiConverterCaSource) Lower(value CaSource) C.RustBuffer {
	return LowerIntoRustBuffer[CaSource](c, value)
}
func (FfiConverterCaSource) Read(reader io.Reader) CaSource {
	id := readInt32(reader)
	switch id {
	case 1:
		return CaSourceFile{
			FfiConverterStringINSTANCE.Read(reader),
		}
	case 2:
		return CaSourcePem{
			FfiConverterStringINSTANCE.Read(reader),
		}
	case 3:
		return CaSourceSpire{
			FfiConverterSpireConfigINSTANCE.Read(reader),
		}
	case 4:
		return CaSourceNone{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterCaSource.Read()", id))
	}
}

func (FfiConverterCaSource) Write(writer io.Writer, value CaSource) {
	switch variant_value := value.(type) {
	case CaSourceFile:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Path)
	case CaSourcePem:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Data)
	case CaSourceSpire:
		writeInt32(writer, 3)
		FfiConverterSpireConfigINSTANCE.Write(writer, variant_value.Config)
	case CaSourceNone:
		writeInt32(writer, 4)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterCaSource.Write", value))
	}
}

type FfiDestroyerCaSource struct{}

func (_ FfiDestroyerCaSource) Destroy(value CaSource) {
	value.Destroy()
}

// Authentication configuration enum for client
type ClientAuthenticationConfig interface {
	Destroy()
}
type ClientAuthenticationConfigBasic struct {
	Config BasicAuth
}

func (e ClientAuthenticationConfigBasic) Destroy() {
	FfiDestroyerBasicAuth{}.Destroy(e.Config)
}

type ClientAuthenticationConfigStaticJwt struct {
	Config StaticJwtAuth
}

func (e ClientAuthenticationConfigStaticJwt) Destroy() {
	FfiDestroyerStaticJwtAuth{}.Destroy(e.Config)
}

type ClientAuthenticationConfigJwt struct {
	Config ClientJwtAuth
}

func (e ClientAuthenticationConfigJwt) Destroy() {
	FfiDestroyerClientJwtAuth{}.Destroy(e.Config)
}

type ClientAuthenticationConfigNone struct {
}

func (e ClientAuthenticationConfigNone) Destroy() {
}

type FfiConverterClientAuthenticationConfig struct{}

var FfiConverterClientAuthenticationConfigINSTANCE = FfiConverterClientAuthenticationConfig{}

func (c FfiConverterClientAuthenticationConfig) Lift(rb RustBufferI) ClientAuthenticationConfig {
	return LiftFromRustBuffer[ClientAuthenticationConfig](c, rb)
}

func (c FfiConverterClientAuthenticationConfig) Lower(value ClientAuthenticationConfig) C.RustBuffer {
	return LowerIntoRustBuffer[ClientAuthenticationConfig](c, value)
}
func (FfiConverterClientAuthenticationConfig) Read(reader io.Reader) ClientAuthenticationConfig {
	id := readInt32(reader)
	switch id {
	case 1:
		return ClientAuthenticationConfigBasic{
			FfiConverterBasicAuthINSTANCE.Read(reader),
		}
	case 2:
		return ClientAuthenticationConfigStaticJwt{
			FfiConverterStaticJwtAuthINSTANCE.Read(reader),
		}
	case 3:
		return ClientAuthenticationConfigJwt{
			FfiConverterClientJwtAuthINSTANCE.Read(reader),
		}
	case 4:
		return ClientAuthenticationConfigNone{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterClientAuthenticationConfig.Read()", id))
	}
}

func (FfiConverterClientAuthenticationConfig) Write(writer io.Writer, value ClientAuthenticationConfig) {
	switch variant_value := value.(type) {
	case ClientAuthenticationConfigBasic:
		writeInt32(writer, 1)
		FfiConverterBasicAuthINSTANCE.Write(writer, variant_value.Config)
	case ClientAuthenticationConfigStaticJwt:
		writeInt32(writer, 2)
		FfiConverterStaticJwtAuthINSTANCE.Write(writer, variant_value.Config)
	case ClientAuthenticationConfigJwt:
		writeInt32(writer, 3)
		FfiConverterClientJwtAuthINSTANCE.Write(writer, variant_value.Config)
	case ClientAuthenticationConfigNone:
		writeInt32(writer, 4)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterClientAuthenticationConfig.Write", value))
	}
}

type FfiDestroyerClientAuthenticationConfig struct{}

func (_ FfiDestroyerClientAuthenticationConfig) Destroy(value ClientAuthenticationConfig) {
	value.Destroy()
}

// gRPC status codes
type Code uint16

const (
	// Success
	CodeOk Code = 0
	// The operation was cancelled
	CodeCancelled Code = 1
	// Unknown error
	CodeUnknown Code = 2
	// Client specified an invalid argument
	CodeInvalidArgument Code = 3
	// Deadline expired before operation could complete
	CodeDeadlineExceeded Code = 4
	// Some requested entity was not found
	CodeNotFound Code = 5
	// Some entity that we attempted to create already exists
	CodeAlreadyExists Code = 6
	// The caller does not have permission to execute the specified operation
	CodePermissionDenied Code = 7
	// Some resource has been exhausted
	CodeResourceExhausted Code = 8
	// The system is not in a state required for the operation's execution
	CodeFailedPrecondition Code = 9
	// The operation was aborted
	CodeAborted Code = 10
	// Operation was attempted past the valid range
	CodeOutOfRange Code = 11
	// Operation is not implemented or not supported
	CodeUnimplemented Code = 12
	// Internal errors
	CodeInternal Code = 13
	// The service is currently unavailable
	CodeUnavailable Code = 14
	// Unrecoverable data loss or corruption
	CodeDataLoss Code = 15
	// The request does not have valid authentication credentials
	CodeUnauthenticated Code = 16
)

type FfiConverterCode struct{}

var FfiConverterCodeINSTANCE = FfiConverterCode{}

func (c FfiConverterCode) Lift(rb RustBufferI) Code {
	return LiftFromRustBuffer[Code](c, rb)
}

func (c FfiConverterCode) Lower(value Code) C.RustBuffer {
	return LowerIntoRustBuffer[Code](c, value)
}
func (FfiConverterCode) Read(reader io.Reader) Code {
	id := readInt32(reader)
	return Code(id)
}

func (FfiConverterCode) Write(writer io.Writer, value Code) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerCode struct{}

func (_ FfiDestroyerCode) Destroy(value Code) {
}

// Compression type for gRPC messages
type CompressionType uint

const (
	CompressionTypeGzip    CompressionType = 1
	CompressionTypeZlib    CompressionType = 2
	CompressionTypeDeflate CompressionType = 3
	CompressionTypeSnappy  CompressionType = 4
	CompressionTypeZstd    CompressionType = 5
	CompressionTypeLz4     CompressionType = 6
	CompressionTypeNone    CompressionType = 7
	CompressionTypeEmpty   CompressionType = 8
)

type FfiConverterCompressionType struct{}

var FfiConverterCompressionTypeINSTANCE = FfiConverterCompressionType{}

func (c FfiConverterCompressionType) Lift(rb RustBufferI) CompressionType {
	return LiftFromRustBuffer[CompressionType](c, rb)
}

func (c FfiConverterCompressionType) Lower(value CompressionType) C.RustBuffer {
	return LowerIntoRustBuffer[CompressionType](c, value)
}
func (FfiConverterCompressionType) Read(reader io.Reader) CompressionType {
	id := readInt32(reader)
	return CompressionType(id)
}

func (FfiConverterCompressionType) Write(writer io.Writer, value CompressionType) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerCompressionType struct{}

func (_ FfiDestroyerCompressionType) Destroy(value CompressionType) {
}

// Direction enum
// Indicates whether the App can send, receive, both, or neither.
type Direction uint

const (
	DirectionSend          Direction = 1
	DirectionRecv          Direction = 2
	DirectionBidirectional Direction = 3
	DirectionNone          Direction = 4
)

type FfiConverterDirection struct{}

var FfiConverterDirectionINSTANCE = FfiConverterDirection{}

func (c FfiConverterDirection) Lift(rb RustBufferI) Direction {
	return LiftFromRustBuffer[Direction](c, rb)
}

func (c FfiConverterDirection) Lower(value Direction) C.RustBuffer {
	return LowerIntoRustBuffer[Direction](c, value)
}
func (FfiConverterDirection) Read(reader io.Reader) Direction {
	id := readInt32(reader)
	return Direction(id)
}

func (FfiConverterDirection) Write(writer io.Writer, value Direction) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerDirection struct{}

func (_ FfiDestroyerDirection) Destroy(value Direction) {
}

// Identity provider configuration - used to prove identity to others
type IdentityProviderConfig interface {
	Destroy()
}

// Shared secret authentication (symmetric key)
type IdentityProviderConfigSharedSecret struct {
	Id   string
	Data string
}

func (e IdentityProviderConfigSharedSecret) Destroy() {
	FfiDestroyerString{}.Destroy(e.Id)
	FfiDestroyerString{}.Destroy(e.Data)
}

// Static JWT loaded from file with auto-reload
type IdentityProviderConfigStaticJwt struct {
	Config StaticJwtAuth
}

func (e IdentityProviderConfigStaticJwt) Destroy() {
	FfiDestroyerStaticJwtAuth{}.Destroy(e.Config)
}

// Dynamic JWT generation with signing key
type IdentityProviderConfigJwt struct {
	Config ClientJwtAuth
}

func (e IdentityProviderConfigJwt) Destroy() {
	FfiDestroyerClientJwtAuth{}.Destroy(e.Config)
}

// SPIRE-based identity provider (non-Windows only)
type IdentityProviderConfigSpire struct {
	Config SpireConfig
}

func (e IdentityProviderConfigSpire) Destroy() {
	FfiDestroyerSpireConfig{}.Destroy(e.Config)
}

// No identity provider configured
type IdentityProviderConfigNone struct {
}

func (e IdentityProviderConfigNone) Destroy() {
}

type FfiConverterIdentityProviderConfig struct{}

var FfiConverterIdentityProviderConfigINSTANCE = FfiConverterIdentityProviderConfig{}

func (c FfiConverterIdentityProviderConfig) Lift(rb RustBufferI) IdentityProviderConfig {
	return LiftFromRustBuffer[IdentityProviderConfig](c, rb)
}

func (c FfiConverterIdentityProviderConfig) Lower(value IdentityProviderConfig) C.RustBuffer {
	return LowerIntoRustBuffer[IdentityProviderConfig](c, value)
}
func (FfiConverterIdentityProviderConfig) Read(reader io.Reader) IdentityProviderConfig {
	id := readInt32(reader)
	switch id {
	case 1:
		return IdentityProviderConfigSharedSecret{
			FfiConverterStringINSTANCE.Read(reader),
			FfiConverterStringINSTANCE.Read(reader),
		}
	case 2:
		return IdentityProviderConfigStaticJwt{
			FfiConverterStaticJwtAuthINSTANCE.Read(reader),
		}
	case 3:
		return IdentityProviderConfigJwt{
			FfiConverterClientJwtAuthINSTANCE.Read(reader),
		}
	case 4:
		return IdentityProviderConfigSpire{
			FfiConverterSpireConfigINSTANCE.Read(reader),
		}
	case 5:
		return IdentityProviderConfigNone{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterIdentityProviderConfig.Read()", id))
	}
}

func (FfiConverterIdentityProviderConfig) Write(writer io.Writer, value IdentityProviderConfig) {
	switch variant_value := value.(type) {
	case IdentityProviderConfigSharedSecret:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Id)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Data)
	case IdentityProviderConfigStaticJwt:
		writeInt32(writer, 2)
		FfiConverterStaticJwtAuthINSTANCE.Write(writer, variant_value.Config)
	case IdentityProviderConfigJwt:
		writeInt32(writer, 3)
		FfiConverterClientJwtAuthINSTANCE.Write(writer, variant_value.Config)
	case IdentityProviderConfigSpire:
		writeInt32(writer, 4)
		FfiConverterSpireConfigINSTANCE.Write(writer, variant_value.Config)
	case IdentityProviderConfigNone:
		writeInt32(writer, 5)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterIdentityProviderConfig.Write", value))
	}
}

type FfiDestroyerIdentityProviderConfig struct{}

func (_ FfiDestroyerIdentityProviderConfig) Destroy(value IdentityProviderConfig) {
	value.Destroy()
}

// Identity verifier configuration - used to verify identity of others
type IdentityVerifierConfig interface {
	Destroy()
}

// Shared secret verification (symmetric key)
type IdentityVerifierConfigSharedSecret struct {
	Id   string
	Data string
}

func (e IdentityVerifierConfigSharedSecret) Destroy() {
	FfiDestroyerString{}.Destroy(e.Id)
	FfiDestroyerString{}.Destroy(e.Data)
}

// JWT verification with decoding key
type IdentityVerifierConfigJwt struct {
	Config JwtAuth
}

func (e IdentityVerifierConfigJwt) Destroy() {
	FfiDestroyerJwtAuth{}.Destroy(e.Config)
}

// SPIRE-based identity verifier (non-Windows only)
type IdentityVerifierConfigSpire struct {
	Config SpireConfig
}

func (e IdentityVerifierConfigSpire) Destroy() {
	FfiDestroyerSpireConfig{}.Destroy(e.Config)
}

// No identity verifier configured
type IdentityVerifierConfigNone struct {
}

func (e IdentityVerifierConfigNone) Destroy() {
}

type FfiConverterIdentityVerifierConfig struct{}

var FfiConverterIdentityVerifierConfigINSTANCE = FfiConverterIdentityVerifierConfig{}

func (c FfiConverterIdentityVerifierConfig) Lift(rb RustBufferI) IdentityVerifierConfig {
	return LiftFromRustBuffer[IdentityVerifierConfig](c, rb)
}

func (c FfiConverterIdentityVerifierConfig) Lower(value IdentityVerifierConfig) C.RustBuffer {
	return LowerIntoRustBuffer[IdentityVerifierConfig](c, value)
}
func (FfiConverterIdentityVerifierConfig) Read(reader io.Reader) IdentityVerifierConfig {
	id := readInt32(reader)
	switch id {
	case 1:
		return IdentityVerifierConfigSharedSecret{
			FfiConverterStringINSTANCE.Read(reader),
			FfiConverterStringINSTANCE.Read(reader),
		}
	case 2:
		return IdentityVerifierConfigJwt{
			FfiConverterJwtAuthINSTANCE.Read(reader),
		}
	case 3:
		return IdentityVerifierConfigSpire{
			FfiConverterSpireConfigINSTANCE.Read(reader),
		}
	case 4:
		return IdentityVerifierConfigNone{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterIdentityVerifierConfig.Read()", id))
	}
}

func (FfiConverterIdentityVerifierConfig) Write(writer io.Writer, value IdentityVerifierConfig) {
	switch variant_value := value.(type) {
	case IdentityVerifierConfigSharedSecret:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Id)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Data)
	case IdentityVerifierConfigJwt:
		writeInt32(writer, 2)
		FfiConverterJwtAuthINSTANCE.Write(writer, variant_value.Config)
	case IdentityVerifierConfigSpire:
		writeInt32(writer, 3)
		FfiConverterSpireConfigINSTANCE.Write(writer, variant_value.Config)
	case IdentityVerifierConfigNone:
		writeInt32(writer, 4)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterIdentityVerifierConfig.Write", value))
	}
}

type FfiDestroyerIdentityVerifierConfig struct{}

func (_ FfiDestroyerIdentityVerifierConfig) Destroy(value IdentityVerifierConfig) {
	value.Destroy()
}

// JWT signing/verification algorithm
type JwtAlgorithm uint

const (
	JwtAlgorithmHs256 JwtAlgorithm = 1
	JwtAlgorithmHs384 JwtAlgorithm = 2
	JwtAlgorithmHs512 JwtAlgorithm = 3
	JwtAlgorithmEs256 JwtAlgorithm = 4
	JwtAlgorithmEs384 JwtAlgorithm = 5
	JwtAlgorithmRs256 JwtAlgorithm = 6
	JwtAlgorithmRs384 JwtAlgorithm = 7
	JwtAlgorithmRs512 JwtAlgorithm = 8
	JwtAlgorithmPs256 JwtAlgorithm = 9
	JwtAlgorithmPs384 JwtAlgorithm = 10
	JwtAlgorithmPs512 JwtAlgorithm = 11
	JwtAlgorithmEdDsa JwtAlgorithm = 12
)

type FfiConverterJwtAlgorithm struct{}

var FfiConverterJwtAlgorithmINSTANCE = FfiConverterJwtAlgorithm{}

func (c FfiConverterJwtAlgorithm) Lift(rb RustBufferI) JwtAlgorithm {
	return LiftFromRustBuffer[JwtAlgorithm](c, rb)
}

func (c FfiConverterJwtAlgorithm) Lower(value JwtAlgorithm) C.RustBuffer {
	return LowerIntoRustBuffer[JwtAlgorithm](c, value)
}
func (FfiConverterJwtAlgorithm) Read(reader io.Reader) JwtAlgorithm {
	id := readInt32(reader)
	return JwtAlgorithm(id)
}

func (FfiConverterJwtAlgorithm) Write(writer io.Writer, value JwtAlgorithm) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerJwtAlgorithm struct{}

func (_ FfiDestroyerJwtAlgorithm) Destroy(value JwtAlgorithm) {
}

// JWT key data source
type JwtKeyData interface {
	Destroy()
}

// String with encoded key(s)
type JwtKeyDataData struct {
	Value string
}

func (e JwtKeyDataData) Destroy() {
	FfiDestroyerString{}.Destroy(e.Value)
}

// File path to the key(s)
type JwtKeyDataFile struct {
	Path string
}

func (e JwtKeyDataFile) Destroy() {
	FfiDestroyerString{}.Destroy(e.Path)
}

type FfiConverterJwtKeyData struct{}

var FfiConverterJwtKeyDataINSTANCE = FfiConverterJwtKeyData{}

func (c FfiConverterJwtKeyData) Lift(rb RustBufferI) JwtKeyData {
	return LiftFromRustBuffer[JwtKeyData](c, rb)
}

func (c FfiConverterJwtKeyData) Lower(value JwtKeyData) C.RustBuffer {
	return LowerIntoRustBuffer[JwtKeyData](c, value)
}
func (FfiConverterJwtKeyData) Read(reader io.Reader) JwtKeyData {
	id := readInt32(reader)
	switch id {
	case 1:
		return JwtKeyDataData{
			FfiConverterStringINSTANCE.Read(reader),
		}
	case 2:
		return JwtKeyDataFile{
			FfiConverterStringINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterJwtKeyData.Read()", id))
	}
}

func (FfiConverterJwtKeyData) Write(writer io.Writer, value JwtKeyData) {
	switch variant_value := value.(type) {
	case JwtKeyDataData:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Value)
	case JwtKeyDataFile:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Path)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterJwtKeyData.Write", value))
	}
}

type FfiDestroyerJwtKeyData struct{}

func (_ FfiDestroyerJwtKeyData) Destroy(value JwtKeyData) {
	value.Destroy()
}

// JWT key format
type JwtKeyFormat uint

const (
	JwtKeyFormatPem  JwtKeyFormat = 1
	JwtKeyFormatJwk  JwtKeyFormat = 2
	JwtKeyFormatJwks JwtKeyFormat = 3
)

type FfiConverterJwtKeyFormat struct{}

var FfiConverterJwtKeyFormatINSTANCE = FfiConverterJwtKeyFormat{}

func (c FfiConverterJwtKeyFormat) Lift(rb RustBufferI) JwtKeyFormat {
	return LiftFromRustBuffer[JwtKeyFormat](c, rb)
}

func (c FfiConverterJwtKeyFormat) Lower(value JwtKeyFormat) C.RustBuffer {
	return LowerIntoRustBuffer[JwtKeyFormat](c, value)
}
func (FfiConverterJwtKeyFormat) Read(reader io.Reader) JwtKeyFormat {
	id := readInt32(reader)
	return JwtKeyFormat(id)
}

func (FfiConverterJwtKeyFormat) Write(writer io.Writer, value JwtKeyFormat) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerJwtKeyFormat struct{}

func (_ FfiDestroyerJwtKeyFormat) Destroy(value JwtKeyFormat) {
}

// JWT key type (encoding, decoding, or autoresolve)
type JwtKeyType interface {
	Destroy()
}

// Encoding key for signing JWTs (client-side)
type JwtKeyTypeEncoding struct {
	Key JwtKeyConfig
}

func (e JwtKeyTypeEncoding) Destroy() {
	FfiDestroyerJwtKeyConfig{}.Destroy(e.Key)
}

// Decoding key for verifying JWTs (server-side)
type JwtKeyTypeDecoding struct {
	Key JwtKeyConfig
}

func (e JwtKeyTypeDecoding) Destroy() {
	FfiDestroyerJwtKeyConfig{}.Destroy(e.Key)
}

// Automatically resolve keys based on claims
type JwtKeyTypeAutoresolve struct {
}

func (e JwtKeyTypeAutoresolve) Destroy() {
}

type FfiConverterJwtKeyType struct{}

var FfiConverterJwtKeyTypeINSTANCE = FfiConverterJwtKeyType{}

func (c FfiConverterJwtKeyType) Lift(rb RustBufferI) JwtKeyType {
	return LiftFromRustBuffer[JwtKeyType](c, rb)
}

func (c FfiConverterJwtKeyType) Lower(value JwtKeyType) C.RustBuffer {
	return LowerIntoRustBuffer[JwtKeyType](c, value)
}
func (FfiConverterJwtKeyType) Read(reader io.Reader) JwtKeyType {
	id := readInt32(reader)
	switch id {
	case 1:
		return JwtKeyTypeEncoding{
			FfiConverterJwtKeyConfigINSTANCE.Read(reader),
		}
	case 2:
		return JwtKeyTypeDecoding{
			FfiConverterJwtKeyConfigINSTANCE.Read(reader),
		}
	case 3:
		return JwtKeyTypeAutoresolve{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterJwtKeyType.Read()", id))
	}
}

func (FfiConverterJwtKeyType) Write(writer io.Writer, value JwtKeyType) {
	switch variant_value := value.(type) {
	case JwtKeyTypeEncoding:
		writeInt32(writer, 1)
		FfiConverterJwtKeyConfigINSTANCE.Write(writer, variant_value.Key)
	case JwtKeyTypeDecoding:
		writeInt32(writer, 2)
		FfiConverterJwtKeyConfigINSTANCE.Write(writer, variant_value.Key)
	case JwtKeyTypeAutoresolve:
		writeInt32(writer, 3)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterJwtKeyType.Write", value))
	}
}

type FfiDestroyerJwtKeyType struct{}

func (_ FfiDestroyerJwtKeyType) Destroy(value JwtKeyType) {
	value.Destroy()
}

// UniFFI-compatible RPC error
//
// This wraps Status to make it compatible with UniFFI foreign language bindings.
// UniFFI requires errors to be represented as enums with associated data.
type RpcError struct {
	err error
}

// Convience method to turn *RpcError into error
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

// Variant structs
type RpcErrorRpc struct {
	message string
}

func NewRpcErrorRpc() *RpcError {
	return &RpcError{err: &RpcErrorRpc{}}
}

func (e RpcErrorRpc) destroy() {
}

func (err RpcErrorRpc) Error() string {
	return fmt.Sprintf("Rpc: %s", err.message)
}

func (self RpcErrorRpc) Is(target error) bool {
	return target == ErrRpcErrorRpc
}

type FfiConverterRpcError struct{}

var FfiConverterRpcErrorINSTANCE = FfiConverterRpcError{}

func (c FfiConverterRpcError) Lift(eb RustBufferI) *RpcError {
	return LiftFromRustBuffer[*RpcError](c, eb)
}

func (c FfiConverterRpcError) Lower(value *RpcError) C.RustBuffer {
	return LowerIntoRustBuffer[*RpcError](c, value)
}

func (c FfiConverterRpcError) Read(reader io.Reader) *RpcError {
	errorID := readUint32(reader)

	message := FfiConverterStringINSTANCE.Read(reader)
	switch errorID {
	case 1:
		return &RpcError{&RpcErrorRpc{message}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterRpcError.Read()", errorID))
	}

}

func (c FfiConverterRpcError) Write(writer io.Writer, value *RpcError) {
	switch variantValue := value.err.(type) {
	case *RpcErrorRpc:
		writeInt32(writer, 1)
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
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiDestroyerRpcError.Destroy", value))
	}
}

// Authentication configuration enum for server
type ServerAuthenticationConfig interface {
	Destroy()
}
type ServerAuthenticationConfigBasic struct {
	Config BasicAuth
}

func (e ServerAuthenticationConfigBasic) Destroy() {
	FfiDestroyerBasicAuth{}.Destroy(e.Config)
}

type ServerAuthenticationConfigJwt struct {
	Config JwtAuth
}

func (e ServerAuthenticationConfigJwt) Destroy() {
	FfiDestroyerJwtAuth{}.Destroy(e.Config)
}

type ServerAuthenticationConfigNone struct {
}

func (e ServerAuthenticationConfigNone) Destroy() {
}

type FfiConverterServerAuthenticationConfig struct{}

var FfiConverterServerAuthenticationConfigINSTANCE = FfiConverterServerAuthenticationConfig{}

func (c FfiConverterServerAuthenticationConfig) Lift(rb RustBufferI) ServerAuthenticationConfig {
	return LiftFromRustBuffer[ServerAuthenticationConfig](c, rb)
}

func (c FfiConverterServerAuthenticationConfig) Lower(value ServerAuthenticationConfig) C.RustBuffer {
	return LowerIntoRustBuffer[ServerAuthenticationConfig](c, value)
}
func (FfiConverterServerAuthenticationConfig) Read(reader io.Reader) ServerAuthenticationConfig {
	id := readInt32(reader)
	switch id {
	case 1:
		return ServerAuthenticationConfigBasic{
			FfiConverterBasicAuthINSTANCE.Read(reader),
		}
	case 2:
		return ServerAuthenticationConfigJwt{
			FfiConverterJwtAuthINSTANCE.Read(reader),
		}
	case 3:
		return ServerAuthenticationConfigNone{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterServerAuthenticationConfig.Read()", id))
	}
}

func (FfiConverterServerAuthenticationConfig) Write(writer io.Writer, value ServerAuthenticationConfig) {
	switch variant_value := value.(type) {
	case ServerAuthenticationConfigBasic:
		writeInt32(writer, 1)
		FfiConverterBasicAuthINSTANCE.Write(writer, variant_value.Config)
	case ServerAuthenticationConfigJwt:
		writeInt32(writer, 2)
		FfiConverterJwtAuthINSTANCE.Write(writer, variant_value.Config)
	case ServerAuthenticationConfigNone:
		writeInt32(writer, 3)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterServerAuthenticationConfig.Write", value))
	}
}

type FfiDestroyerServerAuthenticationConfig struct{}

func (_ FfiDestroyerServerAuthenticationConfig) Destroy(value ServerAuthenticationConfig) {
	value.Destroy()
}

// Session type enum
type SessionType uint

const (
	SessionTypePointToPoint SessionType = 1
	SessionTypeGroup        SessionType = 2
)

type FfiConverterSessionType struct{}

var FfiConverterSessionTypeINSTANCE = FfiConverterSessionType{}

func (c FfiConverterSessionType) Lift(rb RustBufferI) SessionType {
	return LiftFromRustBuffer[SessionType](c, rb)
}

func (c FfiConverterSessionType) Lower(value SessionType) C.RustBuffer {
	return LowerIntoRustBuffer[SessionType](c, value)
}
func (FfiConverterSessionType) Read(reader io.Reader) SessionType {
	id := readInt32(reader)
	return SessionType(id)
}

func (FfiConverterSessionType) Write(writer io.Writer, value SessionType) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerSessionType struct{}

func (_ FfiDestroyerSessionType) Destroy(value SessionType) {
}

// Error types for SLIM operations
type SlimError struct {
	err error
}

// Convience method to turn *SlimError into error
// Avoiding treating nil pointer as non nil error interface
func (err *SlimError) AsError() error {
	if err == nil {
		return nil
	} else {
		return err
	}
}

func (err SlimError) Error() string {
	return fmt.Sprintf("SlimError: %s", err.err.Error())
}

func (err SlimError) Unwrap() error {
	return err.err
}

// Err* are used for checking error type with `errors.Is`
var ErrSlimErrorServiceError = fmt.Errorf("SlimErrorServiceError")
var ErrSlimErrorSessionError = fmt.Errorf("SlimErrorSessionError")
var ErrSlimErrorReceiveError = fmt.Errorf("SlimErrorReceiveError")
var ErrSlimErrorSendError = fmt.Errorf("SlimErrorSendError")
var ErrSlimErrorAuthError = fmt.Errorf("SlimErrorAuthError")
var ErrSlimErrorConfigError = fmt.Errorf("SlimErrorConfigError")
var ErrSlimErrorRpcError = fmt.Errorf("SlimErrorRpcError")
var ErrSlimErrorTimeout = fmt.Errorf("SlimErrorTimeout")
var ErrSlimErrorInvalidArgument = fmt.Errorf("SlimErrorInvalidArgument")
var ErrSlimErrorInternalError = fmt.Errorf("SlimErrorInternalError")

// Variant structs
type SlimErrorServiceError struct {
	Message string
}

func NewSlimErrorServiceError(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorServiceError{
		Message: message}}
}

func (e SlimErrorServiceError) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorServiceError) Error() string {
	return fmt.Sprint("ServiceError",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorServiceError) Is(target error) bool {
	return target == ErrSlimErrorServiceError
}

type SlimErrorSessionError struct {
	Message string
}

func NewSlimErrorSessionError(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorSessionError{
		Message: message}}
}

func (e SlimErrorSessionError) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorSessionError) Error() string {
	return fmt.Sprint("SessionError",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorSessionError) Is(target error) bool {
	return target == ErrSlimErrorSessionError
}

type SlimErrorReceiveError struct {
	Message string
}

func NewSlimErrorReceiveError(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorReceiveError{
		Message: message}}
}

func (e SlimErrorReceiveError) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorReceiveError) Error() string {
	return fmt.Sprint("ReceiveError",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorReceiveError) Is(target error) bool {
	return target == ErrSlimErrorReceiveError
}

type SlimErrorSendError struct {
	Message string
}

func NewSlimErrorSendError(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorSendError{
		Message: message}}
}

func (e SlimErrorSendError) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorSendError) Error() string {
	return fmt.Sprint("SendError",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorSendError) Is(target error) bool {
	return target == ErrSlimErrorSendError
}

type SlimErrorAuthError struct {
	Message string
}

func NewSlimErrorAuthError(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorAuthError{
		Message: message}}
}

func (e SlimErrorAuthError) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorAuthError) Error() string {
	return fmt.Sprint("AuthError",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorAuthError) Is(target error) bool {
	return target == ErrSlimErrorAuthError
}

type SlimErrorConfigError struct {
	Message string
}

func NewSlimErrorConfigError(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorConfigError{
		Message: message}}
}

func (e SlimErrorConfigError) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorConfigError) Error() string {
	return fmt.Sprint("ConfigError",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorConfigError) Is(target error) bool {
	return target == ErrSlimErrorConfigError
}

type SlimErrorRpcError struct {
	Message string
}

func NewSlimErrorRpcError(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorRpcError{
		Message: message}}
}

func (e SlimErrorRpcError) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorRpcError) Error() string {
	return fmt.Sprint("RpcError",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorRpcError) Is(target error) bool {
	return target == ErrSlimErrorRpcError
}

type SlimErrorTimeout struct {
}

func NewSlimErrorTimeout() *SlimError {
	return &SlimError{err: &SlimErrorTimeout{}}
}

func (e SlimErrorTimeout) destroy() {
}

func (err SlimErrorTimeout) Error() string {
	return fmt.Sprint("Timeout")
}

func (self SlimErrorTimeout) Is(target error) bool {
	return target == ErrSlimErrorTimeout
}

type SlimErrorInvalidArgument struct {
	Message string
}

func NewSlimErrorInvalidArgument(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorInvalidArgument{
		Message: message}}
}

func (e SlimErrorInvalidArgument) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorInvalidArgument) Error() string {
	return fmt.Sprint("InvalidArgument",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorInvalidArgument) Is(target error) bool {
	return target == ErrSlimErrorInvalidArgument
}

type SlimErrorInternalError struct {
	Message string
}

func NewSlimErrorInternalError(
	message string,
) *SlimError {
	return &SlimError{err: &SlimErrorInternalError{
		Message: message}}
}

func (e SlimErrorInternalError) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err SlimErrorInternalError) Error() string {
	return fmt.Sprint("InternalError",
		": ",

		"Message=",
		err.Message,
	)
}

func (self SlimErrorInternalError) Is(target error) bool {
	return target == ErrSlimErrorInternalError
}

type FfiConverterSlimError struct{}

var FfiConverterSlimErrorINSTANCE = FfiConverterSlimError{}

func (c FfiConverterSlimError) Lift(eb RustBufferI) *SlimError {
	return LiftFromRustBuffer[*SlimError](c, eb)
}

func (c FfiConverterSlimError) Lower(value *SlimError) C.RustBuffer {
	return LowerIntoRustBuffer[*SlimError](c, value)
}

func (c FfiConverterSlimError) Read(reader io.Reader) *SlimError {
	errorID := readUint32(reader)

	switch errorID {
	case 1:
		return &SlimError{&SlimErrorServiceError{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 2:
		return &SlimError{&SlimErrorSessionError{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 3:
		return &SlimError{&SlimErrorReceiveError{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 4:
		return &SlimError{&SlimErrorSendError{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 5:
		return &SlimError{&SlimErrorAuthError{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 6:
		return &SlimError{&SlimErrorConfigError{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 7:
		return &SlimError{&SlimErrorRpcError{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 8:
		return &SlimError{&SlimErrorTimeout{}}
	case 9:
		return &SlimError{&SlimErrorInvalidArgument{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 10:
		return &SlimError{&SlimErrorInternalError{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterSlimError.Read()", errorID))
	}
}

func (c FfiConverterSlimError) Write(writer io.Writer, value *SlimError) {
	switch variantValue := value.err.(type) {
	case *SlimErrorServiceError:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *SlimErrorSessionError:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *SlimErrorReceiveError:
		writeInt32(writer, 3)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *SlimErrorSendError:
		writeInt32(writer, 4)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *SlimErrorAuthError:
		writeInt32(writer, 5)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *SlimErrorConfigError:
		writeInt32(writer, 6)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *SlimErrorRpcError:
		writeInt32(writer, 7)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *SlimErrorTimeout:
		writeInt32(writer, 8)
	case *SlimErrorInvalidArgument:
		writeInt32(writer, 9)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *SlimErrorInternalError:
		writeInt32(writer, 10)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiConverterSlimError.Write", value))
	}
}

type FfiDestroyerSlimError struct{}

func (_ FfiDestroyerSlimError) Destroy(value *SlimError) {
	switch variantValue := value.err.(type) {
	case SlimErrorServiceError:
		variantValue.destroy()
	case SlimErrorSessionError:
		variantValue.destroy()
	case SlimErrorReceiveError:
		variantValue.destroy()
	case SlimErrorSendError:
		variantValue.destroy()
	case SlimErrorAuthError:
		variantValue.destroy()
	case SlimErrorConfigError:
		variantValue.destroy()
	case SlimErrorRpcError:
		variantValue.destroy()
	case SlimErrorTimeout:
		variantValue.destroy()
	case SlimErrorInvalidArgument:
		variantValue.destroy()
	case SlimErrorInternalError:
		variantValue.destroy()
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiDestroyerSlimError.Destroy", value))
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

// TLS certificate and key source configuration
type TlsSource interface {
	Destroy()
}

// Load certificate and key from PEM strings
type TlsSourcePem struct {
	Cert string
	Key  string
}

func (e TlsSourcePem) Destroy() {
	FfiDestroyerString{}.Destroy(e.Cert)
	FfiDestroyerString{}.Destroy(e.Key)
}

// Load certificate and key from files (with auto-reload support)
type TlsSourceFile struct {
	Cert string
	Key  string
}

func (e TlsSourceFile) Destroy() {
	FfiDestroyerString{}.Destroy(e.Cert)
	FfiDestroyerString{}.Destroy(e.Key)
}

// Load certificate and key from SPIRE Workload API
type TlsSourceSpire struct {
	Config SpireConfig
}

func (e TlsSourceSpire) Destroy() {
	FfiDestroyerSpireConfig{}.Destroy(e.Config)
}

// No certificate/key configured
type TlsSourceNone struct {
}

func (e TlsSourceNone) Destroy() {
}

type FfiConverterTlsSource struct{}

var FfiConverterTlsSourceINSTANCE = FfiConverterTlsSource{}

func (c FfiConverterTlsSource) Lift(rb RustBufferI) TlsSource {
	return LiftFromRustBuffer[TlsSource](c, rb)
}

func (c FfiConverterTlsSource) Lower(value TlsSource) C.RustBuffer {
	return LowerIntoRustBuffer[TlsSource](c, value)
}
func (FfiConverterTlsSource) Read(reader io.Reader) TlsSource {
	id := readInt32(reader)
	switch id {
	case 1:
		return TlsSourcePem{
			FfiConverterStringINSTANCE.Read(reader),
			FfiConverterStringINSTANCE.Read(reader),
		}
	case 2:
		return TlsSourceFile{
			FfiConverterStringINSTANCE.Read(reader),
			FfiConverterStringINSTANCE.Read(reader),
		}
	case 3:
		return TlsSourceSpire{
			FfiConverterSpireConfigINSTANCE.Read(reader),
		}
	case 4:
		return TlsSourceNone{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterTlsSource.Read()", id))
	}
}

func (FfiConverterTlsSource) Write(writer io.Writer, value TlsSource) {
	switch variant_value := value.(type) {
	case TlsSourcePem:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Cert)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Key)
	case TlsSourceFile:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Cert)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Key)
	case TlsSourceSpire:
		writeInt32(writer, 3)
		FfiConverterSpireConfigINSTANCE.Write(writer, variant_value.Config)
	case TlsSourceNone:
		writeInt32(writer, 4)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterTlsSource.Write", value))
	}
}

type FfiDestroyerTlsSource struct{}

func (_ FfiDestroyerTlsSource) Destroy(value TlsSource) {
	value.Destroy()
}

type FfiConverterOptionalUint32 struct{}

var FfiConverterOptionalUint32INSTANCE = FfiConverterOptionalUint32{}

func (c FfiConverterOptionalUint32) Lift(rb RustBufferI) *uint32 {
	return LiftFromRustBuffer[*uint32](c, rb)
}

func (_ FfiConverterOptionalUint32) Read(reader io.Reader) *uint32 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint32INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint32) Lower(value *uint32) C.RustBuffer {
	return LowerIntoRustBuffer[*uint32](c, value)
}

func (_ FfiConverterOptionalUint32) Write(writer io.Writer, value *uint32) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint32INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint32 struct{}

func (_ FfiDestroyerOptionalUint32) Destroy(value *uint32) {
	if value != nil {
		FfiDestroyerUint32{}.Destroy(*value)
	}
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

type FfiConverterOptionalString struct{}

var FfiConverterOptionalStringINSTANCE = FfiConverterOptionalString{}

func (c FfiConverterOptionalString) Lift(rb RustBufferI) *string {
	return LiftFromRustBuffer[*string](c, rb)
}

func (_ FfiConverterOptionalString) Read(reader io.Reader) *string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalString) Lower(value *string) C.RustBuffer {
	return LowerIntoRustBuffer[*string](c, value)
}

func (_ FfiConverterOptionalString) Write(writer io.Writer, value *string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalString struct{}

func (_ FfiDestroyerOptionalString) Destroy(value *string) {
	if value != nil {
		FfiDestroyerString{}.Destroy(*value)
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

type FfiConverterOptionalName struct{}

var FfiConverterOptionalNameINSTANCE = FfiConverterOptionalName{}

func (c FfiConverterOptionalName) Lift(rb RustBufferI) **Name {
	return LiftFromRustBuffer[**Name](c, rb)
}

func (_ FfiConverterOptionalName) Read(reader io.Reader) **Name {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterNameINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalName) Lower(value **Name) C.RustBuffer {
	return LowerIntoRustBuffer[**Name](c, value)
}

func (_ FfiConverterOptionalName) Write(writer io.Writer, value **Name) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterNameINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalName struct{}

func (_ FfiDestroyerOptionalName) Destroy(value **Name) {
	if value != nil {
		FfiDestroyerName{}.Destroy(*value)
	}
}

type FfiConverterOptionalKeepaliveConfig struct{}

var FfiConverterOptionalKeepaliveConfigINSTANCE = FfiConverterOptionalKeepaliveConfig{}

func (c FfiConverterOptionalKeepaliveConfig) Lift(rb RustBufferI) *KeepaliveConfig {
	return LiftFromRustBuffer[*KeepaliveConfig](c, rb)
}

func (_ FfiConverterOptionalKeepaliveConfig) Read(reader io.Reader) *KeepaliveConfig {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterKeepaliveConfigINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalKeepaliveConfig) Lower(value *KeepaliveConfig) C.RustBuffer {
	return LowerIntoRustBuffer[*KeepaliveConfig](c, value)
}

func (_ FfiConverterOptionalKeepaliveConfig) Write(writer io.Writer, value *KeepaliveConfig) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterKeepaliveConfigINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalKeepaliveConfig struct{}

func (_ FfiDestroyerOptionalKeepaliveConfig) Destroy(value *KeepaliveConfig) {
	if value != nil {
		FfiDestroyerKeepaliveConfig{}.Destroy(*value)
	}
}

type FfiConverterOptionalCompressionType struct{}

var FfiConverterOptionalCompressionTypeINSTANCE = FfiConverterOptionalCompressionType{}

func (c FfiConverterOptionalCompressionType) Lift(rb RustBufferI) *CompressionType {
	return LiftFromRustBuffer[*CompressionType](c, rb)
}

func (_ FfiConverterOptionalCompressionType) Read(reader io.Reader) *CompressionType {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterCompressionTypeINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalCompressionType) Lower(value *CompressionType) C.RustBuffer {
	return LowerIntoRustBuffer[*CompressionType](c, value)
}

func (_ FfiConverterOptionalCompressionType) Write(writer io.Writer, value *CompressionType) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterCompressionTypeINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalCompressionType struct{}

func (_ FfiDestroyerOptionalCompressionType) Destroy(value *CompressionType) {
	if value != nil {
		FfiDestroyerCompressionType{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceString struct{}

var FfiConverterOptionalSequenceStringINSTANCE = FfiConverterOptionalSequenceString{}

func (c FfiConverterOptionalSequenceString) Lift(rb RustBufferI) *[]string {
	return LiftFromRustBuffer[*[]string](c, rb)
}

func (_ FfiConverterOptionalSequenceString) Read(reader io.Reader) *[]string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceString) Lower(value *[]string) C.RustBuffer {
	return LowerIntoRustBuffer[*[]string](c, value)
}

func (_ FfiConverterOptionalSequenceString) Write(writer io.Writer, value *[]string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceString struct{}

func (_ FfiDestroyerOptionalSequenceString) Destroy(value *[]string) {
	if value != nil {
		FfiDestroyerSequenceString{}.Destroy(*value)
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

func (c FfiConverterSequenceName) Lift(rb RustBufferI) []*Name {
	return LiftFromRustBuffer[[]*Name](c, rb)
}

func (c FfiConverterSequenceName) Read(reader io.Reader) []*Name {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]*Name, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterNameINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceName) Lower(value []*Name) C.RustBuffer {
	return LowerIntoRustBuffer[[]*Name](c, value)
}

func (c FfiConverterSequenceName) Write(writer io.Writer, value []*Name) {
	if len(value) > math.MaxInt32 {
		panic("[]*Name is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterNameINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceName struct{}

func (FfiDestroyerSequenceName) Destroy(sequence []*Name) {
	for _, value := range sequence {
		FfiDestroyerName{}.Destroy(value)
	}
}

type FfiConverterSequenceService struct{}

var FfiConverterSequenceServiceINSTANCE = FfiConverterSequenceService{}

func (c FfiConverterSequenceService) Lift(rb RustBufferI) []*Service {
	return LiftFromRustBuffer[[]*Service](c, rb)
}

func (c FfiConverterSequenceService) Read(reader io.Reader) []*Service {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]*Service, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterServiceINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceService) Lower(value []*Service) C.RustBuffer {
	return LowerIntoRustBuffer[[]*Service](c, value)
}

func (c FfiConverterSequenceService) Write(writer io.Writer, value []*Service) {
	if len(value) > math.MaxInt32 {
		panic("[]*Service is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterServiceINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceService struct{}

func (FfiDestroyerSequenceService) Destroy(sequence []*Service) {
	for _, value := range sequence {
		FfiDestroyerService{}.Destroy(value)
	}
}

type FfiConverterSequenceClientConfig struct{}

var FfiConverterSequenceClientConfigINSTANCE = FfiConverterSequenceClientConfig{}

func (c FfiConverterSequenceClientConfig) Lift(rb RustBufferI) []ClientConfig {
	return LiftFromRustBuffer[[]ClientConfig](c, rb)
}

func (c FfiConverterSequenceClientConfig) Read(reader io.Reader) []ClientConfig {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ClientConfig, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterClientConfigINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceClientConfig) Lower(value []ClientConfig) C.RustBuffer {
	return LowerIntoRustBuffer[[]ClientConfig](c, value)
}

func (c FfiConverterSequenceClientConfig) Write(writer io.Writer, value []ClientConfig) {
	if len(value) > math.MaxInt32 {
		panic("[]ClientConfig is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterClientConfigINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceClientConfig struct{}

func (FfiDestroyerSequenceClientConfig) Destroy(sequence []ClientConfig) {
	for _, value := range sequence {
		FfiDestroyerClientConfig{}.Destroy(value)
	}
}

type FfiConverterSequenceServerConfig struct{}

var FfiConverterSequenceServerConfigINSTANCE = FfiConverterSequenceServerConfig{}

func (c FfiConverterSequenceServerConfig) Lift(rb RustBufferI) []ServerConfig {
	return LiftFromRustBuffer[[]ServerConfig](c, rb)
}

func (c FfiConverterSequenceServerConfig) Read(reader io.Reader) []ServerConfig {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ServerConfig, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterServerConfigINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceServerConfig) Lower(value []ServerConfig) C.RustBuffer {
	return LowerIntoRustBuffer[[]ServerConfig](c, value)
}

func (c FfiConverterSequenceServerConfig) Write(writer io.Writer, value []ServerConfig) {
	if len(value) > math.MaxInt32 {
		panic("[]ServerConfig is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterServerConfigINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceServerConfig struct{}

func (FfiDestroyerSequenceServerConfig) Destroy(sequence []ServerConfig) {
	for _, value := range sequence {
		FfiDestroyerServerConfig{}.Destroy(value)
	}
}

type FfiConverterSequenceServiceConfig struct{}

var FfiConverterSequenceServiceConfigINSTANCE = FfiConverterSequenceServiceConfig{}

func (c FfiConverterSequenceServiceConfig) Lift(rb RustBufferI) []ServiceConfig {
	return LiftFromRustBuffer[[]ServiceConfig](c, rb)
}

func (c FfiConverterSequenceServiceConfig) Read(reader io.Reader) []ServiceConfig {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ServiceConfig, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterServiceConfigINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceServiceConfig) Lower(value []ServiceConfig) C.RustBuffer {
	return LowerIntoRustBuffer[[]ServiceConfig](c, value)
}

func (c FfiConverterSequenceServiceConfig) Write(writer io.Writer, value []ServiceConfig) {
	if len(value) > math.MaxInt32 {
		panic("[]ServiceConfig is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterServiceConfigINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceServiceConfig struct{}

func (FfiDestroyerSequenceServiceConfig) Destroy(sequence []ServiceConfig) {
	for _, value := range sequence {
		FfiDestroyerServiceConfig{}.Destroy(value)
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

//export slim_bindings_uniffiFutureContinuationCallback
func slim_bindings_uniffiFutureContinuationCallback(data C.uint64_t, pollResult C.int8_t) {
	h := cgo.Handle(uintptr(data))
	waiter := h.Value().(chan int8)
	waiter <- int8(pollResult)
}

func uniffiRustCallAsync[E any, T any, F any](
	errConverter BufReader[*E],
	completeFunc rustFutureCompleteFunc[F],
	liftFunc func(F) T,
	rustFuture C.uint64_t,
	pollFunc rustFuturePollFunc,
	freeFunc rustFutureFreeFunc,
) (T, *E) {
	defer freeFunc(rustFuture)

	pollResult := int8(-1)
	waiter := make(chan int8, 1)

	chanHandle := cgo.NewHandle(waiter)
	defer chanHandle.Delete()

	for pollResult != uniffiRustFuturePollReady {
		pollFunc(
			rustFuture,
			(C.UniffiRustFutureContinuationCallback)(C.slim_bindings_uniffiFutureContinuationCallback),
			C.uint64_t(chanHandle),
		)
		pollResult = <-waiter
	}

	var goValue T
	var ffiValue F
	var err *E

	ffiValue, err = rustCallWithError(errConverter, func(status *C.RustCallStatus) F {
		return completeFunc(rustFuture, status)
	})
	if err != nil {
		return goValue, err
	}
	return liftFunc(ffiValue), nil
}

//export slim_bindings_uniffiFreeGorutine
func slim_bindings_uniffiFreeGorutine(data C.uint64_t) {
	handle := cgo.Handle(uintptr(data))
	defer handle.Delete()

	guard := handle.Value().(chan struct{})
	guard <- struct{}{}
}

// Create a new Service with builder pattern
func CreateService(name string) (*Service, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_func_create_service(FfiConverterStringINSTANCE.Lower(name), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Service
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterServiceINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new Service with configuration
func CreateServiceWithConfig(name string, config ServiceConfig) (*Service, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_func_create_service_with_config(FfiConverterStringINSTANCE.Lower(name), FfiConverterServiceConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Service
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterServiceINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get detailed build information
func GetBuildInfo() BuildInfo {
	return FfiConverterBuildInfoINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_get_build_info(_uniffiStatus),
		}
	}))
}

// Get the global service instance (creates it if it doesn't exist)
//
// This returns a reference to the shared global service that can be used
// across the application. All calls to this function return the same service instance.
func GetGlobalService() *Service {
	return FfiConverterServiceINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_slim_bindings_fn_func_get_global_service(_uniffiStatus)
	}))
}

// Returns references to all global services.
// If not initialized, initializes with defaults first.
func GetServices() []*Service {
	return FfiConverterSequenceServiceINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_get_services(_uniffiStatus),
		}
	}))
}

// Get the version of the SLIM bindings (simple string)
func GetVersion() string {
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_get_version(_uniffiStatus),
		}
	}))
}

// Initialize SLIM bindings from a configuration file
//
// This function:
// 1. Loads the configuration file
// 2. Initializes the crypto provider
// 3. Sets up tracing/logging exactly as the main SLIM application does
// 4. Initializes the global runtime with configuration from the file
// 5. Initializes and starts the global service with servers/clients from config
//
// This must be called before using any SLIM bindings functionality.
// It's safe to call multiple times - subsequent calls will be ignored.
//
// # Arguments
// * `config_path` - Path to the YAML configuration file
//
// # Returns
// * `Ok(())` - Successfully initialized
// * `Err(SlimError)` - If initialization fails
//
// # Example
// ```ignore
// initialize_from_config("/path/to/config.yaml")?;
// ```
func InitializeFromConfig(configPath string) {
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_func_initialize_from_config(FfiConverterStringINSTANCE.Lower(configPath), _uniffiStatus)
		return false
	})
}

// Initialize SLIM bindings with custom configuration structs
//
// This function allows you to programmatically configure SLIM bindings by passing
// configuration structs directly, without needing a config file.
//
// # Arguments
// * `runtime_config` - Runtime configuration (thread count, naming, etc.)
// * `tracing_config` - Tracing/logging configuration
// * `service_config` - Service configuration (node ID, group name, etc.)
//
// # Returns
// * `Ok(())` - Successfully initialized
// * `Err(SlimError)` - If initialization fails
//
// # Example
// ```ignore
// let runtime_config = new_runtime_config();
// let tracing_config = new_tracing_config();
// let mut service_config = new_service_config();
// service_config.node_id = Some("my-node".to_string());
//
// initialize_with_configs(runtime_config, tracing_config, service_config)?;
// ```
func InitializeWithConfigs(runtimeConfig RuntimeConfig, tracingConfig TracingConfig, serviceConfig []ServiceConfig) error {
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_func_initialize_with_configs(FfiConverterRuntimeConfigINSTANCE.Lower(runtimeConfig), FfiConverterTracingConfigINSTANCE.Lower(tracingConfig), FfiConverterSequenceServiceConfigINSTANCE.Lower(serviceConfig), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Initialize SLIM bindings with default configuration
//
// This is a convenience function that initializes the bindings with:
// - Default runtime configuration
// - Default tracing/logging configuration
// - Initialized crypto provider
// - Default global service (no servers/clients)
//
// Use `initialize_from_config` for file-based configuration or
// `initialize_with_configs` for programmatic configuration.
func InitializeWithDefaults() {
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_func_initialize_with_defaults(_uniffiStatus)
		return false
	})
}

// Check if SLIM bindings have been initialized
func IsInitialized() bool {
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_slim_bindings_fn_func_is_initialized(_uniffiStatus)
	}))
}

// Create a new DataplaneConfig
func NewDataplaneConfig() DataplaneConfig {
	return FfiConverterDataplaneConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_dataplane_config(_uniffiStatus),
		}
	}))
}

// Create a new insecure client config (no TLS)
func NewInsecureClientConfig(endpoint string) ClientConfig {
	return FfiConverterClientConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_insecure_client_config(FfiConverterStringINSTANCE.Lower(endpoint), _uniffiStatus),
		}
	}))
}

// Create a new insecure server config (no TLS)
func NewInsecureServerConfig(endpoint string) ServerConfig {
	return FfiConverterServerConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_insecure_server_config(FfiConverterStringINSTANCE.Lower(endpoint), _uniffiStatus),
		}
	}))
}

// Create a new BindingsRuntimeConfig with default values
func NewRuntimeConfig() RuntimeConfig {
	return FfiConverterRuntimeConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_runtime_config(_uniffiStatus),
		}
	}))
}

// Create a new BindingsRuntimeConfig with custom values
func NewRuntimeConfigWith(nCores uint64, threadName string, drainTimeout time.Duration) RuntimeConfig {
	return FfiConverterRuntimeConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_runtime_config_with(FfiConverterUint64INSTANCE.Lower(nCores), FfiConverterStringINSTANCE.Lower(threadName), FfiConverterDurationINSTANCE.Lower(drainTimeout), _uniffiStatus),
		}
	}))
}

// Create a new server config with the given endpoint and default values
func NewServerConfig(endpoint string) ServerConfig {
	return FfiConverterServerConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_server_config(FfiConverterStringINSTANCE.Lower(endpoint), _uniffiStatus),
		}
	}))
}

// Create a new BindingsServiceConfig with default values
func NewServiceConfig() ServiceConfig {
	return FfiConverterServiceConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_service_config(_uniffiStatus),
		}
	}))
}

// Create a new BindingsServiceConfig with custom values
func NewServiceConfigWith(nodeId *string, groupName *string, dataplane DataplaneConfig) ServiceConfig {
	return FfiConverterServiceConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_service_config_with(FfiConverterOptionalStringINSTANCE.Lower(nodeId), FfiConverterOptionalStringINSTANCE.Lower(groupName), FfiConverterDataplaneConfigINSTANCE.Lower(dataplane), _uniffiStatus),
		}
	}))
}

// Create a new ServiceConfiguration
func NewServiceConfiguration() ServiceConfig {
	return FfiConverterServiceConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_service_configuration(_uniffiStatus),
		}
	}))
}

// Create a new BindingsTracingConfig with default values
func NewTracingConfig() TracingConfig {
	return FfiConverterTracingConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_tracing_config(_uniffiStatus),
		}
	}))
}

// Create a new BindingsTracingConfig with custom values
func NewTracingConfigWith(logLevel string, displayThreadNames bool, displayThreadIds bool, filters []string) TracingConfig {
	return FfiConverterTracingConfigINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_slim_bindings_fn_func_new_tracing_config_with(FfiConverterStringINSTANCE.Lower(logLevel), FfiConverterBoolINSTANCE.Lower(displayThreadNames), FfiConverterBoolINSTANCE.Lower(displayThreadIds), FfiConverterSequenceStringINSTANCE.Lower(filters), _uniffiStatus),
		}
	}))
}

// Perform graceful shutdown operations (blocking version)
//
// This is a blocking wrapper around the async `shutdown()` function for use from
// synchronous contexts or language bindings that don't support async.
//
// # Returns
// * `Ok(())` - Successfully shut down
// * `Err(SlimError)` - If shutdown fails
func ShutdownBlocking() error {
	_, _uniffiErr := rustCallWithError[SlimError](FfiConverterSlimError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_slim_bindings_fn_func_shutdown_blocking(_uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
