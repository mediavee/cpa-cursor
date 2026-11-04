// Command plugin is the c-shared entry point CPA loads: it only moves bytes between the C ABI and
// package plugin, which holds all the behaviour.
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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/mediavee/cpa-cursor/plugin"
)

// A panic in any goroutine of the plugin's Go runtime kills CPA: the host cannot recover it. Every
// export that runs Go code recovers, and package plugin guards the goroutines it starts.

var impl = plugin.New(hostCall)

func main() {}

// cliproxy_plugin_init also runs in the host's preflight child, so it only stores pointers.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) (status C.int) {
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if api == nil {
		return 1
	}
	C.store_host_api(host)
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (status C.int) {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	defer func() {
		if v := recover(); v != nil {
			writeResponse(response, plugin.ErrorEnvelope(pluginabi.NewError("plugin_panic", fmt.Sprint(v), 500)))
			status = 1
		}
	}()
	var req []byte
	if request != nil && requestLen > 0 {
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeResponse(response, impl.Call(C.GoString(method), req))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	C.free(ptr)
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	impl.Shutdown()
}

// hostCall calls one of CPA's host methods; it is safe from any goroutine.
func hostCall(method string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	cReq := C.CBytes(raw)
	defer C.free(cReq)
	var resp C.cliproxy_buffer
	C.call_host_api(cMethod, (*C.uint8_t)(cReq), C.size_t(len(raw)), &resp) //nolint:gocritic // false positive in cgo's generated pointer check
	var out []byte
	if resp.ptr != nil {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
		C.free_host_buffer(resp.ptr, resp.len)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("%s: unreadable host answer", method)
	}
	if !env.OK {
		if env.Error == nil {
			return nil, errors.New(method + ": failed")
		}
		return nil, env.Error
	}
	return env.Result, nil
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}
