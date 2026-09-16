//go:build windows

package credstore

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiMagic prefixes a DPAPI-protected credentials file. Files without it
// are plaintext JSON written by older versions.
var dpapiMagic = []byte("DPAPI1\n")

// protectFile encrypts data for the current Windows user (CryptProtectData).
func protectFile(data []byte) ([]byte, error) {
	in, err := dataBlob(data)
	if err != nil {
		return nil, err
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("DPAPI protect: %w", err)
	}
	return append(append([]byte{}, dpapiMagic...), takeBlob(out)...), nil
}

// unprotectFile decrypts a protectFile result; legacy plaintext is returned
// unchanged.
func unprotectFile(raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, dpapiMagic) {
		return raw, nil
	}
	enc := raw[len(dpapiMagic):]
	if len(enc) == 0 {
		return nil, errors.New("DPAPI unprotect: empty data")
	}
	in, err := dataBlob(enc)
	if err != nil {
		return nil, err
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("DPAPI unprotect: %w", err)
	}
	return takeBlob(out), nil
}

// dataBlob points a DataBlob at b without copying.
func dataBlob(b []byte) (windows.DataBlob, error) {
	if len(b) > math.MaxUint32 {
		return windows.DataBlob{}, errors.New("DPAPI: data too large")
	}
	in := windows.DataBlob{Size: uint32(len(b))} //nolint:gosec // G115: bounded above
	if len(b) > 0 {
		in.Data = &b[0]
	}
	return in, nil
}

// takeBlob copies a DPAPI output blob into Go memory and frees it.
func takeBlob(out windows.DataBlob) []byte {
	//nolint:gosec // G103: DPAPI returns LocalAlloc'd memory; this is the only way to read and free it
	b := append([]byte{}, unsafe.Slice(out.Data, out.Size)...)
	_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) //nolint:gosec // G103: see above
	return b
}
