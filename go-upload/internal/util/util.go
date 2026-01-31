package util

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func TimestampLog(msg string) string {
	return fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
}

func JoinArgs(args []string) string {
	return strings.Join(args, " ")
}

func StreamLines(r io.Reader, fn func(string)) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line+"\n")
		fn(line)
	}
	return lines, scanner.Err()
}

func WriteLines(path string, lines []string) {
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644)
}

func Errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
