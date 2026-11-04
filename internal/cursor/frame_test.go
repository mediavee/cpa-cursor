package cursor

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

func TestFrameLayout(t *testing.T) {
	got := encodeFrame(0x02, []byte("abc"))
	want := []byte{0x02, 0, 0, 0, 3, 'a', 'b', 'c'}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame = %v, want %v", got, want)
	}
}

func TestMarshalFrameCompressesOnlyAboveOneKiB(t *testing.T) {
	msg := func(n int) *agentv1.AgentClientMessage {
		return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: runRequest(strings.Repeat("a", n))}}
	}
	for _, tc := range []struct {
		textLen int
		gz      bool
	}{{100, false}, {900, false}, {2000, true}} {
		f, err := marshalFrame(msg(tc.textLen))
		if err != nil {
			t.Fatal(err)
		}
		if gz := f[0]&flagCompressed != 0; gz != tc.gz {
			t.Errorf("text of %d bytes: compressed=%v, want %v", tc.textLen, gz, tc.gz)
		}
		flags, payload, err := frameReader{r: bytes.NewReader(f)}.read()
		if err != nil || flags != 0 || len(payload) < tc.textLen {
			t.Errorf("round trip: flags=%d len=%d err=%v", flags, len(payload), err)
		}
	}
}

func TestFrameReaderEndAndTruncation(t *testing.T) {
	r := frameReader{r: bytes.NewReader(encodeFrame(flagEndStream, []byte(`{}`)))}
	flags, payload, err := r.read()
	if err != nil || flags != flagEndStream || string(payload) != `{}` {
		t.Fatalf("flags=%d payload=%q err=%v", flags, payload, err)
	}
	if _, _, err := r.read(); !errors.Is(err, io.EOF) {
		t.Errorf("clean boundary: err = %v, want io.EOF", err)
	}
	cut := encodeFrame(0, []byte("abcdef"))[:7]
	if _, _, err := (frameReader{r: bytes.NewReader(cut)}).read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated payload: err = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, _, err := (frameReader{r: bytes.NewReader([]byte{0, 0})}).read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated header: err = %v", err)
	}
}

func TestFrameReaderRejectsOversizeAndBombs(t *testing.T) {
	hdr := make([]byte, 5)
	const limit = 1 << 10
	binary.BigEndian.PutUint32(hdr[1:], limit+1)
	if _, _, err := (frameReader{r: bytes.NewReader(hdr), max: limit}).read(); !errors.Is(err, errFrameTooLarge) {
		t.Errorf("oversize header: err = %v", err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(make([]byte, limit+1))
	_ = zw.Close()
	if _, _, err := (frameReader{r: bytes.NewReader(encodeFrame(flagCompressed, buf.Bytes())), max: limit}).read(); !errors.Is(err, errFrameTooLarge) {
		t.Errorf("gzip bomb: err = %v", err)
	}
	if _, _, err := (frameReader{r: bytes.NewReader(encodeFrame(flagCompressed, []byte("not gzip")))}).read(); err == nil {
		t.Error("garbage marked compressed was accepted")
	}
}
