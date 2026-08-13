package protocol

import (
	"encoding/json"
	"testing"
)

func TestEnvelopeRoundTripAndUnknownFields(t *testing.T) {
	frame, err := NewEnvelope(DaemonRegister, DaemonRegisterPayload{DaemonID: "daemon-1"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"unknown":"ignored"}`)...)
	var decoded Envelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	var payload DaemonRegisterPayload
	if err := DecodePayload(decoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DaemonID != "daemon-1" {
		t.Fatalf("unexpected daemon id %q", payload.DaemonID)
	}
}

func TestValidateTypeRejectsUnknown(t *testing.T) {
	if err := ValidateType("unknown"); err == nil {
		t.Fatal("expected unknown type error")
	}
	if err := ValidateType(EventBatch); err != nil {
		t.Fatal(err)
	}
}
