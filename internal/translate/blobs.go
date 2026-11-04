package translate

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
)

// Blob is content the server reads back through the KV channel. ID is the SHA-256 of Data.
type Blob struct {
	ID   []byte
	Data []byte
}

// blobSet collects blobs in insertion order and drops duplicates.
type blobSet struct {
	blobs []Blob
	seen  map[string]bool
}

func newBlobSet() *blobSet { return &blobSet{seen: map[string]bool{}} }

// add stores data and returns its id.
func (s *blobSet) add(data []byte) []byte {
	sum := sha256.Sum256(data)
	if !s.seen[string(sum[:])] {
		s.seen[string(sum[:])] = true
		s.blobs = append(s.blobs, Blob{ID: sum[:], Data: data})
	}
	return sum[:]
}

// addJSON stores the JSON encoding of v and returns its id.
func (s *blobSet) addJSON(v any) ([]byte, error) {
	data, err := marshalJSON(v)
	if err != nil {
		return nil, err
	}
	return s.add(data), nil
}

// marshalJSON encodes v without HTML escaping, so blob bytes stay readable and stable.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
