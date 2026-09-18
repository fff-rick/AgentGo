package main

import (
	"strings"
	"testing"
)

func TestReadEvents(t *testing.T) {
	var names, values []string
	err := readEvents(strings.NewReader("event: answer_delta\r\ndata: {\"message\":\"你\"}\r\n\r\nevent: done\ndata: {}\n\n"), func(name string, data []byte) error {
		names = append(names, name)
		values = append(values, string(data))
		return nil
	})
	if err != nil || len(names) != 2 || names[0] != "answer_delta" || names[1] != "done" || values[0] != `{"message":"你"}` {
		t.Fatalf("events=%v data=%v err=%v", names, values, err)
	}
}
