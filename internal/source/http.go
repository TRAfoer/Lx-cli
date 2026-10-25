package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
)

const maxHTTPResponseBytes = 8 << 20

// request schedules only network work outside the VM. The callback, parsing,
// ArrayBuffer creation and all Promise reactions run on the session's goroutine.
func (s *session) request(call goja.FunctionCall) goja.Value {
	vm := s.vm
	callback, ok := goja.AssertFunction(call.Argument(2))
	if !ok {
		panic(vm.NewTypeError("lx.request requires URL, options and callback"))
	}
	if s.pending >= maxPendingTasks {
		panic(vm.NewTypeError("pending task limit exceeded"))
	}
	opts := call.Argument(1)
	if goja.IsNull(opts) || goja.IsUndefined(opts) {
		opts = vm.NewObject()
	}
	obj := opts.ToObject(vm)
	address := call.Argument(0).String()
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		panic(vm.NewTypeError("lx.request requires an HTTP(S) URL"))
	}
	if query := obj.Get("qs"); isDefined(query) {
		values := parsed.Query()
		q := query.ToObject(vm)
		for _, key := range q.Keys() {
			values.Set(key, q.Get(key).String())
		}
		parsed.RawQuery = values.Encode()
	}
	method := "GET"
	if v := obj.Get("method"); isDefined(v) {
		method = strings.ToUpper(v.String())
	}
	var body []byte
	if v := obj.Get("body"); isDefined(v) {
		body, err = s.bytes(v, true)
		if err != nil {
			encoded, e := s.stringify(v)
			if e != nil {
				panic(vm.NewTypeError("request body: %s", err))
			}
			body = encoded
		}
	}
	contentType := ""
	if form := obj.Get("form"); isDefined(form) {
		values := url.Values{}
		f := form.ToObject(vm)
		for _, key := range f.Keys() {
			values.Set(key, f.Get(key).String())
		}
		body = []byte(values.Encode())
		contentType = "application/x-www-form-urlencoded"
	}
	if len(body) > maxHostBytes {
		panic(vm.NewTypeError("request body exceeds size limit"))
	}
	ctx, cancel := context.WithCancel(s.ctx)
	if timeout := obj.Get("timeout"); isDefined(timeout) && timeout.ToInteger() > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(s.ctx, time.Duration(min(timeout.ToInteger(), int64(2147483647)))*time.Millisecond)
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), bytes.NewReader(body))
	if err != nil {
		cancel()
		panic(vm.NewTypeError("HTTP request: %s", err))
	}
	if headers := obj.Get("headers"); isDefined(headers) {
		h := headers.ToObject(vm)
		for _, key := range h.Keys() {
			req.Header.Set(key, h.Get(key).String())
		}
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Lx-Cli/0.1.1-go")
	}
	s.pending++
	go func() {
		defer cancel()
		var raw []byte
		response, requestErr := s.client.Do(req)
		if requestErr == nil {
			if response.ContentLength > maxHTTPResponseBytes {
				requestErr = fmt.Errorf("HTTP response exceeds %d byte limit", maxHTTPResponseBytes)
			} else {
				raw, requestErr = io.ReadAll(io.LimitReader(response.Body, maxHTTPResponseBytes+1))
				if len(raw) > maxHTTPResponseBytes {
					requestErr = fmt.Errorf("HTTP response exceeds %d byte limit", maxHTTPResponseBytes)
				}
			}
			response.Body.Close()
		}
		s.enqueue(func() error {
			s.pending--
			if requestErr != nil {
				_, err := callback(goja.Undefined(), vm.ToValue(vm.NewGoError(requestErr)), goja.Null(), goja.Null())
				return err
			}
			resp := vm.NewObject()
			resp.Set("statusCode", response.StatusCode)
			resp.Set("status", response.StatusCode)
			headers := map[string]string{}
			for k, values := range response.Header {
				headers[strings.ToLower(k)] = strings.Join(values, ", ")
			}
			resp.Set("headers", headers)
			bodyValue := goja.Null()
			if utf8.Valid(raw) {
				bodyValue = vm.ToValue(string(raw))
				if json.Valid(raw) {
					var decoded any
					if err := json.Unmarshal(raw, &decoded); err == nil {
						bodyValue = vm.ToValue(decoded)
					}
				}
			}
			resp.Set("body", bodyValue)
			resp.Set("raw", vm.NewArrayBuffer(raw))
			_, err := callback(goja.Undefined(), goja.Null(), resp, bodyValue)
			return err
		})
	}()
	return vm.ToValue(func() { cancel() })
}

func isDefined(v goja.Value) bool { return v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) }
