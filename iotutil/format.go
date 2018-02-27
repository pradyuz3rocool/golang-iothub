package iotutil

import (
	"bytes"
	"fmt"
	"sort"
	"unicode"
)

// IsPrintable reports whether the given slice
// of bytes can be safely printed to console.
func IsPrintable(b []byte) bool {
	for _, r := range string(b) {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// FormatPayload converts b into string of hex bytes if it's not printable.
func FormatPayload(b []byte) string {
	if IsPrintable(b) {
		return string(b)
	}
	return fmt.Sprintf("% x", string(b))
}

// FormatProperties formats the given map of properties.
func FormatProperties(m map[string]string) string {
	p := 0
	b := &bytes.Buffer{}
	o := make([]string, 0, len(m))
	for k := range m {
		if p < len(k) {
			p = len(k)
		}
		o = append(o, k)
	}
	sort.Strings(o)
	for i, k := range o {
		if i != 0 {
			b.WriteByte('\n')
		}
		b.WriteString(fmt.Sprintf("%-"+fmt.Sprint(p)+"s : %s", k, m[k]))
	}
	return b.String()
}
