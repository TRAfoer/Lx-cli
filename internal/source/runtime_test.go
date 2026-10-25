package source

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const header = "/*!\n * @name Test 音源\n * @version 1.2.3\n * @author Tester\n * @homepage https://example.test\n */\n"
const initScript = `lx.send(lx.EVENT_NAMES.inited, {status:true,sources:{wy:{qualitys:['128k','320k']},kw:{qualitys:['flac']}}});`

func callScript(t *testing.T, body string) json.RawMessage {
	t.Helper()
	data, err := NewRuntime().Call(context.Background(), header+initScript+`lx.on('request',async(req)=>{`+body+`});`, Request{Action: "musicUrl", Source: "wy", Info: map[string]any{"id": "123"}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInspectMetadataAndAsyncInitialization(t *testing.T) {
	script := header + `(async()=>{await Promise.resolve();await new Promise(resolve=>setTimeout(resolve,5));send(EVENT_NAMES.inited,{status:true,sources:{wy:{qualitys:['128k','320k']},kw:{qualities:['flac']}}});})();`
	m, err := NewRuntime().Inspect(context.Background(), script)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Test 音源" || m.Version != "1.2.3" || m.Author != "Tester" || m.Homepage != "https://example.test" {
		t.Fatalf("metadata: %#v", m)
	}
	if !reflect.DeepEqual(m.Platforms, []string{"kw", "wy"}) || !reflect.DeepEqual(m.Qualities["wy"], []string{"128k", "320k"}) || !reflect.DeepEqual(m.Qualities["kw"], []string{"flac"}) {
		t.Fatalf("manifest: %#v", m)
	}
}

func TestAsyncRequestAndHTTPResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("X-Test") != "present" {
			t.Errorf("request: %s %#v", r.Method, r.Header)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["id"] != "123" {
			t.Errorf("body: %#v %v", body, err)
		}
		w.Header().Set("X-Reply", "ok")
		w.WriteHeader(201)
		fmt.Fprint(w, `{"url":"https://example.test/音频.mp3"}`)
	}))
	defer srv.Close()
	script := header + initScript + fmt.Sprintf(`on('request',async({action,source,info})=>{const r=await new Promise((resolve,reject)=>request(%q,{method:'POST',headers:{'X-Test':'present'},body:JSON.stringify(info)},(e,r,b)=>e?reject(e):resolve(r)));return {action,source,url:r.body.url,status:r.statusCode,header:r.headers['x-reply'],raw:utils.buffer.bufToString(r.raw,'utf8')};});`, srv.URL)
	rt := NewRuntime()
	rt.HTTPClient = srv.Client()
	data, err := rt.Call(context.Background(), script, Request{Action: "musicUrl", Source: "wy", Info: map[string]any{"id": "123"}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["url"] != "https://example.test/音频.mp3" || got["status"] != float64(201) || got["header"] != "ok" || got["action"] != "musicUrl" || !strings.Contains(got["raw"].(string), "音频") {
		t.Fatalf("result: %s", data)
	}
}

func TestHTTPFetchPromiseAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) }))
	defer srv.Close()
	body := fmt.Sprintf(`const r=await httpFetch(%q,{}).promise;return r.body.ok;`, srv.URL)
	if got := string(callScript(t, body)); got != "true" {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewRuntime().Call(ctx, header+initScript+`on('request',()=>42)`, Request{}); err == nil {
		t.Fatal("canceled operation succeeded")
	}
}

