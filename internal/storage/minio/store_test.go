package minio

import "testing"

func TestValidObjectKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"quarantine/00000000-0000-4000-8000-000000000001/00000000-0000-4000-8000-000000000002/00000000-0000-4000-8000-000000000003/source", true},
		{"published/workspace/artifact/source", false},
		{"quarantine/../published/source", false},
		{"quarantine//artifact/source", false},
		{"quarantine/./artifact/source", false},
		{"quarantine/\\\\host\\share", false},
		{"https://example.com/source", false},
		{"quarantine/a/../../b", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := validObjectKey(tc.key); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
