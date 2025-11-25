//go:build !windows

package credstore

// protectFile is the identity outside Windows: the file is 0600 under a
// 0700 directory.
func protectFile(data []byte) ([]byte, error) { return data, nil }

func unprotectFile(raw []byte) ([]byte, error) { return raw, nil }
