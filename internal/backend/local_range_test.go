package backend

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalGetObjectRange(t *testing.T) {
	root := t.TempDir()
	b, err := newLocalBackend(LocalConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("0123456789abcdefghij") // 20 bytes
	if err := os.MkdirAll(filepath.Join(root, "bkt"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bkt", "obj"), data, 0o640); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		rng       string
		wantBody  string
		wantRange string
		wantLen   int64
		wantErr   bool
	}{
		{rng: "", wantBody: "0123456789abcdefghij", wantRange: "", wantLen: 20},
		{rng: "bytes=0-7", wantBody: "01234567", wantRange: "bytes 0-7/20", wantLen: 8},
		{rng: "bytes=5-9", wantBody: "56789", wantRange: "bytes 5-9/20", wantLen: 5},
		{rng: "bytes=10-", wantBody: "abcdefghij", wantRange: "bytes 10-19/20", wantLen: 10},
		{rng: "bytes=15-999", wantBody: "fghij", wantRange: "bytes 15-19/20", wantLen: 5},
		{rng: "bytes=20-25", wantErr: true},
		{rng: "bytes=garbage", wantErr: true},
	}

	for _, c := range cases {
		out, err := b.GetObject(context.Background(), GetObjectInput{Bucket: "bkt", Key: "obj", Range: c.rng})
		if c.wantErr {
			if err == nil {
				out.Body.Close()
				t.Errorf("range %q: expected error, got none", c.rng)
			} else if !errors.Is(err, ErrInvalidRange) {
				t.Errorf("range %q: error = %v, want ErrInvalidRange", c.rng, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("range %q: unexpected error: %v", c.rng, err)
			continue
		}
		body, _ := io.ReadAll(out.Body)
		out.Body.Close()
		if string(body) != c.wantBody {
			t.Errorf("range %q: body = %q, want %q", c.rng, body, c.wantBody)
		}
		if out.ContentRange != c.wantRange {
			t.Errorf("range %q: ContentRange = %q, want %q", c.rng, out.ContentRange, c.wantRange)
		}
		if out.ContentLength != c.wantLen {
			t.Errorf("range %q: ContentLength = %d, want %d", c.rng, out.ContentLength, c.wantLen)
		}
	}
}