func TestBufferCryptoAndZlib(t *testing.T) {
	data := callScript(t, `const {buffer:b,crypto:c,zlib:z}=utils;const original=b.from('你好\u0000 music'); const restored=z.inflate(z.deflate(original));const arr=new Uint8Array([0,1,254,255]);return {text:b.bufToString(restored),hex:b.bufToString(arr,'hex'),base64:b.bufToString(b.from('YWJjZA','base64'),'utf8'),md5:c.md5('abc'),random:c.randomBytes(16).byteLength,aes:b.bufToString(c.aesEncrypt(b.from('6bc1bee22e409f96e93d7e117393172a','hex'),'aes-128-cbc',b.from('2b7e151628aed2a6abf7158809cf4f3c','hex'),b.from('000102030405060708090a0b0c0d0e0f','hex')),'hex'),ecb:b.bufToString(c.aesEncrypt(b.from('6bc1bee22e409f96e93d7e117393172a','hex'),'aes-128-ecb',b.from('2b7e151628aed2a6abf7158809cf4f3c','hex'),b.from('')),'hex')};`)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "你好\x00 music" || got["hex"] != "0001feff" || got["base64"] != "abcd" || got["md5"] != "900150983cd24fb0d6963f7d28e17f72" || got["random"] != float64(16) {
		t.Fatalf("result: %s", data)
	}
	if !strings.HasPrefix(got["aes"].(string), "7649abac8119b246cee98e9b12e9197d") || !strings.HasPrefix(got["ecb"].(string), "3ad77bb40d7a3660a89ecaf32466ef97") {
		t.Fatalf("AES vectors: %s", data)
	}
}

func TestBabelRegeneratorPromise(t *testing.T) {
	data := callScript(t, `const run=regeneratorRuntime.mark(function run(){return regeneratorRuntime.wrap(function run$(_context){while(1)switch(_context.prev=_context.next){case 0:_context.next=2;return Promise.resolve('generated');case 2:return _context.abrupt('return',_context.sent+' async');case 3:case 'end':return _context.stop();}},run);});return _asyncToGenerator(run)();`)
	if string(data) != `"generated async"` {
		t.Fatalf("regenerator result: %s", data)
	}
}

func TestHTTPInitializationDoesNotBlockSynchronousInited(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(stopped) }))
	defer srv.Close()
	script := header + fmt.Sprintf(`request(%q,{},()=>{});`, srv.URL) + initScript
	rt := NewRuntime()
	rt.Timeout = time.Second
	start := time.Now()
	if _, err := rt.Inspect(context.Background(), script); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("inited was delayed by background request")
	}
	// The request may be canceled before it even reaches the local listener.
	select {
	case <-started:
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("background HTTP did not stop")
		}
	default:
	}
}

func TestRSAContractPEMRawAndLegacyModulus(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	body := fmt.Sprintf(`const b=utils.buffer,c=utils.crypto;return {raw:b.bufToString(c.rsaEncrypt(b.from('hello'),%q),'hex'),legacy:b.bufToString(c.rsaEncrypt(b.from('hello'),%q),'hex')};`, pub, key.N.Text(16))
	data := callScript(t, body)
	var got map[string]string
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(got["raw"])
	if err != nil {
		t.Fatal(err)
	}
	want := new(big.Int).Exp(new(big.Int).SetBytes([]byte("hello")), big.NewInt(int64(key.E)), key.N)
	if len(raw) != key.Size() || new(big.Int).SetBytes(raw).Cmp(want) != 0 {
		t.Fatalf("raw RSA mismatch: %s", data)
	}
	enc, err := hex.DecodeString(got["legacy"])
	if err != nil {
		t.Fatal(err)
	}
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, key, enc)
	if err != nil || string(plain) != "hello" {
		t.Fatalf("legacy RSA: %q %v", plain, err)
	}
}

