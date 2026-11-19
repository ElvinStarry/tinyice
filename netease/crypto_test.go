package netease

import "testing"

// Vector generated independently with OpenSSL:
//
//	printf '/api/test-36cd479b6b5-{"a":1}-36cd479b6b5-<md5>' |
//	openssl enc -aes-128-ecb -K 653832636b656e683864696368656e38
func TestEapiParamsMatchesProtocol(t *testing.T) {
	const want = "params=4DC723619A991588865191FD2F319BAD0918BC9C604E1E84A5C3578922E3A7E8810405B5500AF5BEABA2DEAB687471586CE47DE62C9D523E260A0250C7F3AC2802B572BD7B95623F10A1D55EF99B9A8C"
	got := eapiParams("/api/test", []byte(`{"a":1}`))
	if got != want {
		t.Fatalf("eapiParams mismatch\n got %s\nwant %s", got, want)
	}
}

func TestEapiDecryptRoundTrip(t *testing.T) {
	plain := []byte(`{"code":200,"data":"ok"}`)
	enc := eapiEncrypt(plain)
	got, err := eapiDecrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatalf("round trip = %q, want %q", got, plain)
	}
}
