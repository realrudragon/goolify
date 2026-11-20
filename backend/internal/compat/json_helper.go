package compat

import "encoding/json"

func jUnmarshal(raw []byte, dst any) error {
	return json.Unmarshal(raw, dst)
}
