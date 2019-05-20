package file

import (
	"bufio"
	"io"
	"os"
	"strings"
)

func IsExist(filename string) bool {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return false
	}
	return true
}

func ReadLines(filename string) ([]string, error) {
	f, err := os.Open(filename)
	defer f.Close()
	if err != nil {
		return []string{""}, err
	}
	var ret []string
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF {
			break
		}
		ret = append(ret, strings.Trim(line, "\n"))
	}
	return ret, nil
}
