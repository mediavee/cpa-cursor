package cursor

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// Connect streaming envelope: [flags u8][length u32 BE][payload].
const (
	flagCompressed byte = 0x01
	flagEndStream  byte = 0x02

	// compressMinBytes is connect-es' default threshold for outgoing gzip.
	compressMinBytes = 1024
	// maxMessageBytes bounds one frame, compressed or not, so a hostile or
	// corrupt peer cannot make us allocate without limit.
	maxMessageBytes = 64 << 20
)

var errFrameTooLarge = errors.New("cursor: connect frame exceeds size limit")

// marshalFrame encodes m as one Connect message frame, gzip-compressed when
// it is larger than compressMinBytes.
func marshalFrame(m proto.Message) ([]byte, error) {
	payload, err := proto.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("cursor: marshal %T: %w", m, err)
	}
	flags := byte(0)
	if len(payload) > compressMinBytes {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			return nil, fmt.Errorf("cursor: gzip: %w", err)
		}
		if err := zw.Close(); err != nil {
			return nil, fmt.Errorf("cursor: gzip: %w", err)
		}
		payload, flags = buf.Bytes(), flagCompressed
	}
	return encodeFrame(flags, payload), nil
}

func encodeFrame(flags byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// frameReader reads Connect frames and gunzips compressed ones.
type frameReader struct {
	r io.Reader
	// max bounds one frame; maxMessageBytes when zero.
	max int
}

func (f frameReader) limit() int {
	if f.max > 0 {
		return f.max
	}
	return maxMessageBytes
}

// read returns the frame flags with flagCompressed cleared and the
// decompressed payload. It returns io.EOF only on a clean frame boundary.
func (f frameReader) read() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if int64(n) > int64(f.limit()) {
		return 0, nil, errFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(f.r, payload); err != nil {
		return 0, nil, unexpectedEOF(err)
	}
	flags := hdr[0]
	if flags&flagCompressed == 0 {
		return flags, payload, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("cursor: gunzip frame: %w", err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(io.LimitReader(zr, int64(f.limit())+1))
	if err != nil {
		return 0, nil, fmt.Errorf("cursor: gunzip frame: %w", err)
	}
	if len(plain) > f.limit() {
		return 0, nil, errFrameTooLarge
	}
	return flags &^ flagCompressed, plain, nil
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
