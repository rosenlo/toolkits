package str

import "bytes"

func Concatenate(cont ...string) string {
	var buffer bytes.Buffer
	for _, c := range cont {
		buffer.WriteString(c)
	}
	return buffer.String()
}
