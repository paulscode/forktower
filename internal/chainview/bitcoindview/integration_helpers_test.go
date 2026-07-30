//go:build integration

package bitcoindview

import (
	"bytes"
	"encoding/hex"
	"io"
)

func hexToBytes(s string) ([]byte, error) { return hex.DecodeString(s) }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
