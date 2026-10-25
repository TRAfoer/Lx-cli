// Package source embeds the LX music source protocol in a pure Go JavaScript VM.
// The protocol and crypto contracts follow the MIT-licensed alx Rust bridge.
package source

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
)

//go:embed compat.js
var generatorPrelude string

const (
	defaultTimeout  = 30 * time.Second
	maxScriptBytes  = 8 << 20
	maxHostBytes    = 64 << 20
	maxPendingTasks = 128
)

type Request struct {
	Action string         `json:"action"`
	Source string         `json:"source"`
	Info   map[string]any `json:"info"`
}

type Manifest struct {
	Name      string              `json:"name"`
	Version   string              `json:"version"`
	Author    string              `json:"author"`
	Homepage  string              `json:"homepage"`
	Platforms []string            `json:"platforms"`
	Qualities map[string][]string `json:"qualities"`
}

// Runtime is reusable; every operation gets a separate VM. Set these fields
// before starting concurrent operations. Goja itself is accessed on one goroutine.
// Goja is not an OS sandbox and does not provide a per-instance hard heap limit.
type Runtime struct {
	HTTPClient *http.Client
	Timeout    time.Duration
}

func NewRuntime() *Runtime { return &Runtime{HTTPClient: &http.Client{}, Timeout: defaultTimeout} }

type session struct {
	vm         *goja.Runtime
	ctx        context.Context
	client     *http.Client
	events     chan func() error
	pending    int
	nextTimer  int
	timers     map[int]*time.Timer
	handler    goja.Callable
	inited     goja.Value
	rejections map[*goja.Promise]bool
}

func (r *Runtime) Inspect(ctx context.Context, script string) (manifest Manifest, err error) {
	defer recoverError(&err)
	fields, err := metadata(script)
	if err != nil {
		return manifest, err
	}
	err = r.run(ctx, script, fields, func(s *session) error {
		for s.inited == nil {
			if s.pending == 0 {
				return s.initError("script did not send inited")
			}
			if err := s.step(); err != nil {
				return err
			}
		}
		data, err := s.initialized()
		if err != nil {
			return err
		}
		var result struct {
			Sources map[string]json.RawMessage `json:"sources"`
		}
		if err = json.Unmarshal(data, &result); err != nil || result.Sources == nil {
			return errors.New("inited sources must be an object")
		}
		manifest = Manifest{Name: fields["name"], Version: fields["version"], Author: fields["author"], Homepage: fields["homepage"], Platforms: []string{}, Qualities: map[string][]string{}}
		for name, raw := range result.Sources {
			var platform struct {
				Qualitys  []string `json:"qualitys"`
				Qualities []string `json:"qualities"`
			}
			if err = json.Unmarshal(raw, &platform); err != nil {
				return fmt.Errorf("invalid source %s: %w", name, err)
			}
			manifest.Platforms = append(manifest.Platforms, name)
			qualities := platform.Qualitys
			if qualities == nil {
				qualities = platform.Qualities
			}
			if qualities != nil {
				manifest.Qualities[name] = qualities
			}
		}
		sort.Strings(manifest.Platforms)
		return nil
	})
	return manifest, err
}

func (r *Runtime) Call(ctx context.Context, script string, req Request) (result json.RawMessage, err error) {
	defer recoverError(&err)
	fields, _ := metadata(script)
	err = r.run(ctx, script, fields, func(s *session) error {
		for s.handler == nil || (s.inited == nil && s.pending > 0) {
			if s.pending == 0 {
				return s.initError("script did not register a request handler")
			}
			if err := s.step(); err != nil {
				return err
			}
		}
		if s.inited != nil {
			if _, err := s.initialized(); err != nil {
				return err
			}
		}
		if req.Info == nil {
			req.Info = map[string]any{}
		}
		// JSON decoding creates ordinary JS objects, including nested map values.
		requestJSON, e := json.Marshal(req)
		if e != nil {
			return fmt.Errorf("request JSON: %w", e)
		}
		requestValue, e := s.vm.RunString("(" + string(requestJSON) + ")")
		if e != nil {
			return e
		}
		value, e := s.handler(goja.Undefined(), requestValue)
		if e != nil {
			return e
		}
		// Promise.resolve also assimilates custom thenables returned by scripts.
		resolve, e := s.vm.RunString("value => Promise.resolve(value)")
		if e != nil {
			return e
		}
		resolveFn, _ := goja.AssertFunction(resolve)
		promiseValue, e := resolveFn(goja.Undefined(), value)
		if e != nil {
			return e
		}
		promise, ok := promiseValue.Export().(*goja.Promise)
		if !ok {
			return errors.New("request did not produce a Promise")
		}
		for promise.State() == goja.PromiseStatePending {
			if e = s.step(); e != nil {
				return e
			}
		}
		if promise.State() == goja.PromiseStateRejected {
			return fmt.Errorf("source request rejected: %s", valueMessage(promise.Result()))
		}
		result, e = s.stringify(promise.Result())
		return e
	})
	return result, err
}

