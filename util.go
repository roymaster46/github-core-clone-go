// Small shared helpers for the Go forge.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

func currentUnixSec() int64 {
	return time.Now().Unix()
}

func unixToISO(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func jsonUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return dec.Decode(v)
}

// gqlStr is fmt.Sprint except that a missing (nil) value is "" —
// matching Node's String(x ?? ”) for GraphQL inputs.
func gqlStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if f, ok := v.(float64); ok {
		// JSON numbers in variables: render like JS String(number).
		if f == float64(int64(f)) {
			return itoa(int64(f))
		}
		return ftoa(f)
	}
	return sprintAny(v)
}

func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func sprintAny(v any) string {
	return fmt.Sprint(v)
}

func sortIssuesByNumber(issues []*IssueRecord) {
	for i := 1; i < len(issues); i++ {
		for j := i; j > 0 && issues[j].Number < issues[j-1].Number; j-- {
			issues[j], issues[j-1] = issues[j-1], issues[j]
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
