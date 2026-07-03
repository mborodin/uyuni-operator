// Package cobbler is a minimal client for Cobbler's XMLRPC API, exposed by
// Uyuni at {server}/cobbler_api. We hand-roll a tiny XMLRPC codec rather than
// pull in a new module: the call set is small (login + get_*/find_* reads and
// new/modify/save/remove writes), and the build has no local Go toolchain to
// regenerate go.sum for an added dependency.
package cobbler

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// encodeRequest renders an XMLRPC methodCall. Supported arg types: string, bool,
// int, float64, map[string]string, map[string]any, []string, []any.
func encodeRequest(method string, args ...any) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><methodCall><methodName>`)
	_ = xml.EscapeText(&b, []byte(method))
	b.WriteString(`</methodName><params>`)
	for _, a := range args {
		b.WriteString("<param>")
		if err := encodeValue(&b, a); err != nil {
			return nil, err
		}
		b.WriteString("</param>")
	}
	b.WriteString("</params></methodCall>")
	return []byte(b.String()), nil
}

func encodeValue(b *strings.Builder, v any) error {
	b.WriteString("<value>")
	defer b.WriteString("</value>")
	switch val := v.(type) {
	case string:
		b.WriteString("<string>")
		_ = xml.EscapeText(b, []byte(val))
		b.WriteString("</string>")
	case bool:
		if val {
			b.WriteString("<boolean>1</boolean>")
		} else {
			b.WriteString("<boolean>0</boolean>")
		}
	case int:
		fmt.Fprintf(b, "<int>%d</int>", val)
	case float64:
		fmt.Fprintf(b, "<double>%g</double>", val)
	case map[string]string:
		b.WriteString("<struct>")
		for k, mv := range val {
			b.WriteString("<member><name>")
			_ = xml.EscapeText(b, []byte(k))
			b.WriteString("</name>")
			if err := encodeValue(b, mv); err != nil {
				return err
			}
			b.WriteString("</member>")
		}
		b.WriteString("</struct>")
	case map[string]any:
		b.WriteString("<struct>")
		for k, mv := range val {
			b.WriteString("<member><name>")
			_ = xml.EscapeText(b, []byte(k))
			b.WriteString("</name>")
			if err := encodeValue(b, mv); err != nil {
				return err
			}
			b.WriteString("</member>")
		}
		b.WriteString("</struct>")
	case []string:
		b.WriteString("<array><data>")
		for _, av := range val {
			if err := encodeValue(b, av); err != nil {
				return err
			}
		}
		b.WriteString("</data></array>")
	case []any:
		b.WriteString("<array><data>")
		for _, av := range val {
			if err := encodeValue(b, av); err != nil {
				return err
			}
		}
		b.WriteString("</data></array>")
	default:
		return fmt.Errorf("cobbler xmlrpc: unsupported argument type %T", v)
	}
	return nil
}

// FaultError is a Cobbler XMLRPC fault response.
type FaultError struct {
	Code    int
	Message string
}

func (e *FaultError) Error() string {
	return fmt.Sprintf("cobbler fault %d: %s", e.Code, e.Message)
}

// decodeResponse parses an XMLRPC methodResponse and returns the single return
// value, or a *FaultError for a fault.
func decodeResponse(data []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	fault := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("cobbler xmlrpc: no value in response")
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "fault":
			fault = true
		case "value":
			val, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			if fault {
				m, _ := val.(map[string]any)
				code, _ := m["faultCode"].(int)
				msg, _ := m["faultString"].(string)
				return nil, &FaultError{Code: code, Message: msg}
			}
			return val, nil
		}
	}
}

// decodeValue reads a value body; the <value> start element has just been
// consumed. It consumes through the matching </value>.
func decodeValue(dec *xml.Decoder) (any, error) {
	var chardata strings.Builder
	var result any
	typed := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if !typed {
				chardata.Write(t)
			}
		case xml.StartElement:
			typed = true
			var v any
			switch t.Name.Local {
			case "string":
				v, err = readText(dec)
			case "int", "i4":
				var s string
				if s, err = readText(dec); err == nil {
					v, err = strconv.Atoi(strings.TrimSpace(s))
				}
			case "boolean":
				var s string
				s, err = readText(dec)
				v = strings.TrimSpace(s) == "1"
			case "double":
				var s string
				if s, err = readText(dec); err == nil {
					v, err = strconv.ParseFloat(strings.TrimSpace(s), 64)
				}
			case "struct":
				v, err = decodeStruct(dec)
			case "array":
				v, err = decodeArray(dec)
			case "nil":
				err = skipTo(dec, "nil")
				v = nil
			default:
				err = skipTo(dec, t.Name.Local)
				v = nil
			}
			if err != nil {
				return nil, err
			}
			result = v
		case xml.EndElement:
			if t.Name.Local == "value" {
				if typed {
					return result, nil
				}
				return chardata.String(), nil
			}
		}
	}
}

// readText returns the character data up to the matching end element (which has
// just been opened).
func readText(dec *xml.Decoder) (string, error) {
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			return sb.String(), nil
		}
	}
}

func decodeStruct(dec *xml.Decoder) (map[string]any, error) {
	m := map[string]any{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "member" {
				k, v, err := decodeMember(dec)
				if err != nil {
					return nil, err
				}
				m[k] = v
			}
		case xml.EndElement:
			if t.Name.Local == "struct" {
				return m, nil
			}
		}
	}
}

func decodeMember(dec *xml.Decoder) (string, any, error) {
	var name string
	var val any
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "name":
				if name, err = readText(dec); err != nil {
					return "", nil, err
				}
			case "value":
				if val, err = decodeValue(dec); err != nil {
					return "", nil, err
				}
			}
		case xml.EndElement:
			if t.Name.Local == "member" {
				return name, val, nil
			}
		}
	}
}

func decodeArray(dec *xml.Decoder) ([]any, error) {
	var arr []any
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "value" {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				return arr, nil
			}
		}
	}
}

// skipTo consumes tokens until the end of the named element.
func skipTo(dec *xml.Decoder, name string) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if e, ok := tok.(xml.EndElement); ok && e.Name.Local == name {
			return nil
		}
	}
}
