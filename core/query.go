package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
)

// queryFromJSON flattens a message's JSON into query parameters: a nested
// field by its dotted path, a repeated one as one parameter per element.
func queryFromJSON(encoded []byte, skip []string) (url.Values, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		return nil, fmt.Errorf("origincloud: encoding the request: %w", err)
	}
	query := url.Values{}
	if err := flatten(query, "", fields, skip); err != nil {
		return nil, err
	}
	return query, nil
}

func flatten(query url.Values, prefix string, fields map[string]any, skip []string) error {
	for name, value := range fields {
		path := prefix + name
		if slices.Contains(skip, path) {
			continue
		}
		switch v := value.(type) {
		case map[string]any:
			if err := flatten(query, path+".", v, skip); err != nil {
				return err
			}
		case []any:
			for _, element := range v {
				scalar, err := queryScalar(path, element)
				if err != nil {
					return err
				}
				query.Add(path, scalar)
			}
		default:
			scalar, err := queryScalar(path, v)
			if err != nil {
				return err
			}
			query.Set(path, scalar)
		}
	}
	return nil
}

func queryScalar(path string, value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("origincloud: %s can't be sent as a query parameter", path)
	}
}
