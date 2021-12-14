package lib

// all function names need to start with a capital letter to be exported

import "os"
import "strings"

func Value_or_default (value interface{}, def interface{}) (interface{}) {
	switch value.(type) {
	case string:
		if value.(string) != "" { return value.(string) }
		return def.(string)
	case int:
		if value.(int) != 0 { return value.(int) }
		return def.(int)
	}

	return nil
}

func Maxlen (s string, n int) string {
	if len(s) > n {
		return s[:n]
	}

	return s
}

func Subdirs (s string, i int) (string) {
	var result []string
	for n, char := range strings.Split(s, "") {
		if n < i {
			result = append(result, char)
			result = append(result, "/")
		}
	}
	result = append(result, s)

	return strings.Join(result, "")
}

func File_exists (path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func FirstN (s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
