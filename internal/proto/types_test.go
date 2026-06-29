package proto

import "testing"

func TestNewFrame(t *testing.T) {
	f, err := NewFrame(FrameHeartbeat, HeartbeatPayload{UptimeSeconds: 7})
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != FrameHeartbeat {
		t.Fatalf("type = %s", f.Type)
	}
	if f.ID == "" {
		t.Fatal("empty id")
	}
	if len(f.Payload) == 0 {
		t.Fatal("empty payload")
	}
}

func TestRequiredFrameTypes(t *testing.T) {
	required := []FrameType{FrameHello, FrameAuth, FrameSessionOpen, FrameSessionClose, FrameCmdInput, FrameInterrupt, FrameOutputChunk, FrameAlive, FrameFileUpload, FrameFileDownload, FrameFileAck, FrameHeartbeat, FrameAck}
	for _, ft := range required {
		if ft == "" {
			t.Fatal("empty frame type")
		}
	}
}
