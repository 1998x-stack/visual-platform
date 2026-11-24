package minio

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestNewReaderRequiresIndependentPrivateCredentials(t *testing.T) {
	cases := []ReaderConfig{
		{},
		{Endpoint: "http://localhost:9000", AccessKey: "a", SecretKey: "b", Bucket: "quarantine", Secure: true},
		{Endpoint: "localhost:9000", AccessKey: "", SecretKey: "b", Bucket: "quarantine", Secure: true},
		{Endpoint: "localhost:9000", AccessKey: "a", SecretKey: "b", Bucket: "quarantine", Secure: false},
	}
	for _, cfg := range cases {
		if _, err := NewReader(cfg); err == nil {
			t.Fatalf("NewReader(%+v) should reject insecure/incomplete config", cfg)
		}
	}
	_, err := NewReader(ReaderConfig{
		Endpoint: "localhost:9000", AccessKey: "readonly", SecretKey: "secret",
		Bucket: "private-quarantine", Secure: false, AllowInsecureDevelopment: true,
	})
	if err != nil {
		t.Fatalf("isolated development configuration unexpectedly failed: %v", err)
	}
}

func TestExactSizeReadCloser(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int64
		ok   bool
	}{
		{name: "matching", data: "abcdef", want: 6, ok: true},
		{name: "truncated", data: "abc", want: 6},
		{name: "oversize", data: "abcdefg", want: 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := &exactSizeReadCloser{
				source:   io.NopCloser(strings.NewReader(tc.data)),
				expected: tc.want,
			}
			actual, err := io.ReadAll(reader)
			if tc.ok && (err != nil || !bytes.Equal(actual, []byte(tc.data))) {
				t.Fatalf("wanted exact read, got %q, %v", actual, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("mismatched object stream was accepted: %q", actual)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
