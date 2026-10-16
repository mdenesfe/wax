package media

import (
	"bytes"
	"testing"
)

var benchFile = bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18) // 1 MiB

func BenchmarkEncrypt1MiB(b *testing.B) {
	b.SetBytes(int64(len(benchFile)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Encrypt(benchFile, Image); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecrypt1MiB(b *testing.B) {
	enc, err := Encrypt(benchFile, Image)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(benchFile)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Decrypt(enc.Data, enc.MediaKey, Image, enc.FileEncSHA256, enc.FileSHA256); err != nil {
			b.Fatal(err)
		}
	}
}
