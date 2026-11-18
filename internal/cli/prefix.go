package cli

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// providerPrefixWriter labels each line a provider stage writes to the shared
// console, so concurrent providers in a `both` run stay distinguishable. A line
// split across writes is buffered until its newline arrives.
type providerPrefixWriter struct {
	mu     sync.Mutex
	prefix string
	out    io.Writer
	buf    []byte
}

// Write prefixes every completed line in p and forwards it to the shared console.
func (w *providerPrefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out == nil {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for {
		index := bytes.IndexByte(w.buf, '\n')
		if index < 0 {
			break
		}
		if _, err := fmt.Fprintf(w.out, "%s%s", w.prefix, w.buf[:index+1]); err != nil {
			return 0, err
		}
		w.buf = w.buf[index+1:]
	}
	return len(p), nil
}
