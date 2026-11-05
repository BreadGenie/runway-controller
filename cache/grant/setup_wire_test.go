package grant

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func wireRequest(t *testing.T) []byte {
	t.Helper()
	request := SetupRequest{Version: 2, Nonce: "8427b75b-75ae-48eb-839a-a90528f15511"}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func TestSetupWireRespondsWithoutGuestHalfClose(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	deadline := time.Now().Add(time.Second)
	host.SetDeadline(deadline)
	guest.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() {
		request, err := ReadSetup(host)
		if err == nil {
			err = WriteSetupResponse(host, request, true)
		}
		done <- err
	}()
	if _, err := guest.Write(wireRequest(t)); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.NewDecoder(guest).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(response) != 4 || response["route"] != "http://127.0.0.1:18080/" || response["outcome"] != "enabled" || response["version"] != float64(2) {
		t.Fatal("response contract changed")
	}
}

func TestSetupWireRefusesExtraOrUnframedRequests(t *testing.T) {
	valid := wireRequest(t)
	for _, input := range [][]byte{valid[:len(valid)-1], append(append([]byte{}, valid...), valid...), []byte(strings.Repeat(" ", 32768) + "\n"), []byte("{}\n")} {
		if _, err := ReadSetup(bytes.NewReader(input)); err == nil {
			t.Fatal("invalid setup frame accepted")
		}
	}
	request, err := ReadSetup(bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := WriteSetupResponse(&output, request, false); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["outcome"] != "disabled" || response["route"] != nil {
		t.Fatal("disabled setup selected a route")
	}
}