func TestErrorsAreReturned(t *testing.T) {
	for _, tc := range []struct {
		name, script, contains string
		inspect                bool
	}{
		{"metadata", "<html>gateway</html>", "metadata", true},
		{"missing init", header + `console.log('hello')`, "inited", true},
		{"failed init", header + `send('inited',{status:false,sources:{},message:'denied'});`, "denied", true},
		{"syntax", header + `function {`, "SyntaxError", false},
		{"rejected", header + initScript + `on('request',async()=>{throw new Error('remote denied')})`, "remote denied", false},
		{"invalid crypto", header + initScript + `on('request',()=>utils.crypto.aesEncrypt(utils.buffer.from('x'),'aes-128-cbc','bad','bad'))`, "key", false},
		{"invalid zlib", header + initScript + `on('request',()=>utils.zlib.inflate(utils.buffer.from('invalid')))`, "inflate", false},
		{"invalid random", header + initScript + `on('request',()=>utils.crypto.randomBytes(-1))`, "randomBytes", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := NewRuntime()
			var err error
			if tc.inspect {
				_, err = rt.Inspect(context.Background(), tc.script)
			} else {
				_, err = rt.Call(context.Background(), tc.script, Request{})
			}
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v, want %q", err, tc.contains)
			}
		})
	}
}

func TestTimeoutsForJSAndHTTP(t *testing.T) {
	for _, script := range []string{`while(true){}`, initScript + `on('request',()=>{function loop(){return Promise.resolve().then(loop)};return loop()})`, initScript + `on('request',()=>new Promise(()=>{}))`} {
		rt := NewRuntime()
		rt.Timeout = 30 * time.Millisecond
		start := time.Now()
		_, err := rt.Call(context.Background(), header+script, Request{})
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("timeout: %v elapsed=%v", err, time.Since(start))
		}
	}
	stopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(stopped) }))
	defer srv.Close()
	rt := NewRuntime()
	rt.Timeout = 50 * time.Millisecond
	script := header + initScript + fmt.Sprintf(`on('request',()=>httpFetch(%q,{}).promise)`, srv.URL)
	if _, err := rt.Call(context.Background(), script, Request{}); err == nil {
		t.Fatal("HTTP did not time out")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("HTTP context not canceled")
	}
}

func TestHTTPSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", 9<<20))) }))
	defer srv.Close()
	rt := NewRuntime()
	script := header + initScript + fmt.Sprintf(`on('request',()=>httpFetch(%q,{}).promise)`, srv.URL)
	if _, err := rt.Call(context.Background(), script, Request{}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error: %v", err)
	}
}

func TestRuntimeOperationsAreIndependent(t *testing.T) {
	rt := NewRuntime()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			script := header + initScript + fmt.Sprintf(`var isolated=%d;on('request',async()=>{await Promise.resolve();return isolated});`, i)
			data, err := rt.Call(context.Background(), script, Request{})
			if err != nil || string(data) != fmt.Sprint(i) {
				t.Errorf("%d: %s %v", i, data, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestChangqingRealInitialization(t *testing.T) {
	script, err := os.ReadFile("testdata/changqing-fixed.js")
	if err != nil {
		t.Skip(err)
	}
	m, err := NewRuntime().Inspect(context.Background(), string(script))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "长青SVIP音源(二改修复版)" || !reflect.DeepEqual(m.Platforms, []string{"kg", "kw", "tx", "wy"}) {
		t.Fatalf("manifest: %#v", m)
	}
	t.Logf("real script manifest: %+v", m)
}

func TestChangqingRealURLCall(t *testing.T) {
	if os.Getenv("ALX_SOURCE_LIVE_TEST") != "1" {
		t.Skip("set ALX_SOURCE_LIVE_TEST=1 for a real remote URL attempt")
	}
	script, err := os.ReadFile("testdata/changqing-fixed.js")
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime()
	rt.Timeout = 12 * time.Second
	data, err := rt.Call(context.Background(), string(script), Request{Action: "musicUrl", Source: "wy", Info: map[string]any{"type": "128k", "musicInfo": map[string]any{"id": "186016", "songmid": "186016", "name": "晴天", "singer": "周杰伦"}}})
	if err != nil {
		t.Logf("REAL REMOTE URL FAILED: %v", err)
		return
	}
	t.Logf("REAL REMOTE URL RESULT: %s", data)
}
