// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxHeader bounds one frame header of the content and status classes;
// bodies follow a header and are bounded by the size it declares.
const maxHeader = 64 << 10

// errFrame reports a malformed frame.
var errFrame = errors.New("controlstore: malformed 8092 frame")

// writeHeader writes one length-prefixed JSON header.
func writeHeader(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("controlstore: encode frame: %w", err)
	}
	if len(b) > maxHeader {
		return fmt.Errorf("%w: header of %d bytes", errFrame, len(b))
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b))) //nolint:gosec // G115: at most maxHeader.
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// readHeader reads one length-prefixed JSON header into v, refusing
// unknown fields.
func readHeader(r io.Reader, v any) error {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > maxHeader {
		return fmt.Errorf("%w: header of %d bytes", errFrame, size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", errFrame, err)
	}
	return nil
}
