package lib

// all function names need to start with a capital letter to be exported

import "os"
import "strings"

import "github.com/knadh/koanf"
import "github.com/knadh/koanf/parsers/yaml"
import "github.com/knadh/koanf/providers/file"

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

func LoadConfig (component string) (*koanf.Koanf) {
	var k = koanf.New(".")

	// Defaults (for all components)
	k.Set("database.type", "sqlite3")
	k.Set("database.url", "rssmix.sql")

	// Component specific defaults
	switch component {
	case "api":
		k.Set("listen.family", "tcp4")
		k.Set("listen.address", "127.0.0.1:8888")
		k.Set("id.length", 10)
		k.Set("public.protocol", "https")
		k.Set("public.hostname", "localhost")
		k.Set("public.subdirs", 0)
	case "compiler":
		// none
	case "fetcher":
		k.Set("interval", 10)
		k.Set("workdir", os.Getenv("HOME"))
		k.Set("subdirs", 0)
	case "publisher":
		// none
	}

	// Try config files
	for _, configfile := range []string{ "./"+component+".yaml",
                                             os.Getenv("HOME")+"/etc/rssmix/"+component+".yaml",
					     "/etc/rssmix/"+component+".yaml" } {

		loaderr := k.Load(file.Provider(configfile), yaml.Parser())
		if loaderr == nil {
			k.Set("configfile", configfile)
			return k
		}
  	}

	return k, "<none>"
}
