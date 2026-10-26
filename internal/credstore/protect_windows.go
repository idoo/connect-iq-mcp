//go:build windows

package credstore

import (
	"bytes"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiMagic prefixes a DPAPI-protected credentials file. Files without it
// are plaintext JSON written by older versions.
var dpapiMagic = []byte("DPAPI1\n")

// protectFile encrypts data for the current Windows user (CryptProtectData).
func protectFile(data []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(data))}
	if len(data) > 0 {
		in.Data = &data[0]
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("DPAPI protect: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	enc := unsafe.Slice(out.Data, out.Size)
	return append(append([]byte{}, dpapiMagic...), enc...), nil
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
	in := windows.DataBlob{Size: uint32(len(enc)), Data: &enc[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("DPAPI unprotect: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte{}, unsafe.Slice(out.Data, out.Size)...), nil
}
