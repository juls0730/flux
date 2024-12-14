package models

import (
	"fmt"
	"os"
	"sync"
)

type CustomSpinnerWriter struct {
	currentSpinnerMsg string
	lock              sync.Mutex
}

func NewCustomSpinnerWriter() *CustomSpinnerWriter {
	return &CustomSpinnerWriter{
		currentSpinnerMsg: "",
		lock:              sync.Mutex{},
	}
}

func (w *CustomSpinnerWriter) Write(p []byte) (n int, err error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	n, err = os.Stdout.Write(p)
	if err != nil {
		return n, err
	}

	w.currentSpinnerMsg = string(p)

	return len(p), nil
}

type CustomStdout struct {
	spinner *CustomSpinnerWriter
	lock    sync.Mutex
}

func NewCustomStdout(spinner *CustomSpinnerWriter) *CustomStdout {
	return &CustomStdout{
		spinner: spinner,
		lock:    sync.Mutex{},
	}
}

func (w *CustomStdout) Write(p []byte) (n int, err error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	n, err = os.Stdout.Write([]byte(fmt.Sprintf("\033[2K\r%s", p)))
	if err != nil {
		return n, err
	}

	nn, err := os.Stdout.Write([]byte(w.spinner.currentSpinnerMsg))
	if err != nil {
		return n, err
	}

	n = nn + n

	return n, nil
}

func (w *CustomStdout) Printf(format string, a ...interface{}) (n int, err error) {
	str := fmt.Sprintf(format, a...)
	return w.Write([]byte(str))
}
