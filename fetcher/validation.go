package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html/charset"
)

const feedValidationVersion = 1
const maxXMLDepth = 256

// Validate the complete response, not only its first element. gofeed remains
// the final compatibility check because the compiler uses the same parser.
// This is a structural check, not a full RSS/Atom XSD or DTD validator.
func validateFeedFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFeedSize+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxFeedSize {
		return fmt.Errorf("feed exceeds %d bytes", maxFeedSize)
	}
	return validateFeed(data)
}

func validateFeed(data []byte) (validationErr error) {
	// Parsing an untrusted response must not terminate the fetcher if the
	// third-party parser encounters an unexpected nil value or other panic.
	defer func() {
		if recovered := recover(); recovered != nil {
			validationErr = fmt.Errorf("feed parser failed: %v", recovered)
		}
	}()

	clean := bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	trimmed := bytes.TrimSpace(clean)
	if len(trimmed) == 0 {
		return fmt.Errorf("empty feed response")
	}
	if trimmed[0] == '{' {
		if err := validateJSONFeed(trimmed); err != nil {
			return err
		}
	} else {
		if err := validateXMLFeed(clean); err != nil {
			return err
		}
	}
	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("cannot parse feed: %w", err)
	}
	if parsed == nil {
		return fmt.Errorf("parser returned no feed")
	}
	for i, item := range parsed.Items {
		if item == nil {
			return fmt.Errorf("feed item %d is nil", i+1)
		}
		for _, author := range item.Authors {
			if author == nil {
				return fmt.Errorf("feed item %d contains a nil author", i+1)
			}
		}
		for _, enclosure := range item.Enclosures {
			if enclosure == nil {
				return fmt.Errorf("feed item %d contains a nil enclosure", i+1)
			}
		}
	}
	return nil
}

func validateJSONFeed(data []byte) error {
	var header struct {
		Version string             `json:"version"`
		Title   string             `json:"title"`
		Items   *[]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("invalid JSON feed: %w", err)
	}
	if header.Version != "https://jsonfeed.org/version/1" && header.Version != "https://jsonfeed.org/version/1.1" {
		return fmt.Errorf("response is not a supported JSON Feed")
	}
	if header.Title == "" || header.Items == nil {
		return fmt.Errorf("JSON Feed requires a title and an items array")
	}
	for i, item := range *header.Items {
		item = bytes.TrimSpace(item)
		if len(item) == 0 || item[0] != '{' {
			return fmt.Errorf("JSON Feed item %d is not an object", i+1)
		}
	}
	return nil
}

// UTF-8 BOM and XML-declared legacy encodings are handled. For UTF-16, decode
// the BOM/detected byte order before feeding encoding/xml its first token.
func strictXMLDecoder(data []byte) (*xml.Decoder, error) {
	var reader io.Reader = bytes.NewReader(data)
	utf16 := false
	label := ""
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}):
		label = "utf-16le"
		data = data[2:]
	case bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		label = "utf-16be"
		data = data[2:]
	case bytes.HasPrefix(data, []byte{'<', 0}):
		label = "utf-16le"
	case bytes.HasPrefix(data, []byte{0, '<'}):
		label = "utf-16be"
	}
	if label != "" {
		var err error
		reader, err = charset.NewReaderLabel(label, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		utf16 = true
	}
	decoder := xml.NewDecoder(reader)
	decoder.Strict = true
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if utf16 && (strings.EqualFold(label, "utf-16") || strings.EqualFold(label, "utf-16le") || strings.EqualFold(label, "utf-16be")) {
			return input, nil
		}
		return charset.NewReaderLabel(label, input)
	}
	return decoder, nil
}

func validateXMLFeed(data []byte) error {
	decoder, err := strictXMLDecoder(data)
	if err != nil {
		return err
	}
	depth, roots, channels := 0, 0, 0
	kind := ""
	var root xml.Name
	declaration := false
	doctype := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid feed XML: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			attrs := make(map[xml.Name]bool, len(token.Attr))
			for _, attr := range token.Attr {
				if attrs[attr.Name] {
					return fmt.Errorf("duplicate XML attribute %s", attr.Name.Local)
				}
				attrs[attr.Name] = true
			}
			depth++
			if depth > maxXMLDepth {
				return fmt.Errorf("feed XML nesting exceeds %d elements", maxXMLDepth)
			}
			if depth == 1 {
				roots++
				if roots != 1 {
					return fmt.Errorf("feed XML has multiple root elements")
				}
				root = token.Name
				switch {
				case root.Local == "rss" && root.Space == "":
					kind = "rss"
				case root.Local == "RDF" && root.Space == "http://www.w3.org/1999/02/22-rdf-syntax-ns#":
					kind = "rdf"
				case root.Local == "feed" && (root.Space == "http://www.w3.org/2005/Atom" || root.Space == "http://purl.org/atom/ns#"):
					kind = "atom"
				default:
					return fmt.Errorf("XML root {%s}%s is not an RSS/Atom feed", root.Space, root.Local)
				}
			}
			if depth == 2 && token.Name.Local == "channel" {
				if (kind == "rss" && token.Name.Space == "") || (kind == "rdf" && (token.Name.Space == "http://purl.org/rss/1.0/" || token.Name.Space == "http://my.netscape.com/rdf/simple/0.9/")) {
					channels++
				}
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && !xmlSpace(token) {
				return fmt.Errorf("text outside feed XML root")
			}
		case xml.Directive:
			if roots > 0 || doctype || !strings.HasPrefix(strings.TrimSpace(string(token)), "DOCTYPE ") {
				return fmt.Errorf("invalid or misplaced XML directive")
			}
			doctype = true
		case xml.ProcInst:
			if strings.EqualFold(token.Target, "xml") {
				if roots > 0 || declaration {
					return fmt.Errorf("misplaced XML declaration")
				}
				declaration = true
			}
		}
	}
	if roots != 1 || depth != 0 {
		return fmt.Errorf("feed XML has no complete root element")
	}
	if (kind == "rss" || kind == "rdf") && channels != 1 {
		return fmt.Errorf("%s feed must contain exactly one channel element", kind)
	}
	return nil
}
func xmlSpace(data []byte) bool {
	for _, c := range data {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}
