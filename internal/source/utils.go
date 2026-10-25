package source

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dop251/goja"
)

func (s *session) bytes(v goja.Value, allowString bool) ([]byte, error) {
	if v == nil || goja.IsNull(v) || goja.IsUndefined(v) {
		return nil, errors.New("expected string or binary buffer")
	}
	exported := v.Export()
	switch data := exported.(type) {
	case string:
		if allowString {
			if len(data) > maxHostBytes {
				return nil, errors.New("buffer exceeds size limit")
			}
			return []byte(data), nil
		}
	case goja.ArrayBuffer:
		if len(data.Bytes()) > maxHostBytes {
			return nil, errors.New("buffer exceeds size limit")
		}
		return append([]byte(nil), data.Bytes()...), nil
	case []uint8:
		if len(data) > maxHostBytes {
			return nil, errors.New("buffer exceeds size limit")
		}
		return append([]byte(nil), data...), nil
	case []interface{}:
		if len(data) > maxHostBytes {
			return nil, errors.New("buffer exceeds size limit")
		}
		out := make([]byte, len(data))
		for i, item := range data {
			number, ok := item.(int64)
			if !ok || number < 0 || number > 255 {
				return nil, errors.New("buffer array must contain byte values")
			}
			out[i] = byte(number)
		}
		return out, nil
	}
	return nil, errors.New("expected string, ArrayBuffer or Uint8Array")
}

func (s *session) mustBytes(v goja.Value, allowString bool) []byte {
	data, err := s.bytes(v, allowString)
	if err != nil {
		panic(s.vm.NewTypeError("%s", err))
	}
	return data
}

func (s *session) utilities() *goja.Object {
	vm := s.vm
	utils := vm.NewObject()
	buffer := vm.NewObject()
	crypto := vm.NewObject()
	compression := vm.NewObject()
	buffer.Set("from", func(call goja.FunctionCall) goja.Value {
		value := call.Argument(0)
		enc := strings.ToLower(call.Argument(1).String())
		var data []byte
		var err error
		if text, ok := value.Export().(string); ok {
			if len(text) > maxHostBytes {
				panic(vm.NewTypeError("buffer exceeds size limit"))
			}
			switch enc {
			case "hex":
				data, err = hex.DecodeString(text)
			case "base64":
				text = strings.Map(func(r rune) rune {
					if unicode.IsSpace(r) {
						return -1
					}
					return r
				}, text)
				data, err = base64.StdEncoding.DecodeString(text)
				if err != nil {
					data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(text, "="))
				}
			case "utf8", "utf-8", "undefined", "":
				data = []byte(text)
			default:
				err = fmt.Errorf("unsupported buffer encoding %s", enc)
			}
		} else {
			data, err = s.bytes(value, false)
		}
		if err != nil {
			panic(vm.NewTypeError("buffer.from: %s", err))
		}
		return vm.ToValue(vm.NewArrayBuffer(data))
	})
	buffer.Set("bufToString", func(call goja.FunctionCall) goja.Value {
		data := s.mustBytes(call.Argument(0), false)
		enc := strings.ToLower(call.Argument(1).String())
		switch enc {
		case "hex":
			return vm.ToValue(hex.EncodeToString(data))
		case "base64":
			return vm.ToValue(base64.StdEncoding.EncodeToString(data))
		case "utf8", "utf-8", "undefined", "":
			return vm.ToValue(strings.ToValidUTF8(string(data), "\uFFFD"))
		default:
			panic(vm.NewTypeError("unsupported buffer encoding %s", enc))
		}
	})
	crypto.Set("md5", func(call goja.FunctionCall) goja.Value {
		data := s.mustBytes(call.Argument(0), true)
		hash := md5.Sum(data)
		return vm.ToValue(hex.EncodeToString(hash[:]))
	})
	crypto.Set("randomBytes", func(call goja.FunctionCall) goja.Value {
		n := call.Argument(0).ToInteger()
		if n < 0 || n > maxHostBytes {
			panic(vm.NewTypeError("randomBytes size must be between 0 and %d", maxHostBytes))
		}
		data := make([]byte, int(n))
		if _, err := rand.Read(data); err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(vm.NewArrayBuffer(data))
	})
	crypto.Set("aesEncrypt", func(call goja.FunctionCall) goja.Value {
		data := s.mustBytes(call.Argument(0), true)
		mode := strings.ToLower(call.Argument(1).String())
		key := s.mustBytes(call.Argument(2), true)
		block, err := aes.NewCipher(key)
		if err != nil {
			panic(vm.NewTypeError("AES key: %s", err))
		}
		if !strings.Contains(mode, "cbc") && !strings.Contains(mode, "ecb") {
			panic(vm.NewTypeError("unsupported AES mode %s", mode))
		}
		if len(data) > maxHostBytes-aes.BlockSize {
			panic(vm.NewTypeError("AES data exceeds size limit"))
		}
		padding := aes.BlockSize - len(data)%aes.BlockSize
		padded := make([]byte, len(data)+padding)
		copy(padded, data)
		for i := len(data); i < len(padded); i++ {
			padded[i] = byte(padding)
		}
		out := make([]byte, len(padded))
		if strings.Contains(mode, "cbc") {
			iv := s.mustBytes(call.Argument(3), true)
			if len(iv) != aes.BlockSize {
				panic(vm.NewTypeError("AES CBC IV must be 16 bytes"))
			}
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
		} else {
			for i := 0; i < len(padded); i += aes.BlockSize {
				block.Encrypt(out[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
			}
		}
		return vm.ToValue(vm.NewArrayBuffer(out))
	})
	crypto.Set("rsaEncrypt", func(call goja.FunctionCall) goja.Value {
		data := s.mustBytes(call.Argument(0), true)
		key := call.Argument(1).String()
		out, err := encryptRSA(data, key)
		if err != nil {
			panic(vm.NewTypeError("rsaEncrypt: %s", err))
		}
		return vm.ToValue(vm.NewArrayBuffer(out))
	})
	compression.Set("deflate", func(call goja.FunctionCall) goja.Value {
		data := s.mustBytes(call.Argument(0), true)
		var out bytes.Buffer
		w := zlib.NewWriter(&out)
		if _, err := w.Write(data); err != nil {
			panic(vm.NewGoError(err))
		}
		if err := w.Close(); err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(vm.NewArrayBuffer(out.Bytes()))
	})
	compression.Set("inflate", func(call goja.FunctionCall) goja.Value {
		data := s.mustBytes(call.Argument(0), false)
		reader, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			reader, err = gzip.NewReader(bytes.NewReader(data))
		}
		if err != nil {
			panic(vm.NewTypeError("inflate: %s", err))
		}
		defer reader.Close()
		out, err := io.ReadAll(io.LimitReader(reader, maxHostBytes+1))
		if err != nil {
			panic(vm.NewTypeError("inflate: %s", err))
		}
		if len(out) > maxHostBytes {
			panic(vm.NewTypeError("inflate output exceeds size limit"))
		}
		return vm.ToValue(vm.NewArrayBuffer(out))
	})
	utils.Set("buffer", buffer)
	utils.Set("crypto", crypto)
	utils.Set("zlib", compression)
	return utils
}

