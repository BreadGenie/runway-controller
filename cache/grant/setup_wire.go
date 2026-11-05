package grant

import (
	"bufio"
	"encoding/json"
	"io"
)

// ReadSetup reads one newline-framed request without waiting for socket EOF.
// The authenticated connection owner must set deadlines and close after one exchange.
func ReadSetup(reader io.Reader) (SetupRequest, error) {
	buffer := bufio.NewReaderSize(reader, 32768)
	line, err := buffer.ReadSlice('\n')
	if err != nil || len(line) > 32768 || buffer.Buffered() != 0 {
		return SetupRequest{}, ErrInvalid
	}
	return DecodeSetup(line[:len(line)-1])
}

// WriteSetupResponse returns only identity and the fixed credential-free relay
// route. Enabled is valid only after the host has installed the job's translator.
func WriteSetupResponse(writer io.Writer, request SetupRequest, enabled bool) error {
	if request.Version != 2 || !canonicalUUID(request.Nonce) {
		return ErrInvalid
	}
	response := struct {
		Version int     `json:"version"`
		Nonce   string  `json:"nonce"`
		Outcome string  `json:"outcome"`
		Route   *string `json:"route"`
	}{Version: 2, Nonce: request.Nonce, Outcome: "disabled"}
	if enabled {
		route := "http://127.0.0.1:18080/"
		response.Outcome, response.Route = "enabled", &route
	}
	return json.NewEncoder(writer).Encode(response)
}
