// Package netease implements the small subset of the NetEase Cloud Music
// web API that an AutoDJ needs: QR login, playlists, song metadata and
// playback URLs (exhigh preferred, lower levels accepted when necessary).
//
// The request signing below is written from the public eapi protocol
// description. No code from GPL-licensed projects is used.
package netease

import (
	"crypto/aes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
)

// eapiKey is the fixed AES key the NetEase mobile client protocol uses to
// wrap request parameters. It is a protocol constant, not a secret.
const eapiKey = "e82ckenh8dichen8"

// eapiMarker is the separator embedded in the signed plaintext.
const eapiMarker = "36cd479b6b5"

// eapiParams builds the form value for one eapi request:
//
//	plain = path + "-" + marker + "-" + json + "-" + marker + "-" +
//	        md5("nobody" + path + "use" + json + "md5forencrypt")
//	params = hex(aes-128-ecb-pkcs7(plain, eapiKey))
func eapiParams(path string, payload []byte) string {
	h := md5.New()
	h.Write([]byte("nobody"))
	h.Write([]byte(path))
	h.Write([]byte("use"))
	h.Write(payload)
	h.Write([]byte("md5forencrypt"))
	digest := hex.EncodeToString(h.Sum(nil))

	plain := make([]byte, 0, len(path)+len(payload)+len(digest)+3*len(eapiMarker))
	plain = append(plain, path...)
	plain = append(plain, '-')
	plain = append(plain, eapiMarker...)
	plain = append(plain, '-')
	plain = append(plain, payload...)
	plain = append(plain, '-')
	plain = append(plain, eapiMarker...)
	plain = append(plain, '-')
	plain = append(plain, digest...)

	return "params=" + strings.ToUpper(hex.EncodeToString(eapiEncrypt(plain)))
}

func eapiEncrypt(plain []byte) []byte {
	block, err := aes.NewCipher([]byte(eapiKey))
	if err != nil {
		// The key is a compile-time constant with a valid AES size.
		panic(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, len(padded))
	for off := 0; off < len(padded); off += aes.BlockSize {
		block.Encrypt(out[off:off+aes.BlockSize], padded[off:off+aes.BlockSize])
	}
	return out
}

// eapiDecrypt is used only when the server answers with an encrypted body.
// Most of the endpoints this package calls return plain JSON.
func eapiDecrypt(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("eapi: encrypted body length %d is not a multiple of %d", len(data), aes.BlockSize)
	}
	block, err := aes.NewCipher([]byte(eapiKey))
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	for off := 0; off < len(data); off += aes.BlockSize {
		block.Decrypt(out[off:off+aes.BlockSize], data[off:off+aes.BlockSize])
	}
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return nil, fmt.Errorf("eapi: invalid PKCS#7 padding %d", pad)
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("eapi: inconsistent PKCS#7 padding")
		}
	}
	return out[:len(out)-pad], nil
}