func (r *Runtime) run(ctx context.Context, script string, fields map[string]string, work func(*session) error) (err error) {
	if len(script) > maxScriptBytes {
		return errors.New("source script exceeds size limit")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return err
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(2048)
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	s := &session{vm: vm, ctx: ctx, client: client, events: make(chan func() error, maxPendingTasks), timers: map[int]*time.Timer{}, rejections: map[*goja.Promise]bool{}}
	vm.SetPromiseRejectionTracker(func(p *goja.Promise, operation goja.PromiseRejectionOperation) {
		if operation == goja.PromiseRejectionReject {
			s.rejections[p] = true
		} else {
			delete(s.rejections, p)
		}
	})
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-done:
		}
	}()
	defer close(done)
	defer func() {
		for _, timer := range s.timers {
			timer.Stop()
		}
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("source execution canceled or timed out: %w", ctx.Err())
		}
	}()
	if err = s.inject(script, fields); err != nil {
		return err
	}
	if _, err = vm.RunString(script); err != nil {
		return fmt.Errorf("source script: %w", err)
	}
	if err = work(s); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (s *session) step() error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case event := <-s.events:
		return event()
	}
}

func (s *session) enqueue(event func() error) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

func (s *session) initialized() (json.RawMessage, error) {
	data, err := s.stringify(s.inited)
	if err != nil {
		return nil, fmt.Errorf("inited JSON: %w", err)
	}
	var result struct {
		Status  *bool  `json:"status"`
		Message string `json:"message"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid inited: %w", err)
	}
	if result.Status != nil && !*result.Status {
		if result.Message == "" {
			result.Message = "source initialization failed"
		}
		return nil, errors.New(result.Message)
	}
	return data, nil
}

func (s *session) initError(fallback string) error {
	for p := range s.rejections {
		return fmt.Errorf("source initialization rejected: %s", valueMessage(p.Result()))
	}
	return errors.New(fallback)
}

func (s *session) stringify(v goja.Value) (json.RawMessage, error) {
	fn, err := s.vm.RunString("value => JSON.stringify(value)")
	if err != nil {
		return nil, err
	}
	call, _ := goja.AssertFunction(fn)
	encoded, err := call(goja.Undefined(), v)
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(encoded) {
		return nil, errors.New("source returned undefined instead of JSON")
	}
	return json.RawMessage(encoded.String()), nil
}

func valueMessage(v goja.Value) string {
	if obj, ok := v.(*goja.Object); ok {
		if msg := obj.Get("message"); msg != nil && !goja.IsUndefined(msg) {
			return msg.String()
		}
	}
	return v.String()
}

func recoverError(err *error) {
	if p := recover(); p != nil {
		*err = fmt.Errorf("source runtime error: %v", p)
	}
}

func metadata(script string) (map[string]string, error) {
	start := strings.Index(script, "/*")
	if start < 0 {
		return nil, errors.New("source metadata comment is missing")
	}
	end := strings.Index(script[start+2:], "*/")
	if end < 0 {
		return nil, errors.New("source metadata comment is unterminated")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(script[start+2:start+2+end], "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "*!"))
		if !strings.HasPrefix(line, "@") {
			continue
		}
		parts := strings.Fields(line[1:])
		if len(parts) < 2 {
			continue
		}
		key := parts[0]
		fields[key] = strings.TrimSpace(strings.TrimPrefix(line, "@"+key))
	}
	if fields["name"] == "" {
		return nil, errors.New("source metadata @name is missing")
	}
	return fields, nil
}

func (s *session) inject(script string, fields map[string]string) error {
	vm := s.vm
	lx := vm.NewObject()
	lx.Set("EVENT_NAMES", map[string]string{"inited": "inited", "request": "request", "response": "response", "updateAlert": "updateAlert"})
	lx.Set("env", "cli")
	lx.Set("version", "2.0.0")
	lx.Set("currentScriptInfo", map[string]string{"name": fields["name"], "description": fields["description"], "version": fields["version"], "author": fields["author"], "homepage": fields["homepage"], "rawScript": script})
	lx.Set("on", func(call goja.FunctionCall) goja.Value {
		if call.Argument(0).String() == "request" {
			fn, ok := goja.AssertFunction(call.Argument(1))
			if !ok {
				panic(vm.NewTypeError("request handler must be a function"))
			}
			s.handler = fn
		}
		return goja.Undefined()
	})
	lx.Set("send", func(call goja.FunctionCall) goja.Value {
		if call.Argument(0).String() == "inited" {
			s.inited = call.Argument(1)
		}
		return goja.Undefined()
	})
	lx.Set("request", s.request)
	lx.Set("utils", s.utilities())
	if err := vm.Set("lx", lx); err != nil {
		return err
	}
	console := vm.NewObject()
	for _, name := range []string{"log", "info", "warn", "error", "debug", "group", "groupEnd"} {
		console.Set(name, func(goja.FunctionCall) goja.Value { return goja.Undefined() })
	}
	vm.Set("console", console)
	vm.Set("setTimeout", func(call goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			panic(vm.NewTypeError("setTimeout callback must be a function"))
		}
		if s.pending >= maxPendingTasks {
			panic(vm.NewTypeError("pending task limit exceeded"))
		}
		ms := call.Argument(1).ToInteger()
		if ms < 0 {
			ms = 0
		}
		if ms > 2147483647 {
			ms = 2147483647
		}
		s.nextTimer++
		id := s.nextTimer
		s.pending++
		args := append([]goja.Value(nil), call.Arguments[min(2, len(call.Arguments)):]...)
		s.timers[id] = time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
			s.enqueue(func() error {
				if _, ok := s.timers[id]; !ok {
					return nil
				}
				delete(s.timers, id)
				s.pending--
				_, err := fn(goja.Undefined(), args...)
				return err
			})
		})
		return vm.ToValue(id)
	})
	vm.Set("clearTimeout", func(call goja.FunctionCall) goja.Value {
		id := int(call.Argument(0).ToInteger())
		if timer, ok := s.timers[id]; ok {
			timer.Stop()
			delete(s.timers, id)
			s.pending--
		}
		return goja.Undefined()
	})
	if _, err := vm.RunString(hostPrelude); err != nil {
		return err
	}
	_, err := vm.RunString(generatorPrelude)
	return err
}

const hostPrelude = `
globalThis.global=globalThis;
for (const key of ['request','send','on','utils','env','version','EVENT_NAMES']) {
 Object.defineProperty(globalThis,key,{configurable:true,get(){return lx[key]},set(value){Object.defineProperty(globalThis,key,{value,writable:true,configurable:true})}});
}
Object.defineProperty(globalThis,'httpFetch',{configurable:true,get(){return function(url,options,callback){
 if(typeof options==='function'){callback=options;options={}}
 if(typeof callback==='function')return lx.request(url,options||{},callback);
 let cancel;
 const promise=new Promise((resolve,reject)=>{cancel=lx.request(url,options||{},(err,response)=>err?reject(err):resolve(response))});
 return {promise,cancel};
}},set(value){Object.defineProperty(globalThis,'httpFetch',{value,writable:true,configurable:true})}});
globalThis._typeof = value => typeof value;
globalThis._defineProperty = (object,key,value) => {Object.defineProperty(object,key,{value,enumerable:true,configurable:true,writable:true});return object;};
globalThis.DEV_ENABLE=false;globalThis.UPDATE_ENABLE=false;
globalThis.MUSIC_QUALITY={};globalThis.MUSIC_SOURCE={};globalThis.API_URL='';globalThis.API_KEY='';
`
