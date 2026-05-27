package cfrelaytun

import (
	"encoding/binary"
	"encoding/json"
)

// ctlFrame is the JSON envelope on the control channel. Binary frames carry
// stream bodies and start with a 4-byte big-endian stream id.
type ctlFrame struct {
	T       string     `json:"t"`
	ID      uint32     `json:"id,omitempty"`
	Method  string     `json:"method,omitempty"`
	Path    string     `json:"path,omitempty"`
	Headers [][]string `json:"headers,omitempty"`
	HasBody bool       `json:"hasBody,omitempty"`
	Status  int        `json:"status,omitempty"`
	Stream  bool       `json:"streaming,omitempty"`
	Data    string     `json:"data,omitempty"`
	Code    int        `json:"code,omitempty"`
	Reason  string     `json:"reason,omitempty"`
}

// frameType peeks at the `t` field without unmarshaling the rest. Used to
// route unknown extension frames to user-registered handlers.
type frameType struct {
	T string `json:"t"`
}

func encodeBinary(id uint32, data []byte) []byte {
	out := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(out[:4], id)
	copy(out[4:], data)
	return out
}

func decodeBinary(data []byte) (uint32, []byte, bool) {
	if len(data) < 4 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint32(data[:4]), data[4:], true
}

// peekType returns just the `t` field from a control frame JSON.
func peekType(raw []byte) string {
	var ft frameType
	if err := json.Unmarshal(raw, &ft); err != nil {
		return ""
	}
	return ft.T
}
