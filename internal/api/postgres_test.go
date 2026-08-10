package api

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

func pgFrame(typ byte, body ...[]byte) []byte {
	var payload []byte
	for _, part := range body {
		payload = append(payload, part...)
	}
	out := append([]byte{typ}, make([]byte, 4)...)
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)+4))
	return append(out, payload...)
}

func z(s string) []byte { return append([]byte(s), 0) }

func TestASessionIsReadBackAsItsMessages(t *testing.T) {
	call := &store.Call{
		Protocol: config.ProtocolPostgres,
		Request:  store.Message{Body: pgFrame('Q', z("SELECT id FROM orders"))},
		Response: store.Message{Body: append(
			pgFrame('C', z("SELECT 12")), pgFrame('Z', []byte{'I'})...)},
	}

	view := buildPostgresView(call)
	if len(view.Sent) != 1 || view.Sent[0].SQL != "SELECT id FROM orders" {
		t.Fatalf("sent = %+v", view.Sent)
	}
	if len(view.Received) != 2 || view.Received[0].Tag != "SELECT 12" {
		t.Fatalf("received = %+v", view.Received)
	}
	if view.SentIncomplete || view.ReceivedIncomplete {
		t.Error("a whole session was reported as cut short")
	}
}

// A capture cut by the body cap leaves a partial message. Reporting it as a
// clean read would hide a gap the tool already knows about.
func TestASessionCutShortSaysSo(t *testing.T) {
	whole := pgFrame('Q', z("SELECT id FROM orders"))
	view := buildPostgresView(&store.Call{
		Protocol: config.ProtocolPostgres,
		Request:  store.Message{Body: whole[:len(whole)-4]},
	})
	if !view.SentIncomplete {
		t.Error("a stream cut mid-message was not reported as incomplete")
	}
}

// The decoder holds the credential bodies back, and the view must not put them
// back in through the JSON it serialises. Payload and the raw value bytes are
// json:"-" in pgwire for exactly this reason.
func TestTheViewNeverSerialisesARawPayload(t *testing.T) {
	view := buildPostgresView(&store.Call{
		Protocol: config.ProtocolPostgres,
		Request:  store.Message{Body: pgFrame('p', z("would-be-a-password"))},
	})

	out, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "would-be-a-password") {
		t.Errorf("the view serialised a raw message body: %s", out)
	}
	// And it still says an authentication happened, which is the fact a reader
	// is entitled to.
	if len(view.Sent) != 1 || view.Sent[0].Kind != "authentication_response" {
		t.Errorf("sent = %+v", view.Sent)
	}
}
