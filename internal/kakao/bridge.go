package kakao

/*
#cgo CFLAGS: -I${SRCDIR}/../../bridge/include
#include <stdlib.h>
#include "aside_bridge.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"
)

// The Swift side keeps per-window caches and AX calls can block on an
// unresponsive KakaoTalk, so all requests are serialized here.
var bridgeMu sync.Mutex

type response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func request(action string, params map[string]any, out any) error {
	payload := map[string]any{"action": action}
	for k, v := range params {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("kakao bridge: encode %s: %w", action, err)
	}

	bridgeMu.Lock()
	cReq := C.CString(string(body))
	cRes := C.aside_request(cReq)
	C.free(unsafe.Pointer(cReq))
	raw := C.GoString(cRes)
	C.aside_free(cRes)
	bridgeMu.Unlock()

	var res response
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return fmt.Errorf("kakao bridge: decode %s response: %w", action, err)
	}
	if !res.OK {
		return fmt.Errorf("kakao bridge: %s: %s", action, res.Error)
	}
	if out != nil {
		if err := json.Unmarshal(res.Result, out); err != nil {
			return fmt.Errorf("kakao bridge: decode %s result: %w", action, err)
		}
	}
	return nil
}