func encryptRSA(data []byte, key string) ([]byte, error) {
	if strings.HasPrefix(strings.TrimSpace(key), "-----BEGIN") {
		block, _ := pem.Decode([]byte(key))
		if block == nil {
			return nil, errors.New("invalid RSA PEM")
		}
		var pub *rsa.PublicKey
		if parsed, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
			pub, _ = parsed.(*rsa.PublicKey)
		}
		if pub == nil {
			pub, _ = x509.ParsePKCS1PublicKey(block.Bytes)
		}
		if pub == nil {
			return nil, errors.New("invalid RSA public key")
		}
		if pub.Size() > 16384 {
			return nil, errors.New("RSA modulus exceeds size limit")
		}
		if len(data) > pub.Size() {
			return nil, errors.New("input exceeds RSA block size")
		}
		message := new(big.Int).SetBytes(data)
		if message.Cmp(pub.N) >= 0 {
			return nil, errors.New("RSA input is not smaller than modulus")
		}
		encrypted := new(big.Int).Exp(message, big.NewInt(int64(pub.E)), pub.N)
		return encrypted.FillBytes(make([]byte, pub.Size())), nil
	}
	if !utf8.ValidString(key) || len(key) > 32768 {
		return nil, errors.New("invalid RSA hex modulus")
	}
	if len(key)%2 == 1 {
		key = "0" + key
	}
	modulus, err := hex.DecodeString(key)
	if err != nil {
		return nil, err
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: 65537}
	if pub.N.Sign() <= 0 {
		return nil, errors.New("invalid RSA modulus")
	}
	return rsa.EncryptPKCS1v15(rand.Reader, pub, data)
}
