// Command clinepassproxy-connector builds the CPA plugin shared object.
//
// The plugin is control-plane only: it exposes ClinePassProxy's management API
// through CPA/CPAMP and registers no executor, scheduler, interceptor or
// translator, so no model request body crosses a CPA plugin RPC boundary.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static cliproxy_host_api stored_host;
static int host_ready;

static void store_host_api(const cliproxy_host_api* host) {
	if (host == NULL) {
		host_ready = 0;
		return;
	}
	stored_host = *host;
	host_ready = 1;
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"unsafe"

	"github.com/nonlog/ClinePassProxy/cpa-connector/internal/connector"
)

const abiVersion = 1

type rpcEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	ErrorCode  string `json:"code"`
	Message    string `json:"message"`
	Retry      bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

var (
	service   *connector.Service
	libraryMu sync.RWMutex
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) (rc C.int) {
	libraryMu.Lock()
	defer libraryMu.Unlock()
	defer func() {
		if recover() != nil {
			rc = 1
		}
	}()
	if host == nil || host.abi_version != C.uint32_t(abiVersion) || host.call == nil || host.free_buffer == nil || plugin == nil {
		return 1
	}
	if service != nil {
		return 1
	}
	C.store_host_api(host)
	service = connector.NewService()
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	libraryMu.RLock()
	defer libraryMu.RUnlock()
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeResponse(response, failure(&rpcError{ErrorCode: "plugin_panic", Message: "connector callback panicked", HTTPStatus: http.StatusInternalServerError}))
			rc = 1
		}
	}()
	if method == nil || strings.TrimSpace(C.GoString(method)) == "" {
		writeResponse(response, failure(&rpcError{ErrorCode: "invalid_method", Message: "method is required", HTTPStatus: http.StatusBadRequest}))
		return 1
	}
	if requestLen > C.size_t(math.MaxInt32) || (requestLen > 0 && request == nil) {
		writeResponse(response, failure(&rpcError{ErrorCode: "invalid_request", Message: "request buffer is invalid", HTTPStatus: http.StatusBadRequest}))
		return 1
	}
	var raw json.RawMessage
	if requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
		if !json.Valid(raw) {
			writeResponse(response, failure(&rpcError{ErrorCode: "invalid_request", Message: "request JSON is invalid", HTTPStatus: http.StatusBadRequest}))
			return 1
		}
	}
	if service == nil {
		writeResponse(response, failure(&rpcError{ErrorCode: "plugin_stopped", Message: "connector is not initialized", HTTPStatus: http.StatusServiceUnavailable}))
		return 1
	}
	result, err := service.Handle(C.GoString(method), raw)
	if err != nil {
		writeResponse(response, failure(errorDetails(err)))
		return 1
	}
	encoded, err := success(result)
	if err != nil {
		writeResponse(response, failure(&rpcError{ErrorCode: "serialization_error", Message: "connector response cannot be encoded", HTTPStatus: http.StatusInternalServerError}))
		return 1
	}
	writeResponse(response, encoded)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	libraryMu.Lock()
	defer libraryMu.Unlock()
	defer func() {
		service = nil
		C.store_host_api(nil)
		_ = recover()
	}()
	if service != nil {
		service.Shutdown()
	}
}

func success(result any) ([]byte, error) {
	if result == nil {
		result = struct{}{}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rpcEnvelope{OK: true, Result: raw})
}

func failure(details *rpcError) []byte {
	raw, err := json.Marshal(rpcEnvelope{OK: false, Error: details})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"serialization_error","message":"connector error cannot be encoded","http_status":500}}`)
	}
	return raw
}

func errorDetails(err error) *rpcError {
	details := &rpcError{ErrorCode: "connector_error", Message: err.Error(), HTTPStatus: http.StatusBadGateway}
	var status interface{ StatusCode() int }
	if errors.As(err, &status) && status.StatusCode() >= 400 && status.StatusCode() <= 599 {
		details.HTTPStatus = status.StatusCode()
	}
	var code interface{ Code() string }
	if errors.As(err, &code) && strings.TrimSpace(code.Code()) != "" {
		details.ErrorCode = code.Code()
	}
	return details
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

var _ = fmt.Sprintf
